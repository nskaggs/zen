package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgreau/zen/internal/agent"
	"github.com/mgreau/zen/internal/config"
	ghpkg "github.com/mgreau/zen/internal/github"
	"github.com/mgreau/zen/internal/worktree"
)

// mergedWindow is how far back merged PRs are read, so deploys of PRs merged
// late yesterday stay visible until they land.
const mergedWindow = 24 * time.Hour

// maxBackoff caps the wait after repeated GitHub failures.
const maxBackoff = 30 * time.Minute

// execFunc runs a command in dir and returns its stdout.
type execFunc func(ctx context.Context, dir, name string, args ...string) (string, error)

// landWatchFunc runs land-watch --arm over entries and returns its stdout.
type landWatchFunc func(ctx context.Context, entries []LandEntry) (string, error)

// deps is every way the supervisor touches the outside world, so tests can
// replace each one.
type deps struct {
	fetch      func(ctx context.Context, since time.Time) (string, []ghpkg.SupervisedPR, error)
	checks     func(ctx context.Context, repo string, number int) ([]ghpkg.PRCheck, error)
	mergeState func(ctx context.Context, repo string, number int) (string, error)
	worktrees  func() ([]worktree.Worktree, error)
	running    func(path string) bool
	exec       execFunc
	landWatch  landWatchFunc
	now        func() time.Time
}

// Supervisor is the PR supervisor. Poll is safe to call from one goroutine at
// a time; wakes run in the background between polls.
type Supervisor struct {
	cfg   *config.Config
	dir   string
	land  string
	deps  deps
	waker *Waker

	mu          sync.Mutex
	failures    int
	nextAllowed time.Time
	last        Snapshot

	// land-watch runs in the background: deploy tracking for a day of merged
	// PRs takes minutes, which must not hold up the rest of a poll.
	lwMu    sync.Mutex
	lwDone  chan struct{} // non-nil while a run is in flight
	lwOut   string
	lwErr   error
	lwFresh bool // lwErr is from a run no poll has reported yet
}

// landWatchWait is how long a poll waits for land-watch before using the
// previous run's result.
var landWatchWait = 20 * time.Second

// New builds a supervisor wired to GitHub, git, land-watch, and the
// configured agent.
func New(cfg *config.Config) *Supervisor {
	d := deps{
		fetch:      ghpkg.GetSupervisedPRs,
		checks:     ghpkg.GetPRChecks,
		mergeState: ghpkg.GetMergeState,
		worktrees:  func() ([]worktree.Worktree, error) { return worktree.ListAll(cfg) },
		running:    agent.RunningIn,
		exec:       runCommand,
		landWatch:  runLandWatch(Dir()),
		now:        time.Now,
	}
	sc := cfg.Supervisor
	return &Supervisor{
		cfg:   cfg,
		dir:   Dir(),
		land:  LandFile(),
		deps:  d,
		waker: NewWaker(sc.GetMaxWakes(), sc.WakeTimeoutDuration(), headlessCommand(cfg.NewAgent(""), runCommand)),
	}
}

