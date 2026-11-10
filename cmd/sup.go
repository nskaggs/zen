package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mgreau/zen/internal/supervisor"
	"github.com/spf13/cobra"
)

var supOnce bool

var supCmd = &cobra.Command{
	Use:   "sup",
	Short: "Show the PR supervisor's view of your PRs (fits a phone screen)",
	Long: `Show what the PR supervisor in the watch daemon sees: your open and
recently merged PRs grouped by who owns the next step (you, an agent, a
reviewer, the deploy), each in two lines, plus its recent actions. Lines are
at most 48 columns so the view fits a 360px portrait phone through Agent Console.

Redraws every few seconds until interrupted. --once prints one frame;
--json prints the snapshot. The daemon writes the snapshot each poll when
supervisor.enabled is true in ~/.zen/config.yaml.`,
	Args: cobra.NoArgs,
	RunE: runSup,
}

func init() {
	supCmd.Flags().BoolVar(&supOnce, "once", false, "Print one frame and exit")
	rootCmd.AddCommand(supCmd)
}

func supFrame() string {
	dir := supervisor.Dir()
	snap, err := supervisor.LoadSnapshot(dir)
	if err != nil {
		snap.Error = "no snapshot yet: start the watch daemon"
	}
	return supervisor.Render(snap, supervisor.RecentEvents(dir, 6), time.Now())
}

func runSup(cmd *cobra.Command, args []string) error {
	if jsonFlag {
		snap, err := supervisor.LoadSnapshot(supervisor.Dir())
		if err != nil {
			return err
		}
		printJSON(snap)
		return nil
	}
	if supOnce {
		fmt.Print(supFrame())
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		// Home the cursor and clear, so the frame redraws in place.
		fmt.Print("\x1b[H\x1b[2J" + supFrame())
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
