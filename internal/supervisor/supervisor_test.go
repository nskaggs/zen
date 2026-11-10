package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mgreau/zen/internal/config"
	ghpkg "github.com/mgreau/zen/internal/github"
	"github.com/mgreau/zen/internal/worktree"
)

var now = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

func openPR(n int) ghpkg.SupervisedPR {
	pr := ghpkg.SupervisedPR{
		Number: n, Title: "fix the thing", URL: "https://github.com/o/r/pull/" + itoa(n),
		State: "OPEN", ReviewDecision: "REVIEW_REQUIRED", Mergeable: "MERGEABLE",
		BaseRefName: "main", HeadRefName: "nskaggs/thing", HeadRefOid: "aaaaaaaaaa",
	}
	pr.Repository.NameWithOwner = "o/r"
	return pr
}

func withRollup(pr ghpkg.SupervisedPR, state string) ghpkg.SupervisedPR {
	pr.Commits.Nodes = make([]struct {
		Commit struct {
			StatusCheckRollup *struct {
				State string `json:"state"`
			} `json:"statusCheckRollup"`
		} `json:"commit"`
	}, 1)
	pr.Commits.Nodes[0].Commit.StatusCheckRollup = &struct {
		State string `json:"state"`
	}{State: state}
	return pr
}

func thread(author, url string, resolved bool) ghpkg.ReviewThread {
	var t ghpkg.ReviewThread
	t.IsResolved = resolved
	c := ghpkg.ThreadComment{URL: url, Body: "please fix"}
	c.Author.Login = author
	t.Comments.Nodes = []ghpkg.ThreadComment{c}
	return t
}

func failing(name string) ghpkg.PRCheck {
	return ghpkg.PRCheck{Name: name, Status: "COMPLETED", Conclusion: "FAILURE", DetailsURL: "https://github.com/o/r/actions/runs/11/job/1"}
}

