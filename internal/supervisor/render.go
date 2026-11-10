package supervisor

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Width is the view's column limit, so it fits a portrait phone through
// Agent Console.
const Width = 56

// eventTail is how many recent events the view shows.
const eventTail = 6

// sectionItems returns the items for one section, newest-first for landed
// items. Landed items show only those merged since local midnight.
func sectionItems(items []Item, owner Owner, now time.Time) []Item {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var out []Item
	for _, it := range items {
		if it.Owner != owner {
			continue
		}
		if owner == OwnerLanded && it.MergedAt.Before(midnight) {
			continue
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if owner == OwnerLanded {
			return out[i].MergedAt.After(out[j].MergedAt)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// Render draws the snapshot as plain text, every line at most Width columns:
// a header, one section per owner (two lines per PR), and the recent events.
func Render(s Snapshot, events []Event, now time.Time) string {
	var lines []string
	add := func(s string) { lines = append(lines, fit(s, Width)) }

	header := "ZEN PRs  " + now.Local().Format("15:04")
	if !s.UpdatedAt.IsZero() {
		header += "  polled " + shortDuration(now.Sub(s.UpdatedAt)) + " ago"
	}
	add(header)
	if s.Error != "" {
		add("! " + s.Error)
	}
	empty := true
	for _, owner := range Owners {
		items := sectionItems(s.Items, owner, now)
		if len(items) == 0 {
			continue
		}
		empty = false
		add("")
		add(fmt.Sprintf("%s (%d)", owner.Title(), len(items)))
		for _, it := range items {
			ref := it.Repo + "#" + strconv.Itoa(it.Number)
			add(" " + ref + " " + it.Title)
			summary := it.Summary
			if it.Land && owner != OwnerLanded && owner != OwnerDeploying {
				summary += " · land ok"
			}
			add("   " + summary)
		}
	}
	if empty {
		add("")
		add("No open or recently merged PRs.")
	}
	if len(events) > 0 {
		add("")
		add("RECENT")
		start := 0
		if len(events) > eventTail {
			start = len(events) - eventTail
		}
		for _, e := range events[start:] {
			line := " " + e.Time.Local().Format("15:04") + " " + shortPR(e.PR) + " " + e.Kind
			if e.Detail != "" {
				line += ": " + e.Detail
			}
			add(line)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// shortPR drops the owner from "owner/repo#N".
func shortPR(pr string) string {
	if i := strings.Index(pr, "/"); i >= 0 {
		return pr[i+1:]
	}
	return pr
}

// fit truncates s to n runes.
func fit(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func itoa(n int) string { return strconv.Itoa(n) }
