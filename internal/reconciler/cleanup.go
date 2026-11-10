package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/mgreau/zen/internal/config"
	ghpkg "github.com/mgreau/zen/internal/github"
	wt "github.com/mgreau/zen/internal/worktree"
)

// prMergeInfoFunc is the GitHub REST lookup merged-review cleanup needs.
// Tests replace it so cleanup runs without a network.
type prMergeInfoFunc func(ctx context.Context, fullRepo string, prNumber int) (*ghpkg.PRMergeInfo, error)

// prByHeadFunc finds the PR whose head is headOwner:branch (0 when none).
type prByHeadFunc func(ctx context.Context, fullRepo, headOwner, branch string) (int, error)

// CleanupReconciler archives (see worktree.Archive) the worktrees of merged
// PRs: review worktrees once cleanup_after_merge has passed, and work streams
// (feature worktrees) once their PR has also landed, meaning land-watch
// verified every deploy stage of the merge commit.
type CleanupReconciler struct {
	cfg            *config.Config
	prMergeInfo    prMergeInfoFunc
	deployVerified deployVerifiedFunc
}

// NewCleanupReconciler creates a new CleanupReconciler.
func NewCleanupReconciler(cfg *config.Config) *CleanupReconciler {
	return &CleanupReconciler{cfg: cfg}
}

// SetConfig updates the config used by this reconciler.
func (r *CleanupReconciler) SetConfig(cfg *config.Config) {
	r.cfg = cfg
}

// Reconcile processes a single cleanup key: a review key from MakePRKey or a
// work-stream key from MakeFeatureKey.
func (r *CleanupReconciler) Reconcile(ctx context.Context, key string, _ workqueue.Options) error {
	if repo, prNumber, name, err := ParseFeatureKey(key); err == nil {
		return r.reconcileFeature(ctx, repo, prNumber, name)
	}
	repo, prNumber, err := ParsePRKey(key)
	if err != nil {
		return workqueue.NonRetriableError(err, "invalid key format")
	}

	label := fmt.Sprintf("%s PR #%d", repo, prNumber)

	basePath := r.cfg.RepoBasePath(repo)
	if basePath == "" {
		return workqueue.NonRetriableError(
			fmt.Errorf("unknown repo %q", repo),
			"repo not configured",
		)
	}

	worktreePath := wt.PRPath(basePath, repo, prNumber)
	originPath := filepath.Join(basePath, repo)
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		return nil // already removed
	}

	lookup, err := r.lookup(ctx)
	if err != nil {
		return fmt.Errorf("GitHub client: %w", err)
	}
	info, err := lookup(ctx, r.cfg.RepoFullName(repo), prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR merge state: %w", err)
	}

	// The scan that queued this key may be stale, and the daemon only ever
	// removes merged reviews: check again before touching the worktree.
	after := r.cfg.Watch.CleanupAfterMergeDuration()
	if info.State != "MERGED" {
		logf("Cleanup skipped for %s: PR is %s, not merged", label, info.State)
		return nil
	}
	if !mergedFor(info, after, time.Now()) {
		logf("Cleanup skipped for %s: merged less than cleanup_after_merge (%s) ago", label, after)
		return nil
	}

	// Safety refusals (a running agent or not a review checkout) are expected
	// skips; Git and filesystem failures retry. Local changes and commits are
	// kept by the archive.
	archived, err := wt.ArchiveReview(originPath, worktreePath, prNumber)
	if err != nil {
		if wt.RemovalBlocked(err) {
			logf("Cleanup skipped for %s: %v", label, err)
			return nil
		}
		return fmt.Errorf("archive worktree: %w", err)
	}
	logArchived(label, archived)
	return nil
}