func TestDerive(t *testing.T) {
	approved := withRollup(openPR(1), "SUCCESS")
	approved.ReviewDecision = "APPROVED"
	armed := approved
	armed.AutoMergeRequest = &struct {
		EnabledAt string `json:"enabledAt"`
	}{EnabledAt: "x"}
	draft := openPR(1)
	draft.IsDraft = true
	conflict := openPR(1)
	conflict.Mergeable = "CONFLICTING"
	behind := approved
	behind.MergeStateStatus = "BEHIND"
	reviewed := withRollup(openPR(1), "SUCCESS")
	reviewed.ReviewThreads.Nodes = []ghpkg.ReviewThread{thread("bot", "u1", false)}
	selfReply := withRollup(openPR(1), "SUCCESS")
	selfReply.ReviewThreads.Nodes = []ghpkg.ReviewThread{thread("me", "u1", false), thread("bot", "u2", true)}

	ciKey := TriggerKey(openPR(1), Issues{Failing: []string{"test"}})
	cases := []struct {
		name        string
		o           Observed
		owner       Owner
		summary     string
		wantActions []ActionKind
	}{
		{"draft", Observed{PR: draft, Worktree: "/w"}, OwnerYou, "draft", nil},
		{"green awaiting review", Observed{PR: withRollup(openPR(1), "SUCCESS"), Worktree: "/w"}, OwnerReviewer, "CI green · awaiting review", nil},
		{"approved needs land", Observed{PR: approved, Worktree: "/w"}, OwnerYou, "land?", nil},
		{"approved and landing", Observed{PR: approved, Land: true}, OwnerDeploying, "arming", nil},
		{"armed", Observed{PR: armed, Land: true}, OwnerDeploying, "armed", nil},
		{"behind updates", Observed{PR: behind, Worktree: "/w"}, OwnerAgent, "updating branch", []ActionKind{ActionUpdateBranch}},
		{"behind already updated", Observed{PR: behind, Worktree: "/w", Memory: PRMemory{UpdateHead: "aaaaaaaaaa"}}, OwnerYou, "land?", nil},
		{"ci failure wakes", Observed{PR: openPR(1), Worktree: "/w", Checks: []ghpkg.PRCheck{failing("test")}}, OwnerAgent, "queued: CI: test", []ActionKind{ActionWake}},
		{"conflict wakes", Observed{PR: conflict, Worktree: "/w"}, OwnerAgent, "conflict", []ActionKind{ActionWake}},
		{"no worktree no action", Observed{PR: openPR(1), Checks: []ghpkg.PRCheck{failing("test")}}, OwnerYou, "no worktree", nil},
		{"session open no wake", Observed{PR: openPR(1), Worktree: "/w", SessionOpen: true, Checks: []ghpkg.PRCheck{failing("test")}}, OwnerAgent, "session open", nil},
		{"handled and stuck", Observed{PR: openPR(1), Worktree: "/w", Checks: []ghpkg.PRCheck{failing("test")}, Memory: PRMemory{HandledKey: ciKey, HandledHead: "aaaaaaaaaa", Stuck: "needs a decision"}}, OwnerYou, "stuck: needs a decision", nil},
		{"handled no push", Observed{PR: openPR(1), Worktree: "/w", Checks: []ghpkg.PRCheck{failing("test")}, Memory: PRMemory{HandledKey: ciKey, HandledHead: "aaaaaaaaaa"}}, OwnerYou, "pushed no fix", nil},
		{"fix window over", Observed{PR: openPR(1), Worktree: "/w", Checks: []ghpkg.PRCheck{failing("test")}, Memory: PRMemory{FixingSince: now.Add(-5 * time.Hour)}}, OwnerYou, "still failing", nil},
		{"review thread wakes", Observed{PR: reviewed, Worktree: "/w", Self: "me"}, OwnerAgent, "1 review thread", []ActionKind{ActionWake}},
		{"own reply waits on reviewer", Observed{PR: selfReply, Worktree: "/w", Self: "me"}, OwnerReviewer, "awaiting review", nil},
		{"infra failure reruns", Observed{PR: openPR(1), Worktree: "/w", Checks: []ghpkg.PRCheck{{Name: "build", Status: "COMPLETED", Conclusion: "TIMED_OUT", DetailsURL: "https://github.com/o/r/actions/runs/42/job/1"}}}, OwnerAgent, "rerunning: build", []ActionKind{ActionRerun}},
		{"infra after rerun", Observed{PR: openPR(1), Worktree: "/w", Memory: PRMemory{RerunHead: "aaaaaaaaaa"}, Checks: []ghpkg.PRCheck{{Name: "build", Status: "COMPLETED", Conclusion: "CANCELLED"}}}, OwnerYou, "after rerun", nil},
		{"attestation waits on you", Observed{PR: openPR(1), Worktree: "/w", Checks: []ghpkg.PRCheck{failing("Open Source Attestation")}}, OwnerYou, "needs your attestation: Open Source Attestation", nil},
		{"pending", Observed{PR: withRollup(openPR(1), "PENDING"), Worktree: "/w"}, OwnerReviewer, "CI running", nil},
		{"wake running", Observed{PR: openPR(1), Worktree: "/w", Wake: &WakeRun{Summary: "CI: test", Started: now.Add(-4 * time.Minute)}}, OwnerAgent, "fixing CI: test · 4m", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item, actions := Derive(tc.o, now)
			if item.Owner != tc.owner || !strings.Contains(item.Summary, tc.summary) {
				t.Fatalf("got %s %q, want %s containing %q", item.Owner, item.Summary, tc.owner, tc.summary)
			}
			var kinds []ActionKind
			for _, a := range actions {
				kinds = append(kinds, a.Kind)
			}
			if strings.Join(kindStrings(kinds), ",") != strings.Join(kindStrings(tc.wantActions), ",") {
				t.Fatalf("actions = %v, want %v", kinds, tc.wantActions)
			}
		})
	}
}

func kindStrings(k []ActionKind) []string {
	var s []string
	for _, x := range k {
		s = append(s, string(x))
	}
	return s
}

