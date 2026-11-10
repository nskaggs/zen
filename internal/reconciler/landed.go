package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// landWatchTimeout bounds one land-watch run for a single PR.
const landWatchTimeout = 5 * time.Minute

// deployVerifiedFunc reports whether a merged PR's deploy is verified. detail
// is the reason or land-watch's line for the PR, for the daemon log. Tests
// replace it so cleanup runs without land-watch or a network.
type deployVerifiedFunc func(ctx context.Context, fullRepo string, prNumber int) (verified bool, detail string, err error)

// landWatchVerified asks land-watch (without --arm, so it only reads) about
// one PR. A deploy is verified only when land-watch prints landed=yes for it,
// which requires every deploy stage it found for the merge commit to have
// succeeded. Without land-watch on PATH nothing is verified.
func landWatchVerified(ctx context.Context, fullRepo string, prNumber int) (bool, string, error) {
	bin, err := exec.LookPath("land-watch")
	if err != nil {
		return false, "land-watch is not installed", nil
	}
	dir, err := os.MkdirTemp("", "zen-land-watch-")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(dir)

	prs, err := json.Marshal([]map[string]any{{"repo": fullRepo, "number": prNumber}})
	if err != nil {
		return false, "", err
	}
	prsFile := filepath.Join(dir, "prs.json")
	if err := os.WriteFile(prsFile, prs, 0o600); err != nil {
		return false, "", err
	}

	ctx, cancel := context.WithTimeout(ctx, landWatchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--cache", filepath.Join(dir, "cache.json"), prsFile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	// Exit 1 means a deploy stage failed; the output still describes the PR.
	var exitErr *exec.ExitError
	if runErr != nil && !(errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1) {
		return false, "", fmt.Errorf("land-watch: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	verified, detail := parseLandWatch(stdout.String(), fullRepo, prNumber)
	if detail == "" {
		return false, "", fmt.Errorf("land-watch printed nothing for %s#%d: %s", fullRepo, prNumber, strings.TrimSpace(stderr.String()))
	}
	return verified, detail, nil
}

// parseLandWatch finds the PR's summary line in land-watch output and reports
// whether it says landed=yes. detail is that line, or empty when absent.
func parseLandWatch(out, fullRepo string, prNumber int) (bool, string) {
	prefix := fmt.Sprintf("%s#%d ", fullRepo, prNumber)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) || strings.HasPrefix(line, prefix+"stage=") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if field == "landed=yes" {
				return true, line
			}
		}
		return false, line
	}
	return false, ""
}
