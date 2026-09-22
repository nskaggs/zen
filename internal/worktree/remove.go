package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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

	owned := make(map[string]struct{})
	for _, path := range agent.OwnedContextFiles(worktreePath) {
		owned[filepath.ToSlash(path)] = struct{}{}
	}

	dirty, err := removalDirty(worktreePath, owned)
	if err != nil {
		return err
	}
	if dirty {
		return ErrWorktreeDirty
	}

	snapshots, err := removeOwnedContext(worktreePath, owned)
	if err != nil {
		return err
	}

	cmd := exec.Command("git", "worktree", "remove", worktreePath)
	cmd.Dir = originPath
	if out, err := cmd.CombinedOutput(); err != nil {
		if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
			if restoreErr := restoreOwnedContext(snapshots); restoreErr != nil {
				return fmt.Errorf("git worktree remove: %w: %s; restore generated context: %v", err, strings.TrimSpace(string(out)), restoreErr)
			}
		}
		return fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removalDirty(worktreePath string, owned map[string]struct{}) (bool, error) {
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
			if path == "" {
				continue
			}
			if _, ok := owned[filepath.ToSlash(path)]; !ok {
				return true, nil
			}
		}
	}
	return false, nil
}

type contextSnapshot struct {
	path string
	data []byte
	mode fs.FileMode
}

func removeOwnedContext(worktreePath string, owned map[string]struct{}) ([]contextSnapshot, error) {
	var snapshots []contextSnapshot
	for path := range owned {
		tracked, err := trackedPath(worktreePath, path)
		if err != nil {
			return nil, err
		}
		if tracked {
			continue
		}
		fullPath := filepath.Join(worktreePath, filepath.FromSlash(path))
		info, err := os.Stat(fullPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect generated context %s: %w", path, err)
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("read generated context %s: %w", path, err)
		}
		snapshots = append(snapshots, contextSnapshot{path: fullPath, data: data, mode: info.Mode()})
	}
	for i, snapshot := range snapshots {
		if err := os.Remove(snapshot.path); err != nil {
			removeErr := fmt.Errorf("remove generated context %s: %w", filepath.Base(snapshot.path), err)
			if restoreErr := restoreOwnedContext(snapshots[:i]); restoreErr != nil {
				return nil, fmt.Errorf("%w; restore generated context: %v", removeErr, restoreErr)
			}
			return nil, removeErr
		}
	}
	// Remove the generated directory only when no user-owned files remain.
	_ = os.Remove(filepath.Join(worktreePath, ".zen"))
	return snapshots, nil
}

func restoreOwnedContext(snapshots []contextSnapshot) error {
	for _, snapshot := range snapshots {
		if err := os.MkdirAll(filepath.Dir(snapshot.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(snapshot.path, snapshot.data, snapshot.mode.Perm()); err != nil {
			return err
		}
	}
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
