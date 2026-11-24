// Package supervisor drives the user's own PRs toward a verified landing.
//
// Each poll reads every open PR the user authored (and the ones merged in the
// last day) in one GitHub query, derives who owns the next step, and takes the
// deterministic steps itself: update a branch that is behind, rerun checks
// that failed for infrastructure reasons, and arm auto-merge (through
// land-watch) on PRs whose landing the user approved with `zen land`. Only a
// change that needs judgment, a change-caused CI failure, a merge conflict,
// or review feedback, wakes an agent, and only once per distinct state.
package supervisor

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/mgreau/zen/internal/github"
)

// Owner is who the next step on a PR belongs to. The order is the display
// order of the sections.
type Owner string

const (
	OwnerYou       Owner = "you"
	OwnerAgent     Owner = "agent"
	OwnerReviewer  Owner = "reviewer"
	OwnerDeploying Owner = "deploying"
	OwnerLanded    Owner = "landed"
)

// Owners lists every owner in display order.
var Owners = []Owner{OwnerYou, OwnerAgent, OwnerReviewer, OwnerDeploying, OwnerLanded}

// Title is the section heading for an owner.
func (o Owner) Title() string {
	switch o {
	case OwnerYou:
		return "NEEDS YOU"
	case OwnerAgent:
		return "AGENT WORKING"
	case OwnerReviewer:
		return "WAITING ON REVIEW"
	case OwnerDeploying:
		return "DEPLOYING"
	case OwnerLanded:
		return "LANDED TODAY"
	}
	return strings.ToUpper(string(o))
}

// Item is one PR in the snapshot.
type Item struct {
	Repo     string    `json:"repo"` // configured short name
	FullRepo string    `json:"full_repo"`
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Head     string    `json:"head"`
	Owner    Owner     `json:"owner"`
	Summary  string    `json:"summary"`
	Worktree string    `json:"worktree,omitempty"`
	Land     bool      `json:"land"`
	MergedAt time.Time `json:"merged_at,omitempty"`
}

// Deploy is land-watch's view of a merged PR.
type Deploy struct {
	Class  string // merged_ok, merged_failed, pending_deploy, waiting_release
	Landed bool   // landed=yes
	None   bool   // every stage status=none: nothing deployed
	Stage  string // first stage that is not success, for the summary
}

// Observed is everything known about one PR at poll time.
type Observed struct {
	PR          ghpkg.SupervisedPR
	Repo        string
	Self        string // the user's login, to skip their own review replies
	Worktree    string // "" when no local worktree checks out the head branch
	SessionOpen bool   // an interactive agent runs in the worktree
	Land        bool   // the user approved landing
	Checks      []ghpkg.PRCheck
	Wake        *WakeRun // a wake running or queued for this PR
	Memory      PRMemory
	Deploy      *Deploy // merged PRs only, nil when land-watch has no answer
	// SettleAfter is how long after merging a PR with no deploy in flight
	// counts as landed (watch.cleanup_after_merge).
	SettleAfter time.Duration
}

// PRMemory is what the supervisor remembers about a PR between polls.
type PRMemory struct {
	// HandledKey is the trigger key of the last wake, so the same state
	// never wakes an agent twice.
	HandledKey string `json:"handled_key,omitempty"`
	// HandledHead is the head commit when that wake started; a different
	// head now means the agent pushed.
	HandledHead string `json:"handled_head,omitempty"`
	// Waking is set while that wake has not reported back. If it is still
	// set with no wake running, the daemon restarted mid-wake and the wake
	// is lost, so the state is woken again.
	Waking bool `json:"waking,omitempty"`
	// Stuck is set when that wake failed, timed out, or reported STUCK.
	Stuck string `json:"stuck,omitempty"`
	// RerunHead and UpdateHead are the head SHAs already rerun or updated.
	RerunHead  string `json:"rerun_head,omitempty"`
	UpdateHead string `json:"update_head,omitempty"`
	// FixingSince is when the current run of judgment wakes began; it resets
	// once the PR has nothing left that needs judgment. Wakes stop after
	// fixWindow so a fix that keeps failing cannot loop forever.
	FixingSince time.Time `json:"fixing_since,omitempty"`
}

