package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mgreau/zen/internal/agent"
)

var archiveDay = time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)

func notRunning(string) bool { return false }

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestArchive(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		wantWIP bool
		// inCommit lists paths the archived commit must contain; notInCommit
		// lists paths it must not.
		inCommit, notInCommit []string
	}{
		{name: "clean"},
		{name: "tracked change", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "tracked", "changed")
		}, wantWIP: true, inCommit: []string{"tracked"}},
		{name: "untracked file", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, "notes.txt", "keep me")
		}, wantWIP: true, inCommit: []string{"notes.txt"}},
		{name: "ignored file is not committed", prepare: func(t *testing.T, path string) {
			writeRemovalFile(t, path, ".gitignore", "ignored.log\n")
			runRemovalGit(t, path, "add", ".gitignore")
			runRemovalGit(t, path, "commit", "-m", "ignore generated log")
			writeRemovalFile(t, path, "ignored.log", "build output")
		}, notInCommit: []string{"ignored.log"}},
		{name: "generated context is not committed", prepare: func(t *testing.T, path string) {
			if _, err := agent.New(agent.Claude, "").InjectContext(path, "generated"); err != nil {
				t.Fatal(err)
			}
			writeRemovalFile(t, path, "notes.txt", "keep me")
		}, wantWIP: true, inCommit: []string{"notes.txt"}, notInCommit: []string{"CLAUDE.local.md", ".zen/.claude_context_injected"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			origin, path := removalFixture(t)
			if test.prepare != nil {
				test.prepare(t, path)
			}
			before := gitIn(t, path, "rev-parse", "HEAD")
			dir := t.TempDir()
			got, err := archive(origin, path, notRunning, nil, archiveDay, dir)
			if err != nil {
				t.Fatalf("archive() error = %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("archive left the worktree")
			}
			if got.WIP != test.wantWIP {
				t.Errorf("WIP = %t, want %t", got.WIP, test.wantWIP)
			}
			wantRef := "refs/archive/2026-11-10/repo-feature"
			if got.Ref != wantRef || got.Branch != "feature" {
				t.Errorf("archived = %+v, want ref %s on branch feature", got, wantRef)
			}
			if ref := gitIn(t, origin, "rev-parse", wantRef); ref != got.Head {
				t.Errorf("%s = %s, want %s", wantRef, ref, got.Head)
			}
			if branch := gitIn(t, origin, "rev-parse", "feature"); branch != got.Head {
				t.Errorf("branch feature = %s, want archived head %s", branch, got.Head)
			}
			if test.wantWIP {
				if got.Head == before {
					t.Error("WIP archive did not commit")
				}
				if msg := gitIn(t, origin, "log", "-1", "--format=%s", wantRef); msg != ArchiveCommitMessage {
					t.Errorf("commit message = %q", msg)
				}
			} else if got.Head != before {
				t.Errorf("archive committed with nothing to keep: %s != %s", got.Head, before)
			}
			files := gitIn(t, origin, "ls-tree", "-r", "--name-only", wantRef)
			for _, f := range test.inCommit {
				if !strings.Contains("\n"+files+"\n", "\n"+f+"\n") {
					t.Errorf("archived commit is missing %s: %s", f, files)
				}
			}
			for _, f := range test.notInCommit {
				if strings.Contains("\n"+files+"\n", "\n"+f+"\n") {
					t.Errorf("archived commit contains %s", f)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "2026-11-10.tsv"))
			if err != nil {
				t.Fatal(err)
			}
			want := manifestHeader + strings.Join([]string{"repo-feature", path, "feature", got.Head, wantRef, got.IgnoredBackup}, "\t") + "\n"
			if string(data) != want {
				t.Errorf("manifest = %q, want %q", data, want)
			}
		})
	}
}