func TestDeriveMerged(t *testing.T) {
	pr := openPR(1)
	pr.State, pr.MergedAt = "MERGED", now.Format(time.RFC3339)
	cases := []struct {
		d     *Deploy
		owner Owner
		want  string
	}{
		{nil, OwnerDeploying, "deploy unknown"},
		{&Deploy{Class: "merged_failed", Stage: "staging"}, OwnerYou, "deploy failed: staging"},
		{&Deploy{Class: "merged_ok", Landed: true}, OwnerLanded, "deployed"},
		{&Deploy{Class: "merged_ok", None: true}, OwnerLanded, "nothing to deploy"},
		{&Deploy{Class: "pending_deploy", Stage: "staging pending"}, OwnerDeploying, "deploying: staging pending"},
	}
	for _, tc := range cases {
		item, actions := Derive(Observed{PR: pr, Deploy: tc.d}, now)
		if item.Owner != tc.owner || !strings.Contains(item.Summary, tc.want) || actions != nil {
			t.Fatalf("%+v: got %s %q %v", tc.d, item.Owner, item.Summary, actions)
		}
	}
}

func TestTriggerKeyReviewIgnoresHead(t *testing.T) {
	pr := openPR(1)
	is := Issues{Threads: []ghpkg.ThreadComment{{URL: "u1"}}}
	moved := pr
	moved.HeadRefOid = "bbbbbbbb"
	if TriggerKey(pr, is) != TriggerKey(moved, is) {
		t.Fatal("a push must not re-trigger review feedback")
	}
	ci := Issues{Failing: []string{"test"}}
	if TriggerKey(pr, ci) == TriggerKey(moved, ci) {
		t.Fatal("a new head must re-trigger a CI failure")
	}
	if TriggerKey(pr, Issues{Infra: []string{"x"}}) != "" {
		t.Fatal("infrastructure failures need no judgment")
	}
}

func TestParseLandWatchAll(t *testing.T) {
	out := `o/r#1 url=u state=OPEN review=APPROVED armed=true gate_pending=false reviews=1 comments=0 fail=none
o/r#2 url=u state=MERGED review=APPROVED armed=false gate_pending=false reviews=1 comments=0 fail=none landed=no
o/r#2 stage=staging status=pending run="deploy":pending:u
o/r#3 url=u state=MERGED review=APPROVED armed=false gate_pending=false reviews=1 comments=0 fail=none landed=no
o/r#3 stage=staging status=none unmatched="ci"
o/r#4 url=u state=OPEN review=REVIEW_REQUIRED armed=false gate_pending=false reviews=0 comments=0 fail=none arm_error=not_approved
summary open=2 merged_ok=1`
	m := parseLandWatchAll(out)
	if !m["o/r#1"].armed || m["o/r#4"].armError != "not_approved" {
		t.Fatalf("open lines: %+v", m)
	}
	if d := m["o/r#2"].deploy(); d.Class != "pending_deploy" || d.Stage != "staging pending" {
		t.Fatalf("pending: %+v", d)
	}
	if d := m["o/r#3"].deploy(); d.Class != "merged_ok" || !d.None {
		t.Fatalf("none: %+v", d)
	}
	if _, ok := m["summary"]; ok {
		t.Fatal("summary line parsed as a PR")
	}
}

