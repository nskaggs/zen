package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SupervisedPR is what the PR supervisor reads for one of the user's PRs on
// every poll: draft and review state, mergeability, the head commit's check
// rollup, and unresolved review threads. Individual checks are fetched only
// for a failing PR (see GetPRChecks), which keeps the poll query cheap.
type SupervisedPR struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	State          string `json:"state"` // OPEN or MERGED
	IsDraft        bool   `json:"isDraft"`
	ReviewDecision string `json:"reviewDecision"`
	Mergeable      string `json:"mergeable"` // MERGEABLE, CONFLICTING, UNKNOWN
	// MergeStateStatus (BEHIND, DIRTY, BLOCKED, CLEAN, ...) is slow for
	// GitHub to compute in a search, so the poll query leaves it out;
	// GetMergeState fills it for the PRs where it matters.
	MergeStateStatus string   `json:"mergeStateStatus"`
	MergedAt         string   `json:"mergedAt"`
	BaseRefName      string   `json:"baseRefName"`
	HeadRefName      string   `json:"headRefName"`
	HeadRefOid       string   `json:"headRefOid"`
	Repository       RepoInfo `json:"repository"`
	AutoMergeRequest *struct {
		EnabledAt string `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	ReviewThreads struct {
		Nodes []ReviewThread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// ReviewThread is one review thread with its latest comment.
type ReviewThread struct {
	IsResolved bool `json:"isResolved"`
	IsOutdated bool `json:"isOutdated"`
	Comments   struct {
		Nodes []ThreadComment `json:"nodes"`
	} `json:"comments"`
}

// ThreadComment is a review thread's latest comment.
type ThreadComment struct {
	URL    string `json:"url"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

// RollupState is the head commit's combined check state (SUCCESS, FAILURE,
// ERROR, PENDING, EXPECTED), or "" when the commit has no checks.
func (p SupervisedPR) RollupState() string {
	if len(p.Commits.Nodes) == 0 || p.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return ""
	}
	return p.Commits.Nodes[0].Commit.StatusCheckRollup.State
}

const openFields = `
      number title url state isDraft mergeable mergedAt
      baseRefName headRefName headRefOid
      repository { name nameWithOwner }
      autoMergeRequest { enabledAt }
      reviewThreads(first: 30) {
        nodes { isResolved isOutdated comments(last: 1) { nodes { url body author { login } } } }
      }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }`

const mergedFields = `
      number title url state mergedAt baseRefName headRefName headRefOid
      repository { name nameWithOwner }`

// GetSupervisedPRs reads every open PR the user authored plus the ones
// merged since mergedSince, across all repositories, and the viewer's login.
// Filtering to configured repositories is the caller's job: a search scoped
// to the user's own PRs stays small, so unscoped searches replace one per
// repository. GitHub computes reviewDecision slowly, so it is read in its own
// search, in parallel with the others; one combined query ran into GitHub's
// 10-second limit and failed with HTTP 502.
func GetSupervisedPRs(ctx context.Context, mergedSince time.Time) (string, []SupervisedPR, error) {
	open := "is:pr is:open author:@me archived:false"
	merged := "is:pr is:merged author:@me merged:>=" + mergedSince.UTC().Format("2006-01-02T15:04:05Z")
	queries := []string{
		`query($q: String!) { viewer { login } search(query: $q, type: ISSUE, first: 50) { nodes { ... on PullRequest {` + openFields + ` } } } }`,
		`query($q: String!) { search(query: $q, type: ISSUE, first: 50) { nodes { ... on PullRequest { number reviewDecision repository { nameWithOwner } } } } }`,
		`query($q: String!) { search(query: $q, type: ISSUE, first: 50) { nodes { ... on PullRequest {` + mergedFields + ` } } } }`,
	}
	terms := []string{open, open, merged}
	outs := make([][]byte, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = searchGraphQL(ctx, queries[i], terms[i])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return "", nil, err
		}
	}
	return parseSupervisedPRs(outs[0], outs[1], outs[2])
}

func searchGraphQL(ctx context.Context, query, q string) ([]byte, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query, "-f", "q="+q).Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("supervised PRs query timed out after %s", apiTimeout)
		}
		return nil, fmt.Errorf("GraphQL query failed: %s", ghError(err))
	}
	return out, nil
}

type searchResult struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Search struct {
			Nodes []SupervisedPR `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
}

// parseSupervisedPRs joins the open search, the review-decision search, and
// the merged search.
func parseSupervisedPRs(openOut, decisionOut, mergedOut []byte) (string, []SupervisedPR, error) {
	var open, decisions, merged searchResult
	for _, r := range []struct {
		out []byte
		dst *searchResult
	}{{openOut, &open}, {decisionOut, &decisions}, {mergedOut, &merged}} {
		if err := json.Unmarshal(r.out, r.dst); err != nil {
			return "", nil, fmt.Errorf("parsing GraphQL response: %w", err)
		}
	}
	decision := map[string]string{}
	for _, pr := range decisions.Data.Search.Nodes {
		decision[pr.Repository.NameWithOwner+"#"+strconv.Itoa(pr.Number)] = pr.ReviewDecision
	}
	var prs []SupervisedPR
	for _, pr := range open.Data.Search.Nodes {
		if pr.Number != 0 {
			pr.ReviewDecision = decision[pr.Repository.NameWithOwner+"#"+strconv.Itoa(pr.Number)]
			prs = append(prs, pr)
		}
	}
	for _, pr := range merged.Data.Search.Nodes {
		if pr.Number != 0 {
			prs = append(prs, pr)
		}
	}
	return open.Data.Viewer.Login, prs, nil
}

// PRCheck is one check on a PR's head commit, as `gh pr view --json
// statusCheckRollup` reports it: a check run (Name, Status, Conclusion) or a
// commit status (Context, State).
type PRCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	Context    string `json:"context"`
	State      string `json:"state"`
	TargetURL  string `json:"targetUrl"`
}

// Label is the check's display name.
func (c PRCheck) Label() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// GetPRChecks lists the head commit's checks for one PR.
func GetPRChecks(ctx context.Context, fullRepo string, number int) ([]PRCheck, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(number), "--repo", fullRepo, "--json", "statusCheckRollup")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr view %s#%d: %s", fullRepo, number, ghError(err))
	}
	var v struct {
		StatusCheckRollup []PRCheck `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("parsing checks for %s#%d: %w", fullRepo, number, err)
	}
	return v.StatusCheckRollup, nil
}

// GetMergeState returns one PR's mergeStateStatus.
func GetMergeState(ctx context.Context, fullRepo string, number int) (string, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(number), "--repo", fullRepo, "--json", "mergeStateStatus", "--jq", ".mergeStateStatus")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view %s#%d: %s", fullRepo, number, ghError(err))
	}
	return strings.TrimSpace(string(out)), nil
}
