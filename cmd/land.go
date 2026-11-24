package cmd

import (
	"fmt"
	"time"

	"github.com/mgreau/zen/internal/supervisor"
	"github.com/spf13/cobra"
)

var landRemove bool

var landCmd = &cobra.Command{
	Use:   "land [<repo>#<n>|<owner/repo>#<n>|<url>]...",
	Short: "Approve landing PRs: the supervisor arms auto-merge once approvals exist",
	Long: `Record your approval to land PRs. The PR supervisor in the watch daemon
then arms GitHub auto-merge (through land-watch) once the required approvals
exist, watches the deploy, and archives the worktree when it is verified.
Everything before the merge (CI fixes, conflicts, review feedback) happens
whether or not a PR is approved for landing.

With no arguments, lists the approved PRs. --remove withdraws approval.
Approvals live in ~/.zen/state/land.json; a landed PR leaves the list.`,
	RunE: runLand,
}

func init() {
	landCmd.Flags().BoolVar(&landRemove, "remove", false, "Withdraw landing approval")
	rootCmd.AddCommand(landCmd)
}

func runLand(cmd *cobra.Command, args []string) error {
	path := supervisor.LandFile()
	entries, err := supervisor.LoadLand(path)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if jsonFlag {
			printJSON(entries)
			return nil
		}
		if len(entries) == 0 {
			fmt.Println("No PRs approved for landing.")
		}
		for _, e := range entries {
			fmt.Printf("%s#%d  approved %s\n", e.Repo, e.Number, e.Added.Local().Format("2006-01-02 15:04"))
		}
		return nil
	}
	var refs []supervisor.LandEntry
	for _, a := range args {
		ref, err := supervisor.ParseLandRef(cfg, a)
		if err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	if landRemove {
		entries = supervisor.RemoveLand(entries, refs)
		if err := supervisor.SaveLand(path, entries); err != nil {
			return err
		}
		for _, r := range refs {
			fmt.Printf("Withdrew landing approval for %s#%d\n", r.Repo, r.Number)
		}
		return nil
	}
	entries, added := supervisor.AddLand(entries, refs, time.Now())
	if err := supervisor.SaveLand(path, entries); err != nil {
		return err
	}
	for _, r := range refs {
		fmt.Printf("Landing approved for %s#%d\n", r.Repo, r.Number)
	}
	if len(added) < len(refs) {
		fmt.Printf("(%d already approved)\n", len(refs)-len(added))
	}
	return nil
}
