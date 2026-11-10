package reconciler

import (
	"fmt"
	"strconv"
	"strings"
)

// MakePRKey creates a workqueue key for a PR in the format "repo:number".
func MakePRKey(repo string, prNumber int) string {
	return fmt.Sprintf("%s:%d", repo, prNumber)
}

// ParsePRKey parses a workqueue key back into repo and PR number.
func ParsePRKey(key string) (repo string, number int, err error) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid PR key %q: expected format repo:number", key)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid PR key %q: bad number: %w", key, err)
	}
	return parts[0], n, nil
}

// MakeFeatureKey creates a cleanup key for a work stream in the format
// "repo:number:worktree-name". ParsePRKey rejects it, so the two key kinds
// share the cleanup queue without ambiguity.
func MakeFeatureKey(repo string, prNumber int, name string) string {
	return fmt.Sprintf("%s:%d:%s", repo, prNumber, name)
}

// ParseFeatureKey parses a key made by MakeFeatureKey.
func ParseFeatureKey(key string) (repo string, number int, name string, err error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", 0, "", fmt.Errorf("invalid feature key %q: expected format repo:number:name", key)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid feature key %q: bad number: %w", key, err)
	}
	return parts[0], n, parts[2], nil
}

// MakeSlackKey creates a workqueue key for a Slack-flagged message in the
// format "channelID:messageTS". Message timestamps use a literal dot
// (e.g. "1700000000.123456"), so splitting on the first colon is safe.
func MakeSlackKey(channelID, messageTS string) string {
	return fmt.Sprintf("%s:%s", channelID, messageTS)
}

// ParseSlackKey parses a workqueue key back into channel ID and message timestamp.
func ParseSlackKey(key string) (channelID, messageTS string, err error) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid Slack key %q: expected format channel:ts", key)
	}
	return parts[0], parts[1], nil
}
