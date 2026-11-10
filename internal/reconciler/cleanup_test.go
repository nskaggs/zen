package reconciler

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/inmem"
	"github.com/mgreau/zen/internal/agent"
	"github.com/mgreau/zen/internal/config"
	ghpkg "github.com/mgreau/zen/internal/github"
)

func TestCleanupReconcile_InvalidKey(t *testing.T) {
	cfg := &config.Config{Repos: map[string]config.RepoConfig{
		"mono": {FullName: "chainguard-dev/mono", BasePath: "/tmp/test"},
	}}
	rec := NewCleanupReconciler(cfg)

	err := rec.Reconcile(context.Background(), "badkey", workqueue.Options{})
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
	if workqueue.GetNonRetriableDetails(err) == nil {
		t.Error("expected NonRetriableError for invalid key format")
	}
}

func TestCleanupReconcile_MissingWorktree(t *testing.T) {
	// Create a temp config pointing to a temp directory
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "testrepo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755)

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"testrepo": {FullName: "test/testrepo", BasePath: tmpDir},
		},
	}
	rec := NewCleanupReconciler(cfg)
	rec.prMergeInfo = func(context.Context, string, int) (*ghpkg.PRMergeInfo, error) {
		t.Fatal("a missing worktree must not call GitHub")
		return nil, nil
	}

	// Worktree path doesn't exist, so cleanup is a no-op
	err := rec.Reconcile(context.Background(), "testrepo:999", workqueue.Options{})
	if err != nil {
		t.Fatalf("unexpected error for missing worktree: %v", err)
	}
}

func TestCleanupReconcile_ArchivesDirtyReviewWithWIPCommit(t *testing.T) {
	cfg, worktreePath, prHead := cleanupFixture(t)
	if err := os.WriteFile(filepath.Join(worktreePath, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := NewCleanupReconciler(cfg)
	rec.prMergeInfo = mergedInfo(prHead, 2*time.Hour)
	if err := rec.Reconcile(context.Background(), "testrepo:999", workqueue.Options{}); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatal("dirty merged review was not archived")
	}
	ref := archiveRef(t, cfg, "testrepo", "testrepo-pr-999")
	if files := cleanupGitOut(t, filepath.Join(filepath.Dir(worktreePath), "testrepo"), "ls-tree", "-r", "--name-only", ref); !strings.Contains(files, "notes.txt") {
		t.Fatalf("archive %s is missing notes.txt: %s", ref, files)
	}
}

func TestCleanupReconcile_ArchiveKeepsLocalCommit(t *testing.T) {
	cfg, worktreePath, prHead := cleanupFixture(t)
	if err := os.WriteFile(filepath.Join(worktreePath, "tracked"), []byte("reviewer fix"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, worktreePath, "commit", "-am", "reviewer fix")
	local := cleanupGitOut(t, worktreePath, "rev-parse", "HEAD")

	rec := NewCleanupReconciler(cfg)
	rec.prMergeInfo = mergedInfo(prHead, 2*time.Hour)
	if err := rec.Reconcile(context.Background(), "testrepo:999", workqueue.Options{}); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatal("merged review with a local commit was not archived")
	}
	ref := archiveRef(t, cfg, "testrepo", "testrepo-pr-999")
	if got := cleanupGitOut(t, filepath.Join(filepath.Dir(worktreePath), "testrepo"), "rev-parse", ref); got != local {
		t.Fatalf("%s = %s, want local commit %s", ref, got, local)
	}
}

func TestCleanupReconcile_RemovesMergedReviewWithOnlyZenContext(t *testing.T) {
	cfg, worktreePath, prHead := cleanupFixture(t)
	if _, err := agent.New(agent.Claude, "").InjectContext(worktreePath, "generated"); err != nil {
		t.Fatal(err)
	}

	rec := NewCleanupReconciler(cfg)
	rec.prMergeInfo = mergedInfo(prHead, 2*time.Hour)
	if err := rec.Reconcile(context.Background(), "testrepo:999", workqueue.Options{}); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatal("merged review with only zen-owned files was not removed")
	}
}

func TestCleanupReconcile_RechecksMergeGate(t *testing.T) {
	tests := []struct {
		name string
		info ghpkg.PRMergeInfo
	}{
		{name: "closed without merging", info: ghpkg.PRMergeInfo{State: "CLOSED"}},
		{name: "open", info: ghpkg.PRMergeInfo{State: "OPEN"}},
		{name: "merged too recently", info: ghpkg.PRMergeInfo{State: "MERGED", MergedAt: time.Now().Add(-30 * time.Minute)}},
		{name: "merged without a merge time", info: ghpkg.PRMergeInfo{State: "MERGED"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, worktreePath, prHead := cleanupFixture(t)
			info := tt.info
			info.HeadSHA = prHead

			rec := NewCleanupReconciler(cfg)
			rec.prMergeInfo = func(context.Context, string, int) (*ghpkg.PRMergeInfo, error) {
				return &info, nil
			}
			if err := rec.Reconcile(context.Background(), "testrepo:999", workqueue.Options{}); err != nil {
				t.Fatalf("Reconcile() error: %v", err)
			}
			if _, err := os.Stat(worktreePath); err != nil {
				t.Fatal("worktree was removed before its PR passed the merge gate")
			}
		})
	}
}

