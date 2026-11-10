package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mgreau/zen/internal/agent"
	"github.com/mgreau/zen/internal/config"
)

// ArchiveCommitMessage is the message of the commit Archive makes when a
// worktree holds uncommitted work.
const ArchiveCommitMessage = "wip: archive uncommitted work before worktree removal"

// ErrArchiveRefExists means the archive ref for this worktree and day already
// points at a different commit, so archiving again would move it.
var ErrArchiveRefExists = errors.New("refusing to move an existing archive ref")

// ErrBranchChanged means the worktree no longer has the branch checked out
// that the caller verified, so archiving it now would act on other work.
var ErrBranchChanged = errors.New("refusing to archive worktree whose branch changed")

// manifestHeader is the first line of each day's archive manifest.
const manifestHeader = "worktree\tpath\tbranch\thead\tarchive_ref\tignored_backup\n"

// Archived describes one archived worktree.
type Archived struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Branch   string `json:"branch"` // "detached" when HEAD was detached
	Head     string `json:"head"`
	Ref      string `json:"archive_ref"`
	WIP      bool   `json:"wip"` // true when uncommitted work was committed first
	Manifest string `json:"manifest"`
	// IgnoredBackup is the directory holding copies of the worktree's
	// git-ignored files, empty when it had none.
	IgnoredBackup string `json:"ignored_backup,omitempty"`
}

// Archive keeps everything a worktree holds and then removes it. Uncommitted
// tracked changes and untracked files (honoring .gitignore, and leaving out
// Zen-generated context) are committed with ArchiveCommitMessage on the
// checked-out branch or detached HEAD. HEAD is then pinned at
// refs/archive/<YYYY-MM-DD>/<worktree name>, git-ignored files are copied to
// <zen dir>/state/archive/<YYYY-MM-DD>/<worktree name>/, a row is appended
// to that day's manifest under <zen dir>/state/archive/, and the worktree is
// removed from the main checkout. Branches are never deleted.
// It refuses when an agent is running in the worktree. A missing worktree is
// already gone and returns nil, nil.
func Archive(originPath, worktreePath string) (*Archived, error) {
	return archive(originPath, worktreePath, agent.RunningIn, nil, time.Now(), archiveDir())
}

// ArchiveReview archives a PR review worktree. On top of Archive's checks it
// refuses unless the worktree is still checked out as review prNumber
// (ErrNotPRReview). Commits beyond the PR head are kept by the archive ref.
func ArchiveReview(originPath, worktreePath string, prNumber int) (*Archived, error) {
	return archiveReview(originPath, worktreePath, prNumber, agent.RunningIn, time.Now(), archiveDir())
}

// ArchiveBranch archives a work stream only while it still has branch checked
// out, checked under the same lock as the archive (ErrBranchChanged).
func ArchiveBranch(originPath, worktreePath, branch string) (*Archived, error) {
	return archive(originPath, worktreePath, agent.RunningIn, func() error {
		current, detached, err := checkedOutBranch(worktreePath)
		if err != nil {
			return err
		}
		if detached || current != branch {
			return fmt.Errorf("%w: now %q, verified %q", ErrBranchChanged, current, branch)
		}
		return nil
	}, time.Now(), archiveDir())
}

// CurrentBranch returns the branch checked out in worktreePath, or
// detached=true when HEAD is detached.
func CurrentBranch(worktreePath string) (branch string, detached bool, err error) {
	return checkedOutBranch(worktreePath)
}

func archiveReview(originPath, worktreePath string, prNumber int, runningIn func(string) bool, now time.Time, dir string) (*Archived, error) {
	return archive(originPath, worktreePath, runningIn, func() error {
		branch, detached, err := checkedOutBranch(worktreePath)
		if err != nil {
			return err
		}
		if kind, pr := Classify(filepath.Base(worktreePath), branch, detached); kind != TypePRReview || pr != prNumber {
			return fmt.Errorf("%w (branch %q)", ErrNotPRReview, branch)
		}
		return nil
	}, now, dir)
}

func archiveDir() string {
	return filepath.Join(config.Dir(), "state", "archive")
}

