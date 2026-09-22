package reconciler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/mgreau/zen/internal/config"
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

	// Worktree path doesn't exist, so removeWorktree should be a no-op
	err := rec.Reconcile(context.Background(), "testrepo:999", workqueue.Options{})
	if err != nil {
		t.Fatalf("unexpected error for missing worktree: %v", err)
	}
}

func TestCleanupReconcile_DirtyWorktreeIsTerminalSkip(t *testing.T) {
	basePath := t.TempDir()
	originPath := filepath.Join(basePath, "testrepo")
	worktreePath := filepath.Join(basePath, "testrepo-pr-999")
	runCleanupGit(t, basePath, "init", "-b", "main", originPath)
	runCleanupGit(t, originPath, "config", "user.email", "test@example.com")
	runCleanupGit(t, originPath, "config", "user.name", "Test")
	runCleanupGit(t, originPath, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(originPath, "tracked"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, originPath, "add", "tracked")
	runCleanupGit(t, originPath, "commit", "-m", "initial")
	runCleanupGit(t, originPath, "worktree", "add", "-b", "pr-999", worktreePath)
	if err := os.WriteFile(filepath.Join(worktreePath, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repos: map[string]config.RepoConfig{
		"testrepo": {FullName: "test/testrepo", BasePath: basePath},
	}}
	err := NewCleanupReconciler(cfg).Reconcile(context.Background(), "testrepo:999", workqueue.Options{})
	if err != nil {
		t.Fatalf("dirty worktree should be a terminal skip: %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatal("dirty worktree was removed")
	}
}

func runCleanupGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