// Poll runs one supervision pass.
func (s *Supervisor) Poll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.deps.now()
	memory := loadMemory(s.dir)
	s.drainWakes(memory, now)

	interval := s.cfg.Supervisor.PollIntervalDuration()
	if now.Before(s.nextAllowed) {
		return saveMemory(s.dir, memory)
	}
	login, prs, err := s.deps.fetch(ctx, now.Add(-mergedWindow))
	if err != nil {
		s.failures++
		wait := interval << min(s.failures, 6)
		if wait > maxBackoff {
			wait = maxBackoff
		}
		s.nextAllowed = now.Add(wait)
		snap := s.last
		snap.Error = fmt.Sprintf("GitHub read failed (retry in %s): %v", shortDuration(wait), err)
		snap.NextPoll = s.nextAllowed
		s.write(snap, memory, now)
		return err
	}
	s.failures = 0
	s.nextAllowed = time.Time{}

	landList, err := LoadLand(s.land)
	if err != nil {
		return err
	}
	byBranch := s.featureWorktrees()

	// One land-watch run covers every land-approved open PR (it arms those
	// with approvals) and every recently merged PR (deploy state).
	var watch []LandEntry
	for _, pr := range prs {
		full := pr.Repository.NameWithOwner
		if _, ok := s.cfg.ConfiguredRepo(full); !ok {
			continue
		}
		if pr.State == "MERGED" || landIndex(landList, full, pr.Number) >= 0 {
			watch = append(watch, LandEntry{Repo: full, Number: pr.Number})
		}
	}
	lw := s.landWatchResult(ctx, watch, now)

	var items []Item
	for _, pr := range prs {
		full := pr.Repository.NameWithOwner
		repo, ok := s.cfg.ConfiguredRepo(full)
		if !ok {
			continue
		}
		key := prKey(full, pr.Number)
		o := Observed{
			PR:     pr,
			Repo:   repo,
			Self:   login,
			Land:   landIndex(landList, full, pr.Number) >= 0,
			Wake:   s.waker.Running(key),
			Memory: memory[key],
		}
		if pr.State == "OPEN" {
			o.Worktree = byBranch[repo+"\x00"+pr.HeadRefName]
			if o.Worktree != "" {
				o.SessionOpen = s.deps.running(o.Worktree)
			}
			if st := pr.RollupState(); st == "FAILURE" || st == "ERROR" {
				if checks, err := s.deps.checks(ctx, full, pr.Number); err == nil {
					o.Checks = checks
				}
			}
			// Behind only matters once nothing else is in the way.
			if pr.ReviewDecision == "APPROVED" && !pr.IsDraft && pr.Mergeable != "CONFLICTING" {
				if ms, err := s.deps.mergeState(ctx, full, pr.Number); err == nil {
					o.PR.MergeStateStatus = ms
				}
			}
		} else if l, ok := lw[key]; ok {
			d := l.deploy()
			o.Deploy = &d
		}
		o.SettleAfter = s.cfg.Watch.CleanupAfterMergeDuration()
		if l, ok := lw[key]; ok && pr.State == "OPEN" {
			s.noteArm(key, pr, l, now)
		}

		item, actions := Derive(o, now)
		items = append(items, item)
		mem := memory[key]
		if pr.State == "OPEN" && !FindIssues(o).Judgment() {
			mem.FixingSince, mem.Stuck = time.Time{}, ""
		}
		for _, a := range actions {
			mem = s.act(ctx, a, o, mem, now)
		}
		memory[key] = mem
	}

	// A landed PR leaves the land list; its approval is spent.
	kept := landList[:0]
	for _, e := range landList {
		landed := false
		for _, it := range items {
			if it.FullRepo == e.Repo && it.Number == e.Number && it.Owner == OwnerLanded {
				landed = true
			}
		}
		if !landed {
			kept = append(kept, e)
		}
	}
	if len(kept) != len(landList) {
		if err := SaveLand(s.land, kept); err != nil {
			return err
		}
	}

	snap := Snapshot{UpdatedAt: now, NextPoll: now.Add(interval), Items: items}
	s.last = snap
	s.write(snap, memory, now)
	return nil
}

func (s *Supervisor) write(snap Snapshot, memory map[string]PRMemory, now time.Time) {
	_ = saveSnapshot(s.dir, snap)
	_ = writeAtomic(boardPath(s.dir), []byte(BoardMarkdown(snap, RecentEvents(s.dir, 10), now)))
	_ = saveMemory(s.dir, memory)
}

func (s *Supervisor) event(e Event) { _ = appendEvent(s.dir, e) }