// reconcileFeature archives the work stream name once PR prNumber has merged,
// cleanup_after_merge has passed, and land-watch verified its deploy.
func (r *CleanupReconciler) reconcileFeature(ctx context.Context, repo string, prNumber int, name string) error {
	label := fmt.Sprintf("%s PR #%d (%s)", repo, prNumber, name)
	basePath := r.cfg.RepoBasePath(repo)
	if basePath == "" {
		return workqueue.NonRetriableError(fmt.Errorf("unknown repo %q", repo), "repo not configured")
	}
	worktreePath := filepath.Join(basePath, name)
	originPath := filepath.Join(basePath, repo)
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		return nil // already archived
	}

	lookup, err := r.lookup(ctx)
	if err != nil {
		return fmt.Errorf("GitHub client: %w", err)
	}
	fullRepo := r.cfg.RepoFullName(repo)
	info, err := lookup(ctx, fullRepo, prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR merge state: %w", err)
	}
	after := r.cfg.Watch.CleanupAfterMergeDuration()
	if !mergedFor(info, after, time.Now()) {
		logf("Cleanup skipped for %s: not merged at least cleanup_after_merge (%s) ago", label, after)
		return nil
	}

	verify := r.deployVerified
	if verify == nil {
		verify = landWatchVerified
	}
	verified, detail, err := verify(ctx, fullRepo, prNumber)
	if err != nil {
		return fmt.Errorf("verify deploy: %w", err)
	}
	if !verified {
		logf("Cleanup skipped for %s: deploy not verified: %s", label, detail)
		return nil
	}

	archived, err := wt.Archive(originPath, worktreePath)
	if err != nil {
		if wt.RemovalBlocked(err) {
			logf("Cleanup skipped for %s: %v", label, err)
			return nil
		}
		return fmt.Errorf("archive worktree: %w", err)
	}
	logArchived(label, archived)
	return nil
}

func logArchived(label string, a *wt.Archived) {
	if a == nil {
		return
	}
	wip := ""
	if a.WIP {
		wip = " after a WIP commit"
	}
	logf("Archived %s at %s%s (%s)", label, a.Ref, wip, a.Manifest)
}

func (r *CleanupReconciler) lookup(ctx context.Context) (prMergeInfoFunc, error) {
	if r.prMergeInfo != nil {
		return r.prMergeInfo, nil
	}
	client, err := ghpkg.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetPRMergeInfo, nil
}

// mergedFor reports whether info is a merged PR whose mergedAt is at least
// after before now.
func mergedFor(info *ghpkg.PRMergeInfo, after time.Duration, now time.Time) bool {
	return info != nil && info.State == "MERGED" && !info.MergedAt.IsZero() && now.Sub(info.MergedAt) >= after
}

// ScanMergedPRs queues cleanup for worktrees whose PR merged at least
// cleanupAfterMerge ago, by GitHub's mergedAt: PR reviews, and work streams
// whose branch is the head of a PR in one of the worktree's GitHub remotes.
// Closed-unmerged PRs are left alone. Work streams are archived only after
// Reconcile also verifies the deploy.
func ScanMergedPRs(ctx context.Context, cfg *config.Config, queue workqueue.Interface, cleanupAfterMerge time.Duration) {
	ghClient, err := ghpkg.NewClient(ctx)
	if err != nil {
		logf("Error creating GitHub client for cleanup scan: %v", err)
		return
	}
	scanMergedPRs(ctx, cfg, queue, ghClient.GetPRMergeInfo, ghClient.FindPRByHead, cleanupAfterMerge, time.Now())
}

func scanMergedPRs(ctx context.Context, cfg *config.Config, queue workqueue.Interface, lookup prMergeInfoFunc, findPR prByHeadFunc, cleanupAfterMerge time.Duration, now time.Time) {
	wts, err := wt.ListAll(cfg)
	if err != nil {
		logf("Error listing worktrees for cleanup scan: %v", err)
		return
	}

	for _, w := range wts {
		var key string
		prNumber := w.PRNumber
		switch {
		// PR reviews (see worktree.Classify), only at the path Reconcile
		// archives for that PR.
		case w.Type == wt.TypePRReview && w.PRNumber != 0 && w.Name == wt.PRName(w.Repo, w.PRNumber):
			key = MakePRKey(w.Repo, w.PRNumber)
		case w.Type == wt.TypeFeature && w.Branch != "":
			prNumber = featurePR(ctx, cfg.RepoFullName(w.Repo), w, findPR)
			if prNumber == 0 {
				continue
			}
			key = MakeFeatureKey(w.Repo, prNumber, w.Name)
		default:
			continue
		}
		info, err := lookup(ctx, cfg.RepoFullName(w.Repo), prNumber)
		if err != nil {
			continue // skip on API error, try next cycle
		}
		if !mergedFor(info, cleanupAfterMerge, now) {
			continue
		}
		if err := queue.Queue(ctx, key, workqueue.Options{}); err != nil {
			logf("Error queuing cleanup for %s PR #%d: %v", w.Repo, prNumber, err)
		}
	}
}

// featurePR returns the PR whose head is the work stream's branch in the
// first of its GitHub remotes that has one, or 0.
func featurePR(ctx context.Context, fullRepo string, w wt.Worktree, findPR prByHeadFunc) int {
	owners, err := wt.RemoteOwners(w.Path)
	if err != nil {
		return 0
	}
	for _, owner := range owners {
		n, err := findPR(ctx, fullRepo, owner, w.Branch)
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}
