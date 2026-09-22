package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mgreau/zen/internal/agent"
)

func removalFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "repo")
	path := filepath.Join(root, "repo-feature")
	runRemovalGit(t, root, "init", "-b", "main", origin)
	runRemovalGit(t, origin, "config", "user.email", "test@example.com")
	runRemovalGit(t, origin, "config", "user.name", "Test")
	runRemovalGit(t, origin, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(origin, "tracked"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRemovalGit(t, origin, "add", "tracked")
	runRemovalGit(t, origin, "commit", "-m", "initial")
	runRemovalGit(t, origin, "worktree", "add", "-b", "feature", path)
	return origin, path
}

func TestRemove(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		wantErr error
	}{
		{name: "clean"},
		{name: "tracked change", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "tracked", "changed")
		}, wantErr: ErrWorktreeDirty},
		{name: "staged change", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "staged", "new")
			runRemovalGit(t, path, "add", "staged")
		}, wantErr: ErrWorktreeDirty},
		{name: "untracked file", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "notes.txt", "keep me")
		}, wantErr: ErrWorktreeDirty},
		{name: "ignored file", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, ".gitignore", "ignored.log\n")
			runRemovalGit(t, path, "add", ".gitignore")
			runRemovalGit(t, path, "commit", "-m", "ignore generated log")
			writeRemovalFile(t, path, "ignored.log", "keep me")
		}, wantErr: ErrWorktreeDirty},
		{name: "claude context", prepare: func(t *testing.T, path string) {
			if _, err := agent.New(agent.Claude, "").InjectContext(path, "generated"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "codex context", prepare: func(t *testing.T, path string) {
			if _, err := agent.New(agent.Codex, "").InjectContext(path, "generated"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "claude file without sentinel", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "CLAUDE.local.md", "user owned")
		}, wantErr: ErrWorktreeDirty},
		{name: "agents file without sentinel", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "AGENTS.md", "user owned")
		}, wantErr: ErrWorktreeDirty},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			origin, path := removalFixture(t)
			if test.prepare != nil {
				test.prepare(t, path)
			}
			err := remove(origin, path, func(string) bool { return false })
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Remove() error = %v, want %v", err, test.wantErr)
			}
			_, statErr := os.Stat(path)
			if test.wantErr != nil && statErr != nil {
				t.Fatal("refused removal removed the worktree")
			}
			if test.wantErr == nil && !os.IsNotExist(statErr) {
				t.Fatal("successful removal left the worktree")
			}
		})
	}
}

func TestRemoveGitFailurePreservesWorktree(t *testing.T) {
	_, path := removalFixture(t)
	if _, err := agent.New(agent.Claude, "").InjectContext(path, "generated"); err != nil {
		t.Fatal(err)
	}
	err := remove(filepath.Join(t.TempDir(), "not-a-repository"), path, func(string) bool { return false })
	if err == nil || RemovalBlocked(err) {
		t.Fatalf("Remove() error = %v, want Git failure", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Git failure removed the worktree")
	}
	for _, name := range []string{"CLAUDE.local.md", ".zen/.claude_context_injected"} {
		if _, err := os.Stat(filepath.Join(path, filepath.FromSlash(name))); err != nil {
			t.Errorf("Git failure did not restore %s: %v", name, err)
		}
	}
}

func TestRemoveMissingIsIdempotent(t *testing.T) {
	if err := Remove(t.TempDir(), filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesRunningAgent(t *testing.T) {
	origin, path := removalFixture(t)
	err := remove(origin, path, func(string) bool { return true })
	if !errors.Is(err, ErrWorktreeActive) {
		t.Fatalf("Remove() error = %v, want %v", err, ErrWorktreeActive)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("active-agent refusal removed the worktree")
	}
}

func writeRemovalFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runRemovalGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