func archive(originPath, worktreePath string, runningIn func(string) bool, verify func() error, now time.Time, dir string) (*Archived, error) {
	GitMu.Lock()
	defer GitMu.Unlock()

	if _, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect worktree: %w", err)
	}
	if runningIn(worktreePath) {
		return nil, ErrWorktreeActive
	}
	if verify != nil {
		if err := verify(); err != nil {
			return nil, err
		}
	}

	owned := make(map[string]struct{})
	for _, path := range agent.OwnedContextFiles(worktreePath) {
		owned[filepath.ToSlash(path)] = struct{}{}
	}

	wip, err := commitWIP(worktreePath, owned)
	if err != nil {
		return nil, err
	}

	branch, detached, err := checkedOutBranch(worktreePath)
	if err != nil {
		return nil, err
	}
	if detached {
		branch = "detached"
	}
	head, err := HEAD(worktreePath)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(worktreePath)
	day := now.Format("2006-01-02")
	ref := "refs/archive/" + day + "/" + name
	if err := pinRef(worktreePath, ref, head); err != nil {
		return nil, err
	}
	backup, err := copyIgnored(worktreePath, freeDir(filepath.Join(dir, day, name)), owned)
	if err != nil {
		return nil, err
	}
	manifest := filepath.Join(dir, day+".tsv")
	if backup, err = appendManifest(manifest, []string{name, worktreePath, branch, head, ref, backup}); err != nil {
		return nil, err
	}

	snapshots, err := removeOwnedContext(worktreePath, owned)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "worktree", "remove", worktreePath)
	cmd.Dir = originPath
	if out, err := cmd.CombinedOutput(); err != nil {
		if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
			if restoreErr := restoreOwnedContext(snapshots); restoreErr != nil {
				return nil, fmt.Errorf("git worktree remove: %w: %s; restore generated context: %v", err, strings.TrimSpace(string(out)), restoreErr)
			}
		}
		return nil, fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return &Archived{Name: name, Path: worktreePath, Branch: branch, Head: head, Ref: ref, WIP: wip, Manifest: manifest, IgnoredBackup: backup}, nil
}

// commitWIP commits the worktree's uncommitted work, leaving out
// Zen-generated context, and reports whether it made a commit. Content that
// is staged but differs from the working tree is committed first, so `git add
// -A` cannot replace it before it is kept.
func commitWIP(worktreePath string, owned map[string]struct{}) (bool, error) {
	staged, err := commitIndex(worktreePath, owned)
	if err != nil {
		return false, err
	}
	if out, err := gitOutput(worktreePath, "add", "-A"); err != nil {
		return false, fmt.Errorf("git add: %w: %s", err, out)
	}
	working, err := commitIndex(worktreePath, owned)
	if err != nil {
		return false, err
	}
	return staged || working, nil
}

// commitIndex unstages Zen-generated context, then commits the index when it
// differs from HEAD. It reports whether it made a commit.
func commitIndex(worktreePath string, owned map[string]struct{}) (bool, error) {
	if len(owned) > 0 {
		args := []string{"reset", "-q", "--"}
		for path := range owned {
			args = append(args, path)
		}
		if out, err := gitOutput(worktreePath, args...); err != nil {
			return false, fmt.Errorf("git reset generated context: %w: %s", err, out)
		}
	}
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = worktreePath
	if err := cmd.Run(); err == nil {
		return false, nil
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return false, fmt.Errorf("git diff --cached: %w", err)
		}
	}
	if out, err := gitOutput(worktreePath, "commit", "--no-verify", "-q", "-m", ArchiveCommitMessage); err != nil {
		return false, fmt.Errorf("git commit: %w: %s", err, out)
	}
	return true, nil
}

// pinRef creates ref at head. An existing ref at the same commit is kept; one
// at another commit is never moved.
func pinRef(worktreePath, ref, head string) error {
	if existing, err := RevParse(worktreePath, ref); err == nil {
		if SHAEqual(existing, head) {
			return nil
		}
		return fmt.Errorf("%w: %s is at %s, HEAD is %s", ErrArchiveRefExists, ref, abbrev(existing), abbrev(head))
	}
	if out, err := gitOutput(worktreePath, "update-ref", ref, head, ""); err != nil {
		return fmt.Errorf("git update-ref %s: %w: %s", ref, err, out)
	}
	return nil
}