func TestLandFile(t *testing.T) {
	cfg := &config.Config{Repos: map[string]config.RepoConfig{"mono": {FullName: "chainguard-dev/mono"}}}
	for in, want := range map[string]LandEntry{
		"mono#5":                              {Repo: "chainguard-dev/mono", Number: 5},
		"o/r#7":                               {Repo: "o/r", Number: 7},
		"https://github.com/o/r/pull/9/files": {Repo: "o/r", Number: 9},
	} {
		got, err := ParseLandRef(cfg, in)
		if err != nil || got != want {
			t.Fatalf("%s: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"nope#1", "mono", "mono#x", "#3"} {
		if _, err := ParseLandRef(cfg, bad); err == nil {
			t.Fatalf("%s parsed", bad)
		}
	}
	path := filepath.Join(t.TempDir(), "land.json")
	entries, added := AddLand(nil, []LandEntry{{Repo: "o/r", Number: 2}, {Repo: "o/r", Number: 1}}, now)
	entries, again := AddLand(entries, []LandEntry{{Repo: "o/r", Number: 1}}, now)
	if len(added) != 2 || len(again) != 0 {
		t.Fatalf("added %v again %v", added, again)
	}
	if err := SaveLand(path, entries); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLand(path)
	if err != nil || len(loaded) != 2 || loaded[0].Number != 1 {
		t.Fatalf("loaded %+v %v", loaded, err)
	}
	if left := RemoveLand(loaded, []LandEntry{{Repo: "o/r", Number: 1}}); len(left) != 1 || left[0].Number != 2 {
		t.Fatalf("remove: %+v", left)
	}
	if missing, err := LoadLand(filepath.Join(t.TempDir(), "none.json")); err != nil || missing != nil {
		t.Fatal("a missing land file is an empty list")
	}
}

func TestRenderFitsWidth(t *testing.T) {
	long := strings.Repeat("a very long pull request title ", 5)
	snap := Snapshot{UpdatedAt: now.Add(-time.Minute), Error: strings.Repeat("e", 100), Items: []Item{
		{Repo: "infra", Number: 5388, Title: long, Owner: OwnerYou, Summary: "CI green · approved · land?"},
		{Repo: "mono", Number: 82734, Title: long, Owner: OwnerAgent, Summary: "fixing " + long},
		{Repo: "mono", Number: 1, Title: "t", Owner: OwnerLanded, Summary: "deployed", MergedAt: now.Add(-time.Hour)},
		{Repo: "mono", Number: 2, Title: "old", Owner: OwnerLanded, Summary: "deployed", MergedAt: now.Add(-30 * time.Hour)},
	}}
	events := []Event{{Time: now, PR: "chainguard-dev/mono#82734", Kind: "woke agent", Detail: long}}
	out := Render(snap, events, now)
	for _, line := range strings.Split(out, "\n") {
		if utf8.RuneCountInString(line) > Width {
			t.Fatalf("line wider than %d: %q", Width, line)
		}
	}
	order := []string{"NEEDS YOU", "AGENT WORKING", "LANDED TODAY", "RECENT"}
	last := -1
	for _, h := range order {
		i := strings.Index(out, h)
		if i < last {
			t.Fatalf("%s out of order:\n%s", h, out)
		}
		last = i
	}
	if strings.Contains(out, "#2 old") {
		t.Fatal("a PR merged before today is not shown as landed today")
	}
	if !strings.Contains(out, " mono#82734 woke agent") {
		t.Fatalf("event line missing:\n%s", out)
	}
}

func TestBuildBrief(t *testing.T) {
	var log strings.Builder
	for i := 0; i < 400; i++ {
		log.WriteString("line " + itoa(i) + "\n")
	}
	is := Issues{Conflict: true, Failing: []string{"test-unit"}, Threads: []ghpkg.ThreadComment{{URL: "https://c/1", Body: "multi\nline   body"}}}
	b := BuildBrief(BriefInput{URL: "https://github.com/o/r/pull/1", Head: "abcdef1234", Base: "main", Issues: is, Log: log.String(), Diffstat: " a.go | 2 +-"})
	for _, want := range []string{"(head abcdef1)", "Merge conflict with main", "test-unit", "line 399", "a.go | 2 +-", "multi line body (https://c/1)", "Never force push", `"FIXED <what you changed>"`} {
		if !strings.Contains(b, want) {
			t.Fatalf("brief missing %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "line 249\n") {
		t.Fatal("brief carries more than the log tail")
	}
}

func TestWakerGating(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	running, peak := 0, 0
	run := func(ctx context.Context, dir, prompt string) (string, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		running--
		mu.Unlock()
		return "work\nFIXED pushed", nil
	}
	w := NewWaker(2, time.Minute, run)
	ctx := context.Background()
	for _, pr := range []string{"a", "b", "c"} {
		if !w.Start(ctx, pr, "k", "s", "/w", "p") {
			t.Fatalf("%s not started", pr)
		}
	}
	if w.Start(ctx, "a", "k2", "s", "/w", "p") {
		t.Fatal("a second wake for the same PR started")
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return running == 2 })
	queued := 0
	for _, pr := range []string{"a", "b", "c"} {
		if r := w.Running(pr); r != nil && r.Queued {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("%d wakes queued, want 1", queued)
	}
	close(release)
	for range 3 {
		r := <-w.Results()
		if !r.Fixed || r.Reason != "pushed" {
			t.Fatalf("result %+v", r)
		}
	}
	if peak != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak)
	}
}

func TestWakerTimeout(t *testing.T) {
	w := NewWaker(1, 20*time.Millisecond, func(ctx context.Context, dir, prompt string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	w.Start(context.Background(), "a", "k", "s", "/w", "p")
	r := <-w.Results()
	if r.Fixed || !strings.Contains(r.Reason, "timed out") {
		t.Fatalf("result %+v", r)
	}
}

func TestInterpret(t *testing.T) {
	if r := interpret("p", "k", "x\nSTUCK needs a decision\n", nil, nil, time.Minute); r.Fixed || r.Reason != "needs a decision" {
		t.Fatalf("%+v", r)
	}
	if r := interpret("p", "k", "", errors.New("exit 1"), nil, time.Minute); r.Fixed || !strings.Contains(r.Reason, "agent failed") {
		t.Fatalf("%+v", r)
	}
	if r := interpret("p", "k", "done", nil, nil, time.Minute); r.Fixed {
		t.Fatalf("%+v", r)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeSupervisor wires a Supervisor to fakes and records every command.
type fakeSupervisor struct {
	*Supervisor
	cmds  []string
	woken chan string
}

func newFake(t *testing.T, prs []ghpkg.SupervisedPR, running bool) *fakeSupervisor {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Repos: map[string]config.RepoConfig{"r": {FullName: "o/r"}}}
	f := &fakeSupervisor{woken: make(chan string, 10)}
	f.Supervisor = &Supervisor{cfg: cfg, dir: dir, land: filepath.Join(dir, "land.json")}
	f.deps = deps{
		fetch: func(context.Context, time.Time) (string, []ghpkg.SupervisedPR, error) { return "me", prs, nil },
		checks: func(context.Context, string, int) ([]ghpkg.PRCheck, error) {
			return []ghpkg.PRCheck{failing("test")}, nil
		},
		mergeState: func(context.Context, string, int) (string, error) { return "BEHIND", nil },
		worktrees: func() ([]worktree.Worktree, error) {
			return []worktree.Worktree{{Path: "/w", Repo: "r", Branch: "nskaggs/thing", Type: worktree.TypeFeature}}, nil
		},
		running: func(string) bool { return running },
		exec: func(_ context.Context, dir, name string, args ...string) (string, error) {
			f.cmds = append(f.cmds, strings.TrimSpace(dir+" "+name+" "+strings.Join(args, " ")))
			return "log", nil
		},
		landWatch: func(context.Context, []LandEntry) (string, error) { return "", nil },
		now:       func() time.Time { return now },
	}
	f.waker = NewWaker(3, time.Minute, func(ctx context.Context, dir, prompt string) (string, error) {
		f.woken <- dir
		return "FIXED done", nil
	})
	return f
}

func TestPollWakesOnceAndRecords(t *testing.T) {
	pr := withRollup(openPR(1), "FAILURE")
	f := newFake(t, []ghpkg.SupervisedPR{pr}, false)
	if err := f.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dir := <-f.woken; dir != "/w" {
		t.Fatalf("woke in %s", dir)
	}
	want := []string{"gh run view 11 --repo o/r --log-failed", "/w git diff --stat origin/main...HEAD"}
	if strings.Join(f.cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("cmds:\n%s", strings.Join(f.cmds, "\n"))
	}
	waitFor(t, func() bool { return f.waker.Running(prKey("o/r", 1)) == nil })
	f.cmds = nil
	if err := f.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 {
		t.Fatalf("same state woke again: %v", f.cmds)
	}
	snap, err := LoadSnapshot(f.dir)
	if err != nil || len(snap.Items) != 1 || snap.Items[0].Owner != OwnerYou {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	kinds := ""
	for _, e := range RecentEvents(f.dir, 10) {
		kinds += e.Kind + ";"
	}
	if kinds != "woke agent;fixed;" {
		t.Fatalf("events %q", kinds)
	}
	if _, err := os.Stat(boardPath(f.dir)); err != nil {
		t.Fatal("board.md not written")
	}
}

func TestPollLeavesOpenSessionAlone(t *testing.T) {
	f := newFake(t, []ghpkg.SupervisedPR{withRollup(openPR(1), "FAILURE")}, true)
	if err := f.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 || f.waker.Running(prKey("o/r", 1)) != nil {
		t.Fatalf("acted under an open session: %v", f.cmds)
	}
}

func TestPollUpdatesBehindBranchOnce(t *testing.T) {
	pr := withRollup(openPR(1), "SUCCESS")
	pr.ReviewDecision = "APPROVED"
	f := newFake(t, []ghpkg.SupervisedPR{pr}, false)
	for range 2 {
		if err := f.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.cmds, "\n") != "gh pr update-branch 1 --repo o/r" {
		t.Fatalf("cmds: %v", f.cmds)
	}
}

func TestPollBacksOffAndKeepsLastItems(t *testing.T) {
	f := newFake(t, []ghpkg.SupervisedPR{withRollup(openPR(1), "SUCCESS")}, false)
	if err := f.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.deps.fetch = func(context.Context, time.Time) (string, []ghpkg.SupervisedPR, error) {
		calls++
		return "", nil, errors.New("HTTP 502")
	}
	if err := f.Poll(context.Background()); err == nil {
		t.Fatal("want the fetch error")
	}
	_ = f.Poll(context.Background()) // inside the backoff: no fetch
	if calls != 1 {
		t.Fatalf("fetched %d times during backoff", calls)
	}
	snap, _ := LoadSnapshot(f.dir)
	if len(snap.Items) != 1 || !strings.Contains(snap.Error, "HTTP 502") {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestPollDropsLandedApprovals(t *testing.T) {
	pr := openPR(1)
	pr.State, pr.MergedAt = "MERGED", now.Format(time.RFC3339)
	f := newFake(t, []ghpkg.SupervisedPR{pr, withRollup(openPR(2), "SUCCESS")}, false)
	if err := SaveLand(f.land, []LandEntry{{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2}}); err != nil {
		t.Fatal(err)
	}
	var watched []LandEntry
	f.deps.landWatch = func(_ context.Context, e []LandEntry) (string, error) {
		watched = e
		return "o/r#1 url=u state=MERGED landed=yes\no/r#1 stage=staging status=success", nil
	}
	if err := f.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(watched) != 2 {
		t.Fatalf("land-watch got %+v", watched)
	}
	left, _ := LoadLand(f.land)
	if len(left) != 1 || left[0].Number != 2 {
		t.Fatalf("land list %+v", left)
	}
}

func TestPollDoesNotWaitOnSlowLandWatch(t *testing.T) {
	defer func(w time.Duration) { landWatchWait = w }(landWatchWait)
	landWatchWait = 10 * time.Millisecond
	pr := openPR(1)
	pr.State, pr.MergedAt = "MERGED", now.Format(time.RFC3339)
	f := newFake(t, []ghpkg.SupervisedPR{pr}, false)
	release := make(chan struct{})
	var runs atomic.Int32
	f.deps.landWatch = func(context.Context, []LandEntry) (string, error) {
		runs.Add(1)
		<-release
		return "o/r#1 url=u state=MERGED landed=yes", nil
	}
	if err := f.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.Poll(context.Background()); err != nil { // run still in flight: no second run
		t.Fatal(err)
	}
	if snap, _ := LoadSnapshot(f.dir); snap.Items[0].Owner == OwnerLanded {
		t.Fatal("landed before land-watch finished")
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("runs = %d while one was in flight, want 1", n)
	}
	close(release)
	waitFor(t, func() bool {
		if err := f.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		snap, _ := LoadSnapshot(f.dir)
		return snap.Items[0].Owner == OwnerLanded
	})
}