func TestArchiveDetachedHead(t *testing.T) {
	origin, path := removalFixture(t)
	runRemovalGit(t, path, "checkout", "--detach")
	writeRemovalFile(t, path, "notes.txt", "keep me")
	got, err := archive(origin, path, notRunning, nil, archiveDay, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "detached" || !got.WIP {
		t.Fatalf("archived = %+v, want a WIP commit on a detached HEAD", got)
	}
	if ref := gitIn(t, origin, "rev-parse", got.Ref); ref != got.Head {
		t.Fatalf("archive ref = %s, want %s", ref, got.Head)
	}
}

func TestArchiveAppendsManifestRows(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		origin, path := removalFixture(t)
		if _, err := archive(origin, path, notRunning, nil, archiveDay, dir); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-11-10.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 || lines[0]+"\n" != manifestHeader {
		t.Fatalf("manifest = %q, want a header and two rows", data)
	}
}

func TestArchiveRefusesRunningAgent(t *testing.T) {
	origin, path := removalFixture(t)
	writeRemovalFile(t, path, "notes.txt", "keep me")
	_, err := archive(origin, path, func(string) bool { return true }, nil, archiveDay, t.TempDir())
	if !errors.Is(err, ErrWorktreeActive) || !RemovalBlocked(err) {
		t.Fatalf("archive() error = %v, want %v", err, ErrWorktreeActive)
	}
	if _, err := os.Stat(filepath.Join(path, "notes.txt")); err != nil {
		t.Fatal("active-agent refusal touched the worktree")
	}
	if out := gitIn(t, origin, "for-each-ref", "refs/archive"); out != "" {
		t.Fatalf("active-agent refusal created archive refs: %s", out)
	}
}

func TestArchiveNeverMovesExistingRef(t *testing.T) {
	origin, path := removalFixture(t)
	base := gitIn(t, origin, "rev-parse", "main")
	commitRemovalFile(t, path, "tracked", "feature work")
	runRemovalGit(t, origin, "update-ref", "refs/archive/2026-11-10/repo-feature", base)
	_, err := archive(origin, path, notRunning, nil, archiveDay, t.TempDir())
	if !errors.Is(err, ErrArchiveRefExists) || !RemovalBlocked(err) {
		t.Fatalf("archive() error = %v, want %v", err, ErrArchiveRefExists)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("ref conflict removed the worktree")
	}
	if ref := gitIn(t, origin, "rev-parse", "refs/archive/2026-11-10/repo-feature"); ref != base {
		t.Fatal("existing archive ref moved")
	}
}

func TestArchiveMissingIsIdempotent(t *testing.T) {
	got, err := archive(t.TempDir(), filepath.Join(t.TempDir(), "missing"), notRunning, nil, archiveDay, t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("archive() = %+v, %v; want nil, nil", got, err)
	}
}

func TestArchiveReview(t *testing.T) {
	t.Run("local commit is kept by the archive ref", func(t *testing.T) {
		_, origin, path, _ := reviewFixture(t)
		local := commitRemovalFile(t, path, "tracked", "reviewer fix")
		got, err := archiveReview(origin, path, 123, notRunning, archiveDay, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got.Head != local || got.Branch != "pr-123" {
			t.Fatalf("archived = %+v, want head %s on pr-123", got, local)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("archive left the review worktree")
		}
	})
	t.Run("not a review checkout", func(t *testing.T) {
		_, origin, path, _ := reviewFixture(t)
		runRemovalGit(t, path, "checkout", "-b", "my-fix")
		_, err := archiveReview(origin, path, 123, notRunning, archiveDay, t.TempDir())
		if !errors.Is(err, ErrNotPRReview) {
			t.Fatalf("archiveReview() error = %v, want %v", err, ErrNotPRReview)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("refusal removed the worktree")
		}
	})
}

func TestArchiveBacksUpIgnoredFiles(t *testing.T) {
	origin, path := removalFixture(t)
	writeRemovalFile(t, path, ".gitignore", "*.log\nbuild/\n.zen/\n")
	runRemovalGit(t, path, "add", ".gitignore")
	runRemovalGit(t, path, "commit", "-m", "ignore outputs")
	writeRemovalFile(t, path, "run.log", "log line")
	writeRemovalFile(t, path, "build/bin/tool", "binary")
	if err := os.Chmod(filepath.Join(path, "build/bin/tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/tool", filepath.Join(path, "build/current")); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.New(agent.Claude, "").InjectContext(path, "generated"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	got, err := archive(origin, path, notRunning, nil, archiveDay, dir)
	if err != nil {
		t.Fatal(err)
	}
	wantBackup := filepath.Join(dir, "2026-11-10", "repo-feature")
	if got.IgnoredBackup != wantBackup {
		t.Fatalf("IgnoredBackup = %q, want %q", got.IgnoredBackup, wantBackup)
	}
	for name, want := range map[string]string{"run.log": "log line", "build/bin/tool": "binary"} {
		data, err := os.ReadFile(filepath.Join(wantBackup, name))
		if err != nil || string(data) != want {
			t.Errorf("backup %s = %q, %v; want %q", name, data, err, want)
		}
	}
	if info, err := os.Stat(filepath.Join(wantBackup, "build/bin/tool")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("backup kept mode %v, %v; want 0755", info.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(wantBackup, "build/current")); err != nil || target != "bin/tool" {
		t.Errorf("backup symlink = %q, %v; want bin/tool", target, err)
	}
	if _, err := os.Lstat(filepath.Join(wantBackup, ".zen")); !os.IsNotExist(err) {
		t.Error("generated context was backed up")
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-11-10.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\t"+wantBackup+"\n") {
		t.Errorf("manifest does not end with the backup dir: %q", data)
	}
}

func TestArchiveWithoutIgnoredFilesHasNoBackup(t *testing.T) {
	origin, path := removalFixture(t)
	dir := t.TempDir()
	got, err := archive(origin, path, notRunning, nil, archiveDay, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.IgnoredBackup != "" {
		t.Fatalf("IgnoredBackup = %q, want empty", got.IgnoredBackup)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-10")); !os.IsNotExist(err) {
		t.Fatal("backup dir created with nothing to back up")
	}
}

func TestAppendManifestIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-11-10.tsv")
	row := []string{"repo-feature", "/w/repo-feature", "feature", "abc", "refs/archive/2026-11-10/repo-feature", ""}
	for range 2 {
		if _, err := appendManifest(path, row); err != nil {
			t.Fatal(err)
		}
	}
	other := append([]string(nil), row...)
	other[3] = "def"
	if _, err := appendManifest(path, other); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(lines) != 3 {
		t.Fatalf("manifest = %q, want a header, the row once, and the other head", data)
	}
}

func TestArchiveKeepsStagedContentTheWorkingTreeReverted(t *testing.T) {
	origin, path := removalFixture(t)
	writeRemovalFile(t, path, "tracked", "staged only")
	runRemovalGit(t, path, "add", "tracked")
	writeRemovalFile(t, path, "tracked", "base")
	got, err := archive(origin, path, notRunning, nil, archiveDay, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !got.WIP {
		t.Fatal("staged-only content made no WIP commit")
	}
	found := false
	for _, sha := range strings.Fields(gitIn(t, origin, "rev-list", got.Ref)) {
		if gitIn(t, origin, "show", sha+":tracked") == "staged only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no commit reachable from %s holds the staged content", got.Ref)
	}
	if content := gitIn(t, origin, "show", got.Ref+":tracked"); content != "base" {
		t.Fatalf("archived tip tracked = %q, want the working tree's base", content)
	}
}

// archiveIgnoredTwice archives a worktree with ignored file run.log holding
// first, recreates the worktree at the same path and HEAD, writes second, and
// archives again the same day.
func archiveIgnoredTwice(t *testing.T, first, second string) (dir string, a, b *Archived) {
	t.Helper()
	origin, path := removalFixture(t)
	writeRemovalFile(t, path, ".gitignore", "*.log\n")
	runRemovalGit(t, path, "add", ".gitignore")
	runRemovalGit(t, path, "commit", "-m", "ignore logs")
	writeRemovalFile(t, path, "run.log", first)
	dir = t.TempDir()
	a, err := archive(origin, path, notRunning, nil, archiveDay, dir)
	if err != nil {
		t.Fatal(err)
	}
	runRemovalGit(t, origin, "worktree", "add", path, "feature")
	writeRemovalFile(t, path, "run.log", second)
	b, err = archive(origin, path, notRunning, nil, archiveDay, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, a, b
}

func TestArchiveAgainNeverOverwritesBackup(t *testing.T) {
	dir, a, b := archiveIgnoredTwice(t, "first", "second")
	if a.IgnoredBackup == b.IgnoredBackup {
		t.Fatalf("both archives used backup %s", a.IgnoredBackup)
	}
	for backup, want := range map[string]string{a.IgnoredBackup: "first", b.IgnoredBackup: "second"} {
		if data, err := os.ReadFile(filepath.Join(backup, "run.log")); err != nil || string(data) != want {
			t.Errorf("%s/run.log = %q, %v; want %q", backup, data, err, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-11-10.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(lines) != 3 {
		t.Fatalf("manifest = %q, want a row for each backup", data)
	}
}

func TestArchiveRetryReusesIdenticalBackup(t *testing.T) {
	dir, a, b := archiveIgnoredTwice(t, "same", "same")
	if a.IgnoredBackup != b.IgnoredBackup {
		t.Fatalf("retry recorded %s, want the first backup %s", b.IgnoredBackup, a.IgnoredBackup)
	}
	if _, err := os.Stat(a.IgnoredBackup + ".2"); !os.IsNotExist(err) {
		t.Fatal("identical retry left a duplicate backup dir")
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-11-10.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(lines) != 2 {
		t.Fatalf("manifest = %q, want one row", data)
	}
}

func TestArchiveBranchRefusesChangedBranch(t *testing.T) {
	t.Setenv("ZEN_HOME", t.TempDir())
	origin, path := removalFixture(t)
	runRemovalGit(t, path, "checkout", "-b", "other")
	_, err := ArchiveBranch(origin, path, "feature")
	if !errors.Is(err, ErrBranchChanged) || !RemovalBlocked(err) {
		t.Fatalf("ArchiveBranch() error = %v, want %v", err, ErrBranchChanged)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refusal removed the worktree")
	}
}