// drainWakes records finished wakes.
func (s *Supervisor) drainWakes(memory map[string]PRMemory, now time.Time) {
	for {
		select {
		case r := <-s.waker.Results():
			mem := memory[r.PR]
			mem.HandledKey, mem.Waking = r.Key, false
			kind := "fixed"
			if r.Fixed {
				mem.Stuck = ""
			} else {
				mem.Stuck = r.Reason
				kind = "stuck"
			}
			memory[r.PR] = mem
			s.event(Event{Time: now, PR: r.PR, Kind: kind, Detail: truncate(r.Reason, 160)})
		default:
			return
		}
	}
}

// noteArm records auto-merge being armed, or failing to arm, once.
func (s *Supervisor) noteArm(key string, pr ghpkg.SupervisedPR, l landLine, now time.Time) {
	if l.armed && pr.AutoMergeRequest == nil {
		s.event(Event{Time: now, PR: key, Kind: "armed auto-merge"})
	}
	if l.armError != "" {
		s.event(Event{Time: now, PR: key, Kind: "arm failed", Detail: truncate(l.armError, 160)})
	}
}

// act takes one action and returns the updated memory.
func (s *Supervisor) act(ctx context.Context, a Action, o Observed, mem PRMemory, now time.Time) PRMemory {
	pr := o.PR
	full := pr.Repository.NameWithOwner
	key := prKey(full, pr.Number)
	n := strconv.Itoa(pr.Number)
	switch a.Kind {
	case ActionUpdateBranch:
		mem.UpdateHead = pr.HeadRefOid
		_, err := s.deps.exec(ctx, "", "gh", "pr", "update-branch", n, "--repo", full)
		s.event(resultEvent(now, key, "updated branch", err))
	case ActionRerun:
		mem.RerunHead = pr.HeadRefOid
		for _, id := range a.Runs {
			_, err := s.deps.exec(ctx, "", "gh", "run", "rerun", id, "--failed", "--repo", full)
			s.event(resultEvent(now, key, "reran run "+id, err))
		}
	case ActionWake:
		is := FindIssues(o)
		in := BriefInput{URL: pr.URL, Head: pr.HeadRefOid, Base: pr.BaseRefName, Issues: is}
		if id := failingRunID(o.Checks); id != "" {
			in.Log, _ = s.deps.exec(ctx, "", "gh", "run", "view", id, "--repo", full, "--log-failed")
		}
		in.Diffstat, _ = s.deps.exec(ctx, o.Worktree, "git", "diff", "--stat", "origin/"+pr.BaseRefName+"...HEAD")
		if s.waker.Start(ctx, key, a.Key, is.Describe(), o.Worktree, BuildBrief(in)) {
			mem.HandledKey, mem.HandledHead, mem.Stuck, mem.Waking = a.Key, pr.HeadRefOid, "", true
			if mem.FixingSince.IsZero() {
				mem.FixingSince = now
			}
			s.event(Event{Time: now, PR: key, Kind: "woke agent", Detail: is.Describe()})
		}
	}
	return mem
}

func resultEvent(now time.Time, pr, kind string, err error) Event {
	e := Event{Time: now, PR: pr, Kind: kind}
	if err != nil {
		e.Kind += " failed"
		e.Detail = truncate(err.Error(), 160)
	}
	return e
}

// featureWorktrees maps "repo\x00branch" to the feature worktree path.
func (s *Supervisor) featureWorktrees() map[string]string {
	m := map[string]string{}
	wts, err := s.deps.worktrees()
	if err != nil {
		return m
	}
	for _, wt := range wts {
		if wt.Type == worktree.TypeFeature && wt.Branch != "" {
			m[wt.Repo+"\x00"+wt.Branch] = wt.Path
		}
	}
	return m
}

// landLine is land-watch's output for one PR.
type landLine struct {
	merged   bool
	landed   bool
	armed    bool
	armError string
	stages   []string // "name=status"
}

