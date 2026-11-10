package worktree

import (
	"errors"
	"fmt"
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

// manifestHeader is the first line of each day's archive manifest.
const manifestHeader = "worktree\tpath\tbranch\thead\tarchive_ref\n"

// Archived describes one archived worktree.
type Archived struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Branch   string `json:"branch"` // "detached" when HEAD was detached
	Head     string `json:"head"`
	Ref      string `json:"archive_ref"`
	WIP      bool   `json:"wip"` // true when uncommitted work was committed first
	Manifest string `json:"manifest"`
}

// Archive keeps everything a worktree holds and then removes it. Uncommitted
// tracked changes and untracked files (honoring .gitignore, and leaving out
// Zen-generated context) are committed with ArchiveCommitMessage on the
// checked-out branch or detached HEAD. HEAD is then pinned at
// refs/archive/<YYYY-MM-DD>/<worktree name>, a row is appended to that day's
// manifest under <zen dir>/state/archive/, and the worktree is removed from
// the main checkout. Branches are never deleted. Ignored files are not kept.
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
	manifest := filepath.Join(dir, day+".tsv")
	if err := appendManifest(manifest, name, worktreePath, branch, head, ref); err != nil {
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
	return &Archived{Name: name, Path: worktreePath, Branch: branch, Head: head, Ref: ref, WIP: wip, Manifest: manifest}, nil
}

// commitWIP stages every change except Zen-generated context and commits it
// when anything is staged. It reports whether it made a commit.
func commitWIP(worktreePath string, owned map[string]struct{}) (bool, error) {
	if out, err := gitOutput(worktreePath, "add", "-A"); err != nil {
		return false, fmt.Errorf("git add: %w: %s", err, out)
	}
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

func appendManifest(path, name, worktreePath, branch, head, ref string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create archive manifest dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open archive manifest: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect archive manifest: %w", err)
	}
	row := strings.Join([]string{name, worktreePath, branch, head, ref}, "\t") + "\n"
	if info.Size() == 0 {
		row = manifestHeader + row
	}
	if _, err := f.WriteString(row); err != nil {
		return fmt.Errorf("write archive manifest: %w", err)
	}
	return nil
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
