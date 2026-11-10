package supervisor

import (
	"fmt"
	"strings"
)

// logTailLines is how much of the failing job log the brief carries.
const logTailLines = 150

// BriefInput is the deterministic material for one wake.
type BriefInput struct {
	URL      string
	Head     string
	Base     string
	Issues   Issues
	Log      string // failing job log, already fetched
	Diffstat string
}

// BuildBrief renders the prompt an agent is woken with. It carries only what
// the agent needs to start: the PR, what is wrong, the log tail, the diffstat,
// and the unresolved comments, so a wake does not re-explore the PR.
func BuildBrief(in BriefInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Zen supervisor: %s (head %s) needs work before it is ready to merge.\n", in.URL, short(in.Head))
	b.WriteString("Nicholas's rule: carry this PR to ready-to-merge with approvals; only the merge waits on his land approval.\n\n")
	b.WriteString("Issues:\n")
	if in.Issues.Conflict {
		fmt.Fprintf(&b, "- Merge conflict with %s. Merge origin/%s into the branch and resolve it.\n", in.Base, in.Base)
	}
	if len(in.Issues.Failing) > 0 {
		fmt.Fprintf(&b, "- Failing checks: %s.\n", strings.Join(in.Issues.Failing, ", "))
	}
	if in.Issues.ChangesRequested {
		b.WriteString("- A reviewer requested changes.\n")
	}
	if n := len(in.Issues.Threads); n > 0 {
		fmt.Fprintf(&b, "- %d unresolved review thread%s (below).\n", n, plural(n))
	}
	if log := tail(in.Log, logTailLines); log != "" {
		fmt.Fprintf(&b, "\nFailing job log (last %d lines):\n%s\n", logTailLines, log)
	}
	if d := strings.TrimSpace(in.Diffstat); d != "" {
		fmt.Fprintf(&b, "\nDiffstat against %s:\n%s\n", in.Base, d)
	}
	if len(in.Issues.Threads) > 0 {
		b.WriteString("\nUnresolved review comments:\n")
		for _, t := range in.Issues.Threads {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", t.Author.Login, truncate(oneLine(t.Body), 600), t.URL)
		}
	}
	b.WriteString(`
Do: fix it in this worktree, run the relevant local tests, commit, and make an ordinary push to the PR branch.
After the push, resolve only the review threads that push addresses; do not reply to them.
Never force push, merge, enable auto-merge, comment or reply as Nicholas, or trigger a release, deploy, or apply.
If the fix is ambiguous, needs a decision, or is outside the PR's scope, change nothing and say why.
End with exactly one final line: "FIXED <what you changed>" or "STUCK <why>".
`)
	return b.String()
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
