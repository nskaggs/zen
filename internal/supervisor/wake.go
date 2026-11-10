package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mgreau/zen/internal/agent"
)

// WakeRun is one agent wake for a PR.
type WakeRun struct {
	Key     string
	Summary string // the issues it was woken for
	Queued  bool   // waiting for a free slot
	Started time.Time
}

// Describe is the item summary while the wake is queued or running.
func (w *WakeRun) Describe(now time.Time) string {
	if w.Queued {
		return "queued: " + w.Summary
	}
	return fmt.Sprintf("fixing %s · %s", w.Summary, shortDuration(now.Sub(w.Started)))
}

// WakeResult is how a wake ended.
type WakeResult struct {
	PR     string
	Key    string
	Fixed  bool
	Reason string // the agent's final line, or why it failed
}

// agentCommand runs an agent headlessly in dir with prompt and returns its
// final message. Tests replace it.
type agentCommand func(ctx context.Context, dir, prompt string) (string, error)

// Waker runs agent wakes: at most one per PR, at most max at once, each
// bounded by timeout. A wake that does not get a slot waits in a queue.
type Waker struct {
	mu      sync.Mutex
	max     int
	timeout time.Duration
	run     agentCommand
	active  map[string]*WakeRun
	slots   chan struct{}
	results chan WakeResult
}

// NewWaker builds a Waker over run.
func NewWaker(max int, timeout time.Duration, run agentCommand) *Waker {
	if max < 1 {
		max = 1
	}
	return &Waker{
		max:     max,
		timeout: timeout,
		run:     run,
		active:  map[string]*WakeRun{},
		slots:   make(chan struct{}, max),
		results: make(chan WakeResult, 64),
	}
}

// Running returns a copy of the wake for pr, or nil.
func (w *Waker) Running(pr string) *WakeRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r, ok := w.active[pr]; ok {
		c := *r
		return &c
	}
	return nil
}

// Start queues a wake for pr unless one is already active for it. It reports
// whether a wake was started.
func (w *Waker) Start(ctx context.Context, pr, key, summary, dir, prompt string) bool {
	w.mu.Lock()
	if _, ok := w.active[pr]; ok {
		w.mu.Unlock()
		return false
	}
	run := &WakeRun{Key: key, Summary: summary, Queued: true}
	w.active[pr] = run
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.active, pr)
			w.mu.Unlock()
		}()
		select {
		case w.slots <- struct{}{}:
		case <-ctx.Done():
			w.results <- WakeResult{PR: pr, Key: key, Reason: "supervisor stopped before the wake started"}
			return
		}
		defer func() { <-w.slots }()
		w.mu.Lock()
		run.Queued = false
		run.Started = time.Now()
		w.mu.Unlock()

		wctx, cancel := context.WithTimeout(ctx, w.timeout)
		defer cancel()
		out, err := w.run(wctx, dir, prompt)
		w.results <- interpret(pr, key, out, err, wctx.Err(), w.timeout)
	}()
	return true
}

// Results delivers finished wakes.
func (w *Waker) Results() <-chan WakeResult { return w.results }

// interpret turns an agent's final message into a result. The brief asks the
// agent to end with "FIXED <summary>" or "STUCK <reason>"; anything else, a
// timeout, or an error is treated as stuck so the user sees it.
func interpret(pr, key, out string, err, ctxErr error, timeout time.Duration) WakeResult {
	res := WakeResult{PR: pr, Key: key}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		res.Reason = "wake timed out after " + timeout.String()
		return res
	}
	last := lastLine(out)
	switch {
	case strings.HasPrefix(last, "FIXED"):
		res.Fixed = true
		res.Reason = strings.TrimSpace(strings.TrimPrefix(last, "FIXED"))
	case strings.HasPrefix(last, "STUCK"):
		res.Reason = strings.TrimSpace(strings.TrimPrefix(last, "STUCK"))
	case err != nil:
		res.Reason = "agent failed: " + truncate(err.Error(), 120)
	default:
		res.Reason = "agent ended without FIXED or STUCK: " + truncate(last, 100)
	}
	return res
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// headlessCommand builds the agentCommand for the configured agent. It
// resumes the worktree's newest recorded session so the agent keeps the
// task's context, or starts a fresh one when the worktree has none.
func headlessCommand(a agent.Agent, exec execFunc) agentCommand {
	return func(ctx context.Context, dir, prompt string) (string, error) {
		session := ""
		if sessions, err := a.FindSessions(dir); err == nil && len(sessions) > 0 {
			session = sessions[0].ID
		}
		switch a.Kind() {
		case agent.Codex:
			outFile, err := os.CreateTemp("", "zen-wake-*.txt")
			if err != nil {
				return "", err
			}
			outFile.Close()
			defer os.Remove(outFile.Name())
			args := []string{"exec"}
			if session != "" {
				args = append(args, "resume", "-o", outFile.Name(), session, prompt)
			} else {
				args = append(args, "-o", outFile.Name(), prompt)
			}
			_, runErr := exec(ctx, dir, a.Bin(), args...)
			last, _ := os.ReadFile(filepath.Clean(outFile.Name()))
			return string(last), runErr
		default:
			args := []string{"-p"}
			if session != "" {
				args = append(args, "--resume", session)
			}
			args = append(args, prompt)
			return exec(ctx, dir, a.Bin(), args...)
		}
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