// fixWindow bounds, in time, how long the supervisor keeps waking agents on
// one PR before handing it to the user.
const fixWindow = 4 * time.Hour

// infraConclusions are GitHub's check conclusions for a run that did not get
// to judge the change. They are rerun once per head instead of waking an
// agent.
var infraConclusions = map[string]bool{
	"TIMED_OUT":       true,
	"CANCELLED":       true,
	"STARTUP_FAILURE": true,
	"STALE":           true,
}

// humanGated reports whether a failing check waits on a human attestation,
// such as a label confirming public code holds nothing proprietary. Only
// Nicholas can satisfy it, so it never wakes an agent.
func humanGated(label string) bool {
	return strings.Contains(strings.ToLower(label), "attestation")
}

// Issues are the problems on an open PR that need work.
type Issues struct {
	Conflict         bool
	Failing          []string // change-caused failing checks
	Infra            []string // infrastructure-failed checks
	Attest           []string // failing checks that wait on Nicholas's attestation
	ChangesRequested bool
	Threads          []ghpkg.ThreadComment // unresolved threads waiting on the author
	Pending          bool
	Behind           bool
}

// Judgment reports whether the issues need an agent.
func (i Issues) Judgment() bool {
	return i.Conflict || len(i.Failing) > 0 || i.ChangesRequested || len(i.Threads) > 0
}

// Describe is a short summary of the judgment issues.
func (i Issues) Describe() string {
	var parts []string
	if i.Conflict {
		parts = append(parts, "conflict")
	}
	if len(i.Failing) > 0 {
		parts = append(parts, "CI: "+strings.Join(i.Failing, ","))
	}
	if i.ChangesRequested {
		parts = append(parts, "changes requested")
	}
	if n := len(i.Threads); n > 0 {
		parts = append(parts, fmt.Sprintf("%d review thread%s", n, plural(n)))
	}
	return strings.Join(parts, " · ")
}

// FindIssues classifies an open PR's state.
func FindIssues(o Observed) Issues {
	pr := o.PR
	var is Issues
	is.Conflict = pr.Mergeable == "CONFLICTING" || pr.MergeStateStatus == "DIRTY"
	is.Behind = pr.MergeStateStatus == "BEHIND"
	is.ChangesRequested = pr.ReviewDecision == "CHANGES_REQUESTED"
	for _, c := range o.Checks {
		switch {
		case c.Name != "" && c.Status != "" && c.Status != "COMPLETED":
			is.Pending = true
		case c.Name != "" && infraConclusions[c.Conclusion]:
			is.Infra = append(is.Infra, c.Label())
		case (c.Conclusion == "FAILURE" || c.Conclusion == "ACTION_REQUIRED" || c.State == "FAILURE" || c.State == "ERROR") && humanGated(c.Label()):
			is.Attest = append(is.Attest, c.Label())
		case c.Conclusion == "FAILURE" || c.Conclusion == "ACTION_REQUIRED" || c.State == "FAILURE" || c.State == "ERROR":
			is.Failing = append(is.Failing, c.Label())
		case c.State == "PENDING" || c.State == "EXPECTED":
			is.Pending = true
		}
	}
	if len(o.Checks) == 0 {
		switch pr.RollupState() {
		case "PENDING", "EXPECTED":
			is.Pending = true
		}
	}
	sort.Strings(is.Failing)
	sort.Strings(is.Infra)
	sort.Strings(is.Attest)
	for _, t := range pr.ReviewThreads.Nodes {
		if t.IsResolved || t.IsOutdated || len(t.Comments.Nodes) == 0 {
			continue
		}
		last := t.Comments.Nodes[len(t.Comments.Nodes)-1]
		if o.Self != "" && last.Author.Login == o.Self {
			continue // the author answered; the reviewer owns the thread
		}
		is.Threads = append(is.Threads, last)
	}
	return is
}