// copyIgnored copies every git-ignored file in worktreePath, except
// Zen-generated context, to dest, keeping relative paths, modes, and
// symlinks. It returns dest, or "" when there was nothing to copy.
func copyIgnored(worktreePath, dest string, owned map[string]struct{}) (string, error) {
	cmd := exec.Command("git", "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list ignored files: %w", err)
	}
	copied := false
	for _, raw := range bytes.Split(out, []byte{0}) {
		rel := string(raw)
		if rel == "" {
			continue
		}
		if _, ok := owned[filepath.ToSlash(rel)]; ok {
			continue
		}
		if err := copyEntry(filepath.Join(worktreePath, rel), filepath.Join(dest, rel)); err != nil {
			return "", fmt.Errorf("back up ignored file %s: %w", rel, err)
		}
		copied = true
	}
	if !copied {
		return "", nil
	}
	return dest, nil
}

// freeDir returns dir when nothing exists there yet, otherwise the first of
// dir.2, dir.3, ... that does not exist, so an earlier backup is never
// overwritten.
func freeDir(dir string) string {
	candidate := dir
	for n := 2; ; n++ {
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s.%d", dir, n)
	}
}

// copyEntry copies one file or symlink from src to dst, replacing dst.
func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !info.Mode().IsRegular() {
		return nil // sockets, FIFOs, and devices hold no content to keep
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	outFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(outFile, in); err != nil {
		outFile.Close()
		return err
	}
	return outFile.Close()
}

// appendManifest appends row and returns its ignored-file backup dir. When a
// row for the same worktree, path, head, and archive ref already records a
// backup with identical content, as after a retried archive, it removes the
// new copy, writes nothing, and returns the recorded dir instead.
func appendManifest(path string, row []string) (string, error) {
	backup := row[5]
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create archive manifest dir: %w", err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read archive manifest: %w", err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 6 || f[0] != row[0] || f[1] != row[1] || f[3] != row[3] || f[4] != row[4] {
			continue
		}
		same, err := sameTree(f[5], backup)
		if err != nil {
			return "", err
		}
		if same {
			if backup != "" && backup != f[5] {
				if err := os.RemoveAll(backup); err != nil {
					return "", fmt.Errorf("remove duplicate backup: %w", err)
				}
			}
			return f[5], nil
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("open archive manifest: %w", err)
	}
	defer f.Close()
	line := strings.Join(row, "\t") + "\n"
	if len(existing) == 0 {
		line = manifestHeader + line
	}
	if _, err := f.WriteString(line); err != nil {
		return "", fmt.Errorf("write archive manifest: %w", err)
	}
	return backup, nil
}

// sameTree reports whether directories a and b hold the same files, symlinks,
// modes, and contents. Two empty names (no backup) are the same; a missing
// directory is not the same as any other.
func sameTree(a, b string) (bool, error) {
	if a == "" || b == "" {
		return a == b, nil
	}
	count := 0
	err := filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(a, p)
		if d.IsDir() {
			return nil
		}
		count++
		same, err := sameEntry(p, filepath.Join(b, rel))
		if err != nil || !same {
			if err == nil {
				err = errDiffer
			}
			return err
		}
		return nil
	})
	if errors.Is(err, errDiffer) || os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare backups: %w", err)
	}
	other := 0
	if err := filepath.WalkDir(b, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			other++
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("compare backups: %w", err)
	}
	return count == other, nil
}

var errDiffer = errors.New("backups differ")

func sameEntry(a, b string) (bool, error) {
	ai, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Lstat(b)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ai.Mode() != bi.Mode() || (ai.Mode().IsRegular() && ai.Size() != bi.Size()) {
		return false, nil
	}
	if ai.Mode()&os.ModeSymlink != 0 {
		at, err := os.Readlink(a)
		if err != nil {
			return false, err
		}
		bt, err := os.Readlink(b)
		return err == nil && at == bt, err
	}
	ad, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bd, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ad, bd), nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// githubOwner matches the owner in a GitHub HTTPS or SSH remote URL.
var githubOwner = regexp.MustCompile(`github\.com[:/]([^/]+)/`)

// RemoteOwners returns the distinct GitHub owners of worktreePath's fetch
// remotes, in the order git lists them. A PR's head lives in one of them.
func RemoteOwners(worktreePath string) ([]string, error) {
	out, err := gitOutput(worktreePath, "remote", "-v")
	if err != nil {
		return nil, fmt.Errorf("git remote -v: %w: %s", err, out)
	}
	var owners []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "(fetch)" {
			continue
		}
		m := githubOwner.FindStringSubmatch(fields[1])
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		owners = append(owners, m[1])
	}
	return owners, nil
}
