package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/mgreau/zen/internal/config"
	"github.com/mgreau/zen/internal/ui"
)

// GitMu serializes git worktree operations to prevent concurrent index.lock conflicts.
var GitMu sync.Mutex

// CleanStaleLocks removes stale index.lock files from worktrees of the given repo.
// A lock is stale once nothing has written it for staleLockAge.
func CleanStaleLocks(cfg *config.Config, repo string) {
	basePath := cfg.RepoBasePath(repo)
	if basePath == "" {
		return
	}

	gitDir := filepath.Join(basePath, repo, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return
	}

	worktreesDir := filepath.Join(gitDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		lockFile := filepath.Join(worktreesDir, entry.Name(), "index.lock")
		RemoveStaleLock(lockFile, entry.Name())
	}

	// Also check the main repo's own index.lock
	mainLock := filepath.Join(gitDir, "index.lock")
	RemoveStaleLock(mainLock, repo)
}

// CleanAllStaleLocks cleans stale locks across all known repos.
func CleanAllStaleLocks(cfg *config.Config) {
	for _, repo := range cfg.RepoNames() {
		CleanStaleLocks(cfg, repo)
	}
}

// CleanupFailedAdd cleans up after a failed "git worktree add" that may have
// created the branch and/or a partial worktree directory but failed to complete
// (e.g., "Could not write new index file"). It removes the partial worktree
// directory, prunes git's worktree metadata, and deletes the orphaned branch.
//
// originPath is the main repo directory, worktreePath is the target worktree
// directory, and branch is the git branch that was being created.
func CleanupFailedAdd(originPath, worktreePath, branch string) {
	// Remove partial worktree directory if it exists
	if _, err := os.Stat(worktreePath); err == nil {
		os.RemoveAll(worktreePath)
	}

	// Prune stale worktree metadata
	pruneCmd := execCommand("git", "worktree", "prune")
	pruneCmd.Dir = originPath
	pruneCmd.CombinedOutput()

	// Delete the orphaned branch
	delCmd := execCommand("git", "branch", "-D", branch)
	delCmd.Dir = originPath
	delCmd.CombinedOutput()
}

// execCommand is a variable for testing.
var execCommand = exec.Command

// staleLockAge is how long an index.lock must go unmodified before it is
// stale. Git writes no PID into index.lock: the file is the new index, and a
// checkout of a large repository holds it for minutes before its last write.
const staleLockAge = 30 * time.Minute

// RemoveStaleLock removes an index.lock file only if nothing has written it
// for staleLockAge. Safe to call if the file does not exist.
func RemoveStaleLock(lockFile, name string) {
	info, err := os.Stat(lockFile)
	if err != nil {
		return // file doesn't exist or can't be read
	}
	if time.Since(info.ModTime()) < staleLockAge || openByAProcess(lockFile) {
		return // a git process may still be writing it
	}
	// Another scan may have removed it and git taken a new lock since.
	if now, err := os.Stat(lockFile); err != nil || !os.SameFile(info, now) {
		return
	}

	ui.LogWarn(fmt.Sprintf("Removing stale index.lock for worktree: %s", name))
	os.Remove(lockFile)
}

// openByAProcess reports whether any process has path open, read from
// /proc. Where /proc is absent it reports false.
func openByAProcess(path string) bool {
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		if target, err := os.Readlink(fd); err == nil && target == path {
			return true
		}
	}
	return false
}
