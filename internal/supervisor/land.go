package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgreau/zen/internal/config"
)

// LandEntry is one PR the user approved for landing.
type LandEntry struct {
	Repo   string    `json:"repo"` // owner/name
	Number int       `json:"number"`
	Added  time.Time `json:"added"`
}

// LandFile is ~/.zen/state/land.json.
func LandFile() string { return filepath.Join(config.StateDir(), "land.json") }

// LoadLand reads the land list. A missing file is an empty list.
func LoadLand(path string) ([]LandEntry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []LandEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return entries, nil
}

// SaveLand writes the land list atomically, sorted.
func SaveLand(path string, entries []LandEntry) error {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Repo != entries[j].Repo {
			return entries[i].Repo < entries[j].Repo
		}
		return entries[i].Number < entries[j].Number
	})
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// AddLand adds refs to the list and returns the entries actually added.
func AddLand(entries []LandEntry, refs []LandEntry, now time.Time) ([]LandEntry, []LandEntry) {
	var added []LandEntry
	for _, r := range refs {
		if landIndex(entries, r.Repo, r.Number) >= 0 {
			continue
		}
		r.Added = now
		entries = append(entries, r)
		added = append(added, r)
	}
	return entries, added
}

// RemoveLand drops refs from the list.
func RemoveLand(entries []LandEntry, refs []LandEntry) []LandEntry {
	out := entries[:0]
	for _, e := range entries {
		if landIndex(refs, e.Repo, e.Number) < 0 {
			out = append(out, e)
		}
	}
	return out
}

func landIndex(entries []LandEntry, repo string, number int) int {
	for i, e := range entries {
		if e.Repo == repo && e.Number == number {
			return i
		}
	}
	return -1
}

var prURL = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/pull/(\d+)`)

// ParseLandRef reads "owner/repo#N", "short#N" (a configured repo), or a PR
// URL.
func ParseLandRef(cfg *config.Config, ref string) (LandEntry, error) {
	ref = strings.TrimSpace(ref)
	if m := prURL.FindStringSubmatch(ref); m != nil {
		n, _ := strconv.Atoi(m[2])
		return LandEntry{Repo: m[1], Number: n}, nil
	}
	repo, num, ok := strings.Cut(ref, "#")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n <= 0 || repo == "" {
		return LandEntry{}, fmt.Errorf("%q: want owner/repo#N, repo#N, or a PR URL", ref)
	}
	if !strings.Contains(repo, "/") {
		if _, known := cfg.Repos[repo]; !known {
			return LandEntry{}, fmt.Errorf("%q: %s is not a configured repo", ref, repo)
		}
		repo = cfg.RepoFullName(repo)
	}
	return LandEntry{Repo: repo, Number: n}, nil
}
