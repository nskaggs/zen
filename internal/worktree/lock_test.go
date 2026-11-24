package worktree

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveStaleLockKeepsALockInUse(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "index.lock")
	// Git's index.lock holds index data, not a PID.
	if err := os.WriteFile(lock, []byte("DIRC\x00\x00\x00\x02"), 0o644); err != nil {
		t.Fatal(err)
	}
	RemoveStaleLock(lock, "wt")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a fresh lock was removed: %v", err)
	}

	old := time.Now().Add(-staleLockAge - time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	// An old lock a process still holds is kept.
	f, err := os.Open(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/proc/self/fd"); err == nil {
		RemoveStaleLock(lock, "wt")
		if _, err := os.Stat(lock); err != nil {
			t.Fatalf("a held lock was removed: %v", err)
		}
	}
	f.Close()
	RemoveStaleLock(lock, "wt")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("a stale lock was kept: %v", err)
	}
}
