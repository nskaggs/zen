package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mgreau/zen/internal/ui"
	wt "github.com/mgreau/zen/internal/worktree"
	"github.com/spf13/cobra"
)

var archiveCmd = &cobra.Command{
	Use:   "archive <name|path>",
	Short: "Keep a worktree's work in Git, then remove the worktree",
	Long: `Archive a worktree: commit any uncommitted work, pin HEAD under
refs/archive/<YYYY-MM-DD>/<name>, record it in ~/.zen/state/archive/<YYYY-MM-DD>.tsv,
and remove the worktree. Branches stay. Ignored files and Zen-generated
context are not kept. Refuses while an agent is running in the worktree.

Restore with: git worktree add <path> refs/archive/<YYYY-MM-DD>/<name>`,
	Args: cobra.ExactArgs(1),
	RunE: runArchive,
}

func init() {
	rootCmd.AddCommand(archiveCmd)
}

func runArchive(cmd *cobra.Command, args []string) error {
	match, err := findWorktreeExact(args[0])
	if err != nil {
		return err
	}
	originPath := filepath.Join(cfg.RepoBasePath(match.Repo), match.Repo)
	archived, err := wt.Archive(originPath, match.Path)
	if err != nil {
		return err
	}
	if archived == nil {
		return fmt.Errorf("worktree %s is already gone", match.Name)
	}
	if jsonFlag {
		printJSON(archived)
		return nil
	}
	home := os.Getenv("HOME")
	wip := ""
	if archived.WIP {
		wip = " (committed uncommitted work first)"
	}
	ui.LogSuccess(fmt.Sprintf("Archived %s at %s%s", archived.Name, archived.Ref, wip))
	fmt.Printf("  Branch:   %s\n", archived.Branch)
	fmt.Printf("  Manifest: %s\n", ui.ShortenHome(archived.Manifest, home))
	fmt.Printf("  Restore:  git -C %s worktree add %s %s\n", ui.ShortenHome(originPath, home), ui.ShortenHome(archived.Path, home), archived.Ref)
	return nil
}
