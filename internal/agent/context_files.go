package agent

import (
	"os"
	"path/filepath"
	"strings"
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
	if sentinel := filepath.Join(worktreePath, codexSentinel); pathExists(sentinel) {
		owned := codexOwnedContextFile(worktreePath, sentinel)
		paths = append(paths, owned, codexSentinel)
	}
	return paths
}

func codexOwnedContextFile(worktreePath, sentinel string) string {
	if data, err := os.ReadFile(sentinel); err == nil {
		switch strings.TrimSpace(string(data)) {
		case "AGENTS.md":
			return "AGENTS.md"
		case codexSideContextFile:
			return codexSideContextFile
		}
	}

	// Sentinels created before they recorded ownership were empty. Those
	// injections used the side file only when it existed, so retain that
	// behavior without claiming both possible context files.
	if pathExists(filepath.Join(worktreePath, codexSideContextFile)) {
		return codexSideContextFile
	}
	return "AGENTS.md"
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
