package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mgreau/zen/internal/agent"
)

var (
	// ErrWorktreeDirty means removal would discard local file changes.
	ErrWorktreeDirty = errors.New("refusing to remove worktree with local changes")
	// ErrWorktreeActive means a supported coding agent is running in the worktree.
	ErrWorktreeActive = errors.New("refusing to remove worktree with a running agent")
)

// RemovalBlocked reports whether err is an expected safety refusal rather than
// a Git or filesystem failure.
func RemovalBlocked(err error) bool {
	return errors.Is(err, ErrWorktreeDirty) || errors.Is(err, ErrWorktreeActive)
}

// Remove removes a linked worktree only when it is inactive and contains no
// local changes. Zen-generated review context is reproducible and does not make
// a worktree dirty. A missing worktree is already removed and succeeds.
func Remove(originPath, worktreePath string) error {
	return remove(originPath, worktreePath, agent.RunningIn)
}

func remove(originPath, worktreePath string, runningIn func(string) bool) error {
	GitMu.Lock()
	defer GitMu.Unlock()

	if _, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect worktree: %w", err)
	}
	if runningIn(worktreePath) {
		return ErrWorktreeActive
	}

	dirty, err := removalDirty(worktreePath)
	if err != nil {
		return err
	}
	if dirty {
		return ErrWorktreeDirty
	}

	if err := removeOwnedContext(worktreePath); err != nil {
		return err
	}

	cmd := exec.Command("git", "worktree", "remove", worktreePath)
	cmd.Dir = originPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removalDirty(worktreePath string) (bool, error) {
	dirty, err := TrackedDirty(worktreePath)
	if err != nil || dirty {
		return dirty, err
	}

	for _, args := range [][]string{
		{"ls-files", "--others", "--exclude-standard", "-z"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "-z"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = worktreePath
		out, err := cmd.Output()
		if err != nil {
			return false, fmt.Errorf("inspect untracked files: %w", err)
		}
		for _, raw := range bytes.Split(out, []byte{0}) {
			path := string(raw)
			if path != "" && !zenOwnedContext(worktreePath, filepath.ToSlash(path)) {
				return true, nil
			}
		}
	}
	return false, nil
}

func removeOwnedContext(worktreePath string) error {
	paths := []string{"CLAUDE.local.md", ".zen/PR_CONTEXT.md", ".zen/.pr_context_injected"}
	if zenOwnedContext(worktreePath, "AGENTS.md") {
		paths = append(paths, "AGENTS.md")
	}
	for _, path := range paths {
		tracked, err := trackedPath(worktreePath, path)
		if err != nil {
			return err
		}
		if tracked {
			continue
		}
		if err := os.Remove(filepath.Join(worktreePath, filepath.FromSlash(path))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove generated context %s: %w", path, err)
		}
	}
	// Remove the generated directory only when no user-owned files remain.
	_ = os.Remove(filepath.Join(worktreePath, ".zen"))
	return nil
}

func trackedPath(worktreePath, path string) (bool, error) {
	cmd := exec.Command("git", "ls-files", "--error-unmatch", "--", path)
	cmd.Dir = worktreePath
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("inspect context path %s: %w", path, err)
	}
	return true, nil
}

func zenOwnedContext(worktreePath, path string) bool {
	switch path {
	case "CLAUDE.local.md", ".zen/PR_CONTEXT.md", ".zen/.pr_context_injected":
		return true
	case "AGENTS.md":
		_, err := os.Stat(filepath.Join(worktreePath, ".zen", ".pr_context_injected"))
		return err == nil
	default:
		return false
	}
}
