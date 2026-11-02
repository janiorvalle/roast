// Package blame tags findings whose line predates the reviewed change, so an
// agent can tell a bug the diff caused from one that was already there.
package blame

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
	"github.com/janiorvalle/roast/internal/verdict"
)

const uncommittedLine = "0000000000000000000000000000000000000000"

// Tag blames each finding's line at the reviewed revision and marks the
// finding pre-existing when the commit that last changed the line is outside
// the change: before the base for a branch or PR, another commit for a
// commit review, and for a dirty review neither uncommitted nor on the branch
// since its base, because a dirty round in a review loop sits on top of the
// task's own commits. A line on a file the diff adds is the change's own and
// is not blamed. A finding's line has no side, so when the post-change line
// at a number predates the change but the diff deleted the old line of that
// number, the finding may mean the deleted line; it stays untagged. So does a
// line blame cannot resolve, and every committed line when a dirty review has
// no base to measure from. Each is named in a notice, so doubt never hides a
// defect.
func Tag(ctx context.Context, reviewTarget target.Target, diff []byte, findings []verdict.Finding, commands runner.Runner) ([]verdict.Finding, []string) {
	change, err := changeCommits(ctx, reviewTarget, commands)
	if err != nil {
		return findings, []string{fmt.Sprintf("[ROAST-BLAME] cannot list the reviewed commits: %v; findings are reported without a pre-existing tag", err)}
	}
	added := verdict.DiffAddedFiles(diff)
	deleted := verdict.DiffDeletedLines(diff)
	tagged := make([]verdict.Finding, 0, len(findings))
	notices := make([]string, 0)
	if change.notice != "" {
		notices = append(notices, change.notice)
	}
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
		case change.includes(commit):
		case !change.baseKnown:
		case isDeletedLine:
			notices = append(notices, fmt.Sprintf("[ROAST-BLAME] %s:%d is a line number this change deleted, so the finding may point at the deleted line; it is reported without a pre-existing tag", finding.File, finding.Line))
		default:
			finding.PreExisting = true
		}
		tagged = append(tagged, finding)
	}
	return tagged, notices
}

// changeMembership says which commits belong to the reviewed change. When
// baseKnown is false only uncommitted lines are known to belong, and nothing
// else can be called pre-existing.
type changeMembership struct {
	uncommitted bool
	commits     map[string]struct{}
	baseKnown   bool
	notice      string
}

func (change changeMembership) includes(commit string) bool {
	if change.uncommitted && commit == uncommittedLine {
		return true
	}
	_, ok := change.commits[commit]
	return ok
}

func changeCommits(ctx context.Context, reviewTarget target.Target, commands runner.Runner) (changeMembership, error) {
	switch reviewTarget.Kind {
	case target.KindDirty:
		return dirtyChangeCommits(ctx, reviewTarget, commands)
	case target.KindCommit:
		if reviewTarget.RootCommit {
			return changeMembership{baseKnown: false}, nil
		}
		return changeMembership{commits: map[string]struct{}{reviewTarget.HeadSHA: {}}, baseKnown: true}, nil
	default:
		commits, err := rangeCommits(ctx, reviewTarget.RepoDir, reviewTarget.BaseSHA, reviewTarget.HeadSHA, commands)
		if err != nil {
			return changeMembership{}, err
		}
		return changeMembership{commits: commits, baseKnown: true}, nil
	}
}

// dirtyChangeCommits treats the uncommitted lines and every commit on the
// branch since its base as the change. The base comes from the same
// candidates auto mode tries; without one, committed lines stay untagged.
func dirtyChangeCommits(ctx context.Context, reviewTarget target.Target, commands runner.Runner) (changeMembership, error) {
	base, err := target.BranchBase(ctx, reviewTarget.RepoDir, commands)
	if errors.Is(err, target.ErrNoBranchBase) {
		return changeMembership{
			uncommitted: true,
			notice:      fmt.Sprintf("[ROAST-BLAME] cannot find the branch base (%v), so committed lines are reported without a pre-existing tag; fetch origin/main or create main to measure from", err),
		}, nil
	}
	if err != nil {
		return changeMembership{}, err
	}
	commits, err := rangeCommits(ctx, reviewTarget.RepoDir, base.SHA, reviewTarget.BaseSHA, commands)
	if err != nil {
		return changeMembership{}, err
	}
	return changeMembership{uncommitted: true, commits: commits, baseKnown: true}, nil
}

func rangeCommits(ctx context.Context, repoDir, baseSHA, headSHA string, commands runner.Runner) (map[string]struct{}, error) {
	args := []string{"rev-list", baseSHA + ".." + headSHA}
	result, err := commands.Run(ctx, repoDir, "git", args...)
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
	return commits, nil
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