func TestScanMergedPRs(t *testing.T) {
	basePath := t.TempDir()
	originPath := filepath.Join(basePath, "mono")
	initCleanupRepo(t, basePath, originPath)
	runCleanupGit(t, originPath, "worktree", "add", "-b", "pr-1", filepath.Join(basePath, "mono-pr-1"))
	runCleanupGit(t, originPath, "worktree", "add", "-b", "pr-2", filepath.Join(basePath, "mono-pr-2"))
	runCleanupGit(t, originPath, "worktree", "add", "-b", "mgreau/fix-pr-3", filepath.Join(basePath, "mono-fix-pr-3"))
	runCleanupGit(t, originPath, "worktree", "add", "-b", "pr-4", filepath.Join(basePath, "mono-pr-4"))
	runCleanupGit(t, originPath, "worktree", "add", "-b", "nskaggs/feat", filepath.Join(basePath, "mono-feat"))
	runCleanupGit(t, originPath, "remote", "add", "origin", "https://github.com/example/mono.git")
	runCleanupGit(t, originPath, "remote", "add", "nskaggs", "git@github.com:nskaggs/mono.git")

	now := time.Now()
	prs := map[int]ghpkg.PRMergeInfo{
		1: {State: "MERGED", MergedAt: now.Add(-30 * time.Minute)},
		2: {State: "MERGED", MergedAt: now.Add(-2 * time.Hour)},
		3: {State: "MERGED", MergedAt: now.Add(-2 * time.Hour)},
		4: {State: "CLOSED"},
		5: {State: "MERGED", MergedAt: now.Add(-2 * time.Hour)},
	}
	var looked []int
	lookup := func(_ context.Context, fullRepo string, prNumber int) (*ghpkg.PRMergeInfo, error) {
		if fullRepo != "example/mono" {
			t.Errorf("lookup repo = %q, want example/mono", fullRepo)
		}
		looked = append(looked, prNumber)
		info := prs[prNumber]
		return &info, nil
	}

	cfg := &config.Config{Repos: map[string]config.RepoConfig{
		"mono": {FullName: "example/mono", BasePath: basePath},
	}}
	queue := inmem.NewWorkQueue(10)
	findPR := func(_ context.Context, fullRepo, owner, branch string) (int, error) {
		if owner == "nskaggs" && branch == "nskaggs/feat" {
			return 5, nil
		}
		return 0, nil
	}
	scanMergedPRs(context.Background(), cfg, queue, lookup, findPR, time.Hour, now)

	_, queued, _, err := queue.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, k := range queued {
		keys = append(keys, k.Name())
	}
	// PR 1 merged 30m ago (< 1h), PR 3 is a work stream with no PR of its
	// own, PR 4 never merged, PR 5 is mono-feat's PR from the nskaggs fork.
	sort.Strings(keys)
	if strings.Join(keys, ",") != "mono:2,mono:5:mono-feat" {
		t.Errorf("queued %v, want [mono:2 mono:5:mono-feat]", keys)
	}
	for _, pr := range looked {
		if pr == 3 {
			t.Error("work stream mono-fix-pr-3 was looked up as a review of PR 3")
		}
	}
}

// cleanupFixture creates <base>/testrepo with one commit, the PR head, and
// the review worktree <base>/testrepo-pr-999 on branch pr-999.
func cleanupFixture(t *testing.T) (cfg *config.Config, worktreePath, prHead string) {
	t.Helper()
	t.Setenv("ZEN_HOME", t.TempDir())
	basePath := t.TempDir()
	originPath := filepath.Join(basePath, "testrepo")
	worktreePath = filepath.Join(basePath, "testrepo-pr-999")
	initCleanupRepo(t, basePath, originPath)
	runCleanupGit(t, originPath, "worktree", "add", "-b", "pr-999", worktreePath)

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	cfg = &config.Config{Repos: map[string]config.RepoConfig{
		"testrepo": {FullName: "test/testrepo", BasePath: basePath},
	}}
	return cfg, worktreePath, strings.TrimSpace(string(out))
}

