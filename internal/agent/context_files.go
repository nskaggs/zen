package agent

import (
	"os"
	"path/filepath"
)

const (
	claudeContextFile     = "CLAUDE.local.md"
	claudeContextSentinel = ".zen/.claude_context_injected"
)

// OwnedContextFiles returns context files that a successful Zen injection
// marked as generated. Files without the corresponding sentinel are not Zen
// owned, even when their names match Zen's context filenames.
func OwnedContextFiles(worktreePath string) []string {
	var paths []string
	if pathExists(filepath.Join(worktreePath, claudeContextSentinel)) {
		paths = append(paths, claudeContextFile, claudeContextSentinel)
	}
	if pathExists(filepath.Join(worktreePath, codexSentinel)) {
		paths = append(paths, "AGENTS.md", codexSideContextFile, codexSentinel)
	}
	return paths
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
