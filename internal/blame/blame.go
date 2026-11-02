// Package blame tags findings whose line predates the reviewed change, so an
// agent can tell a bug the diff caused from one that was already there.
package blame

import (
	"context"
	"fmt"
	"strings"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
	"github.com/janiorvalle/roast/internal/verdict"
)

const uncommittedLine = "0000000000000000000000000000000000000000"

// Tag blames each finding's line at the reviewed revision and marks the
// finding pre-existing when the commit that last changed the line is outside
// the reviewed range: before the base for a branch or PR, another commit for a
// commit review, or already committed for a dirty review. A line on a file the
// diff adds is the change's own and is not blamed. A finding's line has no
// side, so when the post-change line at a number predates the change but the
// diff deleted the old line of that number, the finding may mean the deleted
// line; it stays untagged. So does a line blame cannot resolve. Both are named
// in a notice, so doubt never hides a defect.
func Tag(ctx context.Context, reviewTarget target.Target, diff []byte, findings []verdict.Finding, commands runner.Runner) ([]verdict.Finding, []string) {
	inRange, err := changeCommits(ctx, reviewTarget, commands)
	if err != nil {
		return findings, []string{fmt.Sprintf("[ROAST-BLAME] cannot list the reviewed commits: %v; findings are reported without a pre-existing tag", err)}
	}
	added := verdict.DiffAddedFiles(diff)
	deleted := verdict.DiffDeletedLines(diff)
	tagged := make([]verdict.Finding, 0, len(findings))
	notices := make([]string, 0)
	for _, finding := range findings {
		if _, isNew := added[finding.File]; isNew {
			tagged = append(tagged, finding)
			continue
		}
		commit, err := lineCommit(ctx, reviewTarget, finding, commands)
		_, isDeletedLine := deleted[finding.File][finding.Line]
		switch {
		case err != nil:
			notices = append(notices, fmt.Sprintf("[ROAST-BLAME] cannot blame %s:%d: %v; the finding is reported without a pre-existing tag", finding.File, finding.Line, err))
		case inRange(commit):
		case isDeletedLine:
			notices = append(notices, fmt.Sprintf("[ROAST-BLAME] %s:%d is a line number this change deleted, so the finding may point at the deleted line; it is reported without a pre-existing tag", finding.File, finding.Line))
		default:
			finding.PreExisting = true
		}
		tagged = append(tagged, finding)
	}
	return tagged, notices
}

// changeCommits returns the test for a commit being part of the reviewed
// change.
func changeCommits(ctx context.Context, reviewTarget target.Target, commands runner.Runner) (func(string) bool, error) {
	switch reviewTarget.Kind {
	case target.KindDirty:
		return func(commit string) bool { return commit == uncommittedLine }, nil
	case target.KindCommit:
		if reviewTarget.RootCommit {
			return func(string) bool { return true }, nil
		}
		return func(commit string) bool { return commit == reviewTarget.HeadSHA }, nil
	default:
		args := []string{"rev-list", reviewTarget.BaseSHA + ".." + reviewTarget.HeadSHA}
		result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, runner.Failure("git", args, result)
		}
		commits := make(map[string]struct{})
		for _, commit := range strings.Fields(string(result.Stdout)) {
			commits[commit] = struct{}{}
		}
		return func(commit string) bool {
			_, ok := commits[commit]
			return ok
		}, nil
	}
}

// lineCommit returns the commit that last changed the finding's line: the
// worktree for a dirty review, the reviewed head otherwise.
func lineCommit(ctx context.Context, reviewTarget target.Target, finding verdict.Finding, commands runner.Runner) (string, error) {
	lineRange := fmt.Sprintf("%d,%d", finding.Line, finding.Line)
	args := []string{"blame", "--porcelain", "-L", lineRange}
	if reviewTarget.Kind != target.KindDirty {
		args = append(args, reviewTarget.HeadSHA)
	}
	args = append(args, "--", finding.File)
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", runner.Failure("git", args, result)
	}
	header, _, _ := strings.Cut(string(result.Stdout), "\n")
	commit, _, _ := strings.Cut(header, " ")
	if len(commit) != len(uncommittedLine) {
		return "", fmt.Errorf("git blame returned no commit for the line")
	}
	return commit, nil
}