func initCleanupRepo(t *testing.T, basePath, originPath string) {
	t.Helper()
	runCleanupGit(t, basePath, "init", "-b", "main", originPath)
	runCleanupGit(t, originPath, "config", "user.email", "test@example.com")
	runCleanupGit(t, originPath, "config", "user.name", "Test")
	runCleanupGit(t, originPath, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(originPath, "tracked"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, originPath, "add", "tracked")
	runCleanupGit(t, originPath, "commit", "-m", "initial")
}

func mergedInfo(prHead string, ago time.Duration) prMergeInfoFunc {
	return func(context.Context, string, int) (*ghpkg.PRMergeInfo, error) {
		return &ghpkg.PRMergeInfo{State: "MERGED", HeadSHA: prHead, MergedAt: time.Now().Add(-ago)}, nil
	}
}

func TestCleanupReconcile_Feature(t *testing.T) {
	tests := []struct {
		name         string
		ago          time.Duration
		verified     bool
		verifyErr    error
		wantArchived bool
		wantErr      bool
	}{
		{name: "landed", ago: 2 * time.Hour, verified: true, wantArchived: true},
		{name: "deploy not verified", ago: 2 * time.Hour},
		{name: "land-watch failed", ago: 2 * time.Hour, verifyErr: errors.New("gh: rate limited"), wantErr: true},
		{name: "merged too recently", ago: 30 * time.Minute, verified: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ZEN_HOME", t.TempDir())
			basePath := t.TempDir()
			originPath := filepath.Join(basePath, "mono")
			worktreePath := filepath.Join(basePath, "mono-feat")
			initCleanupRepo(t, basePath, originPath)
			runCleanupGit(t, originPath, "worktree", "add", "-b", "nskaggs/feat", worktreePath)
			if err := os.WriteFile(filepath.Join(worktreePath, "follow-up.txt"), []byte("wip"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Repos: map[string]config.RepoConfig{
				"mono": {FullName: "example/mono", BasePath: basePath},
			}}

			rec := NewCleanupReconciler(cfg)
			rec.prMergeInfo = mergedInfo("", tt.ago)
			verifyCalls := 0
			rec.deployVerified = func(_ context.Context, fullRepo string, prNumber int) (bool, string, error) {
				verifyCalls++
				if fullRepo != "example/mono" || prNumber != 5 {
					t.Errorf("verify(%s, %d), want example/mono 5", fullRepo, prNumber)
				}
				return tt.verified, "example/mono#5 landed=no", tt.verifyErr
			}
			err := rec.Reconcile(context.Background(), MakeFeatureKey("mono", 5, "mono-feat"), workqueue.Options{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.ago < time.Hour && verifyCalls != 0 {
				t.Error("deploy was checked before cleanup_after_merge passed")
			}
			_, statErr := os.Stat(worktreePath)
			if archived := os.IsNotExist(statErr); archived != tt.wantArchived {
				t.Fatalf("archived = %t, want %t", archived, tt.wantArchived)
			}
			if tt.wantArchived {
				ref := archiveRef(t, cfg, "mono", "mono-feat")
				if files := cleanupGitOut(t, originPath, "ls-tree", "-r", "--name-only", ref); !strings.Contains(files, "follow-up.txt") {
					t.Fatalf("archive %s is missing follow-up.txt: %s", ref, files)
				}
				if branch := cleanupGitOut(t, originPath, "rev-parse", "nskaggs/feat"); branch != cleanupGitOut(t, originPath, "rev-parse", ref) {
					t.Fatal("branch does not hold the archived commit")
				}
			}
		})
	}
}

func TestParseFeatureKeyRejectsReviewKeys(t *testing.T) {
	if _, _, _, err := ParseFeatureKey(MakePRKey("mono", 42)); err == nil {
		t.Fatal("a review key parsed as a feature key")
	}
	if _, _, err := ParsePRKey(MakeFeatureKey("mono", 42, "mono-feat")); err == nil {
		t.Fatal("a feature key parsed as a review key")
	}
	repo, n, name, err := ParseFeatureKey(MakeFeatureKey("mono", 42, "mono-feat"))
	if err != nil || repo != "mono" || n != 42 || name != "mono-feat" {
		t.Fatalf("ParseFeatureKey() = %q %d %q %v", repo, n, name, err)
	}
}

// archiveRef returns the single archive ref made for worktree name.
func archiveRef(t *testing.T, cfg *config.Config, repo, name string) string {
	t.Helper()
	origin := filepath.Join(cfg.RepoBasePath(repo), repo)
	refs := cleanupGitOut(t, origin, "for-each-ref", "--format=%(refname)", "refs/archive/")
	if !strings.HasSuffix(refs, "/"+name) || strings.Contains(refs, "\n") {
		t.Fatalf("archive refs = %q, want one ending in /%s", refs, name)
	}
	return refs
}

func cleanupGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func runCleanupGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