// deploy summarizes a merged PR's stages.
func (l landLine) deploy() Deploy {
	d := Deploy{Landed: l.landed, Class: "merged_ok"}
	none := len(l.stages) > 0
	for _, st := range l.stages {
		name, status, _ := strings.Cut(st, "=")
		if status != "none" {
			none = false
		}
		switch status {
		case "failure":
			d.Class, d.Stage = "merged_failed", name
		case "pending":
			if d.Class != "merged_failed" {
				d.Class, d.Stage = "pending_deploy", name+" pending"
			}
		case "waiting":
			if d.Class == "merged_ok" {
				d.Class, d.Stage = "waiting_release", name+" waiting"
			}
		}
	}
	d.None = none
	return d
}

// parseLandWatchAll reads land-watch's per-PR summary and stage lines.
func parseLandWatchAll(out string) map[string]landLine {
	m := map[string]landLine{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(fields[0], "#") {
			continue
		}
		key := fields[0]
		l := m[key]
		if strings.HasPrefix(fields[1], "stage=") {
			name := strings.TrimPrefix(fields[1], "stage=")
			for _, f := range fields[2:] {
				if v, ok := strings.CutPrefix(f, "status="); ok {
					l.stages = append(l.stages, name+"="+v)
					break
				}
			}
			m[key] = l
			continue
		}
		for _, f := range fields[1:] {
			switch {
			case f == "state=MERGED":
				l.merged = true
			case f == "landed=yes":
				l.landed = true
			case f == "armed=true":
				l.armed = true
			case strings.HasPrefix(f, "arm_error="):
				l.armError = strings.TrimPrefix(f, "arm_error=")
			}
		}
		m[key] = l
	}
	return m
}

// runLandWatch runs land-watch --arm with a persistent cache in dir. Exit
// status 1 only means a deploy stage failed; its output is still used.
// landWatchResult starts a land-watch run over watch unless one is already in
// flight, waits up to landWatchWait for it, and returns the newest finished
// run's lines. A slow run is picked up by a later poll.
func (s *Supervisor) landWatchResult(ctx context.Context, watch []LandEntry, now time.Time) map[string]landLine {
	if len(watch) == 0 {
		return map[string]landLine{}
	}
	s.lwMu.Lock()
	if s.lwDone == nil {
		done := make(chan struct{})
		s.lwDone = done
		go func() {
			out, err := s.deps.landWatch(ctx, watch)
			s.lwMu.Lock()
			s.lwOut, s.lwErr, s.lwFresh, s.lwDone = out, err, true, nil
			s.lwMu.Unlock()
			close(done)
		}()
	}
	done := s.lwDone
	s.lwMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(landWatchWait):
		case <-ctx.Done():
		}
	}
	s.lwMu.Lock()
	defer s.lwMu.Unlock()
	if s.lwFresh && s.lwErr != nil {
		s.event(Event{Time: now, PR: "land-watch", Kind: "error", Detail: truncate(s.lwErr.Error(), 160)})
	}
	s.lwFresh = false
	return parseLandWatchAll(s.lwOut)
}

func runLandWatch(dir string) landWatchFunc {
	return func(ctx context.Context, entries []LandEntry) (string, error) {
		bin, err := exec.LookPath("land-watch")
		if err != nil {
			return "", errors.New("land-watch is not installed")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		type prsEntry struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		}
		list := make([]prsEntry, len(entries))
		for i, e := range entries {
			list[i] = prsEntry{Repo: e.Repo, Number: e.Number}
		}
		data, err := json.Marshal(list)
		if err != nil {
			return "", err
		}
		prsFile := filepath.Join(dir, "land-watch-prs.json")
		if err := writeAtomic(prsFile, data); err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "--arm", "--cache", filepath.Join(dir, "land-watch-cache.json"), prsFile)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
			return stdout.String(), fmt.Errorf("land-watch: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
}

// runCommand runs name in dir and returns stdout; a failure carries stderr.
func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, truncate(stderr.String(), 300))
	}
	return stdout.String(), nil
}
