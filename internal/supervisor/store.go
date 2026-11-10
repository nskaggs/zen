package supervisor

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mgreau/zen/internal/config"
)

// Snapshot is the supervisor's view after a poll, written atomically to
// snapshot.json for `zen sup` and other readers.
type Snapshot struct {
	UpdatedAt time.Time `json:"updated_at"`
	NextPoll  time.Time `json:"next_poll"`
	Items     []Item    `json:"items"`
	Error     string    `json:"error,omitempty"`
}

// Event is one supervisor action or wake result, appended to events.jsonl.
type Event struct {
	Time   time.Time `json:"time"`
	PR     string    `json:"pr"` // owner/repo#N
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

// Dir is ~/.zen/state/supervisor.
func Dir() string { return filepath.Join(config.StateDir(), "supervisor") }

func snapshotPath(dir string) string { return filepath.Join(dir, "snapshot.json") }
func eventsPath(dir string) string   { return filepath.Join(dir, "events.jsonl") }
func memoryPath(dir string) string   { return filepath.Join(dir, "memory.json") }
func boardPath(dir string) string    { return filepath.Join(dir, "board.md") }

// LoadSnapshot reads the latest snapshot.
func LoadSnapshot(dir string) (Snapshot, error) {
	var s Snapshot
	data, err := os.ReadFile(snapshotPath(dir))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(data, &s)
	return s, err
}

func saveSnapshot(dir string, s Snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(snapshotPath(dir), append(data, '\n'))
}

func appendEvent(dir string, e Event) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(eventsPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// RecentEvents returns the last n events, oldest first.
func RecentEvents(dir string, n int) []Event {
	f, err := os.Open(eventsPath(dir))
	if err != nil {
		return nil
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			events = append(events, e)
			if len(events) > n {
				events = events[1:]
			}
		}
	}
	return events
}

func loadMemory(dir string) map[string]PRMemory {
	m := map[string]PRMemory{}
	if data, err := os.ReadFile(memoryPath(dir)); err == nil {
		if json.Unmarshal(data, &m) != nil || m == nil {
			m = map[string]PRMemory{}
		}
	}
	return m
}

func saveMemory(dir string, m map[string]PRMemory) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(memoryPath(dir), append(data, '\n'))
}

// writeAtomic writes data to path through a temp file and rename, so a
// reader never sees a partial file.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// BoardMarkdown renders the snapshot as Markdown for agents and documents.
func BoardMarkdown(s Snapshot, events []Event, now time.Time) string {
	var b strings.Builder
	b.WriteString("# Zen supervisor\n\n")
	b.WriteString("Updated " + s.UpdatedAt.Local().Format("2006-01-02 15:04") + ".\n")
	if s.Error != "" {
		b.WriteString("\nLast poll error: " + s.Error + "\n")
	}
	for _, owner := range Owners {
		items := sectionItems(s.Items, owner, now)
		if len(items) == 0 {
			continue
		}
		b.WriteString("\n## " + owner.Title() + "\n\n")
		for _, it := range items {
			b.WriteString("- [" + it.Repo + "#" + itoa(it.Number) + "](" + it.URL + ") " + it.Title + ": " + it.Summary)
			if it.Land {
				b.WriteString(" (land approved)")
			}
			b.WriteString("\n")
		}
	}
	if len(events) > 0 {
		b.WriteString("\n## Recent actions\n\n")
		for _, e := range events {
			b.WriteString("- " + e.Time.Local().Format("15:04") + " " + e.PR + " " + e.Kind)
			if e.Detail != "" {
				b.WriteString(": " + e.Detail)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