// TriggerKey identifies a state that needs judgment. A wake happens only when
// the key differs from the last handled one. CI failures and conflicts are
// keyed on the head commit, since a push replaces them. Review feedback is
// keyed on the comments themselves: a fix push leaves them in place until the
// reviewer resolves or re-reviews, and that must not wake an agent again.
func TriggerKey(pr ghpkg.SupervisedPR, is Issues) string {
	if !is.Judgment() {
		return ""
	}
	var parts []string
	if is.Conflict || len(is.Failing) > 0 {
		parts = append(parts, pr.HeadRefOid)
	}
	if is.Conflict {
		parts = append(parts, "conflict")
	}
	if len(is.Failing) > 0 {
		parts = append(parts, "ci="+strings.Join(is.Failing, ","))
	}
	if is.ChangesRequested {
		parts = append(parts, "changes")
	}
	for _, t := range is.Threads {
		parts = append(parts, t.URL)
	}
	return strings.Join(parts, "|")
}

// ActionKind is a step the supervisor takes.
type ActionKind string

const (
	ActionUpdateBranch ActionKind = "update-branch"
	ActionRerun        ActionKind = "rerun"
	ActionWake         ActionKind = "wake"
)

// Action is one step to take this poll.
type Action struct {
	Kind ActionKind
	Key  string   // ActionWake: trigger key
	Runs []string // ActionRerun: workflow run IDs
}

// Derive computes the item and the deterministic or judgment actions for one
// PR. Arming auto-merge is not an action here: land-watch arms every
// land-approved PR it is given, once approvals exist.
func Derive(o Observed, now time.Time) (Item, []Action) {
	pr := o.PR
	item := Item{
		Repo:     o.Repo,
		FullRepo: pr.Repository.NameWithOwner,
		Number:   pr.Number,
		Title:    pr.Title,
		URL:      pr.URL,
		Head:     pr.HeadRefOid,
		Worktree: o.Worktree,
		Land:     o.Land,
	}
	if pr.State == "MERGED" {
		item.MergedAt, _ = time.Parse(time.RFC3339, pr.MergedAt)
		item.Owner, item.Summary = deployState(o.Deploy)
		// No deploy signal, or nothing in flight, long after the merge: it
		// has landed as far as anyone can tell. Only live deploys stay.
		settled := o.Deploy == nil || o.Deploy.None || o.Deploy.Class == "merged_ok"
		if item.Owner == OwnerDeploying && settled && o.SettleAfter > 0 && !item.MergedAt.IsZero() && now.Sub(item.MergedAt) > o.SettleAfter {
			item.Owner, item.Summary = OwnerLanded, "merged · no deploy in flight"
		}
		return item, nil
	}

	is := FindIssues(o)
	set := func(owner Owner, summary string) { item.Owner, item.Summary = owner, summary }

	if pr.IsDraft {
		set(OwnerYou, "draft")
		return item, nil
	}
	if o.Wake != nil {
		set(OwnerAgent, o.Wake.Describe(now))
		return item, nil
	}

	var actions []Action
	enrolled := o.Worktree != ""
	key := TriggerKey(pr, is)
	switch {
	case is.Judgment() && !enrolled:
		set(OwnerYou, "no worktree: "+is.Describe())
	case is.Judgment() && o.Memory.HandledKey == key && o.Memory.Waking && !o.SessionOpen:
		set(OwnerAgent, "requeued after restart: "+is.Describe())
		actions = append(actions, Action{Kind: ActionWake, Key: key})
	case is.Judgment() && o.Memory.HandledKey == key && o.Memory.Stuck != "":
		set(OwnerYou, "stuck: "+o.Memory.Stuck)
	case is.Judgment() && o.SessionOpen:
		set(OwnerAgent, "session open: "+is.Describe())
	case is.Judgment() && o.Memory.HandledKey == key && o.Memory.HandledHead != pr.HeadRefOid:
		set(OwnerReviewer, "fix pushed · awaiting reviewer: "+is.Describe())
	case is.Judgment() && o.Memory.HandledKey == key:
		set(OwnerYou, "agent pushed no fix: "+is.Describe())
	case is.Judgment() && !o.Memory.FixingSince.IsZero() && now.Sub(o.Memory.FixingSince) > fixWindow:
		set(OwnerYou, "still failing after "+fixWindow.String()+" of fixes: "+is.Describe())
	case is.Judgment():
		set(OwnerAgent, "queued: "+is.Describe())
		actions = append(actions, Action{Kind: ActionWake, Key: key})
	case len(is.Attest) > 0:
		set(OwnerYou, "needs your attestation: "+strings.Join(is.Attest, ","))
	case len(is.Infra) > 0 && o.Memory.RerunHead == pr.HeadRefOid:
		set(OwnerYou, "infra failure after rerun: "+strings.Join(is.Infra, ","))
	case len(is.Infra) > 0 && enrolled:
		set(OwnerAgent, "rerunning: "+strings.Join(is.Infra, ","))
		actions = append(actions, Action{Kind: ActionRerun, Runs: runIDs(o.Checks)})
	case len(is.Infra) > 0:
		set(OwnerYou, "no worktree: infra failure "+strings.Join(is.Infra, ","))
	case is.Pending:
		set(OwnerReviewer, "CI running"+reviewSuffix(pr))
	case is.Behind && enrolled && o.Memory.UpdateHead != pr.HeadRefOid:
		set(OwnerAgent, "updating branch")
		actions = append(actions, Action{Kind: ActionUpdateBranch})
	case pr.ReviewDecision == "APPROVED" && o.Land && pr.AutoMergeRequest != nil:
		set(OwnerDeploying, "auto-merge armed · merging")
	case pr.ReviewDecision == "APPROVED" && o.Land:
		set(OwnerDeploying, "approved · arming auto-merge")
	case pr.ReviewDecision == "APPROVED":
		set(OwnerYou, "CI green · approved · land?")
	case o.Land:
		set(OwnerReviewer, "land approved · awaiting review")
	default:
		set(OwnerReviewer, "CI green · awaiting review")
	}
	return item, actions
}

