package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestClaudeInjectContextMarksOwnedFiles(t *testing.T) {
	worktreePath := t.TempDir()
	ag := New(Claude, "")

	if _, err := ag.InjectContext(worktreePath, "generated"); err != nil {
		t.Fatal(err)
	}
	owned := OwnedContextFiles(worktreePath)
	for _, want := range []string{claudeContextFile, claudeContextSentinel} {
		if !slices.Contains(owned, want) {
			t.Errorf("OwnedContextFiles() = %v, want %q", owned, want)
		}
	}
}

func TestClaudeContextWithoutMarkerIsNotOwned(t *testing.T) {
	worktreePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktreePath, claudeContextFile), []byte("user owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	if owned := OwnedContextFiles(worktreePath); len(owned) != 0 {
		t.Fatalf("OwnedContextFiles() = %v, want no owned files", owned)
	}
}