func reviewSuffix(pr ghpkg.SupervisedPR) string {
	switch pr.ReviewDecision {
	case "APPROVED":
		return " · approved"
	case "REVIEW_REQUIRED":
		return " · awaiting review"
	}
	return ""
}

func deployState(d *Deploy) (Owner, string) {
	switch {
	case d == nil:
		return OwnerDeploying, "merged · deploy unknown"
	case d.Class == "merged_failed":
		return OwnerYou, "deploy failed: " + d.Stage
	case d.Landed:
		return OwnerLanded, "deployed"
	case d.Class == "merged_ok" && d.None:
		return OwnerLanded, "merged · nothing to deploy"
	case d.Class == "waiting_release":
		return OwnerDeploying, "waiting for release tag"
	case d.Class == "pending_deploy":
		return OwnerDeploying, "deploying: " + d.Stage
	}
	return OwnerDeploying, "merged · " + d.Class
}

var runIDPattern = regexp.MustCompile(`/actions/runs/(\d+)`)

// runIDs returns the distinct workflow run IDs of infrastructure-failed check
// runs, read from their details URLs.
func runIDs(checks []ghpkg.PRCheck) []string {
	seen := map[string]bool{}
	var ids []string
	for _, c := range checks {
		if !infraConclusions[c.Conclusion] {
			continue
		}
		if m := runIDPattern.FindStringSubmatch(c.DetailsURL); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	return ids
}

// failingRunID returns the workflow run ID of the first change-caused failing
// check run, for the brief's log excerpt.
func failingRunID(checks []ghpkg.PRCheck) string {
	for _, c := range checks {
		if c.Conclusion == "FAILURE" {
			if m := runIDPattern.FindStringSubmatch(c.DetailsURL); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// prKey is the stable key for a PR across polls.
func prKey(fullRepo string, number int) string {
	return fullRepo + "#" + strconv.Itoa(number)
}
