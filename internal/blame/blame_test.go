package blame

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janiorvalle/roast/internal/bundle"
	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
	"github.com/janiorvalle/roast/internal/verdict"
)

// fixture is a repository with a base commit on main holding lib.go with
// three lines, and a task branch whose one commit rewrites line 2.
func fixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Roast Test")
	write(t, repo, "lib.go", "line one\nline two\nline three\n")
	git(t, repo, "add", "lib.go")
	git(t, repo, "commit", "-m", "base")
	git(t, repo, "checkout", "-q", "-b", "task")
	write(t, repo, "lib.go", "line one\nline two changed\nline three\n")
	git(t, repo, "commit", "-am", "task")
	return repo
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func write(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolve(t *testing.T, repo string, options target.Options) (target.Target, []byte) {
	t.Helper()
	options.RepoDir = repo
	options.Runner = runner.ExecRunner{}
	reviewTarget, err := target.Resolve(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := bundle.Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	return reviewTarget, reviewBundle.Diff
}

func finding(file string, line int) verdict.Finding {
	return verdict.Finding{Priority: verdict.PriorityP1, File: file, Line: line, Title: "t", Rationale: "r"}
}

func TestTagMarksLinesOlderThanTheBaseRange(t *testing.T) {
	repo := fixture(t)
	reviewTarget, diff := resolve(t, repo, target.Options{Base: "main"})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 1), finding("lib.go", 2)}, runner.ExecRunner{})
	if len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	if !tagged[0].PreExisting || tagged[1].PreExisting {
		t.Fatalf("tagged = %#v", tagged)
	}
}

func TestTagMarksCommittedLinesForADirtyReview(t *testing.T) {
	repo := fixture(t)
	write(t, repo, "lib.go", "line one\nline two changed\nline three edited\n")
	write(t, repo, "fresh.go", "brand new\n")
	reviewTarget, diff := resolve(t, repo, target.Options{Dirty: true})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 1), finding("lib.go", 3), finding("fresh.go", 1)}, runner.ExecRunner{})
	if len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	if !tagged[0].PreExisting || tagged[1].PreExisting || tagged[2].PreExisting {
		t.Fatalf("tagged = %#v", tagged)
	}
}

func TestTagMarksLinesFromOtherCommitsForACommitReview(t *testing.T) {
	repo := fixture(t)
	reviewTarget, diff := resolve(t, repo, target.Options{Commit: "HEAD"})
	tagged, _ := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 1), finding("lib.go", 2)}, runner.ExecRunner{})
	if !tagged[0].PreExisting || tagged[1].PreExisting {
		t.Fatalf("tagged = %#v", tagged)
	}
}

func TestTagLeavesARootCommitUntagged(t *testing.T) {
	repo := fixture(t)
	root := strings.TrimSpace(output(t, repo, "rev-list", "--max-parents=0", "HEAD"))
	reviewTarget, diff := resolve(t, repo, target.Options{Commit: root})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 1)}, runner.ExecRunner{})
	if tagged[0].PreExisting || len(notices) != 0 {
		t.Fatalf("tagged = %#v, notices = %v", tagged, notices)
	}
}

func TestTagReportsALineItCannotBlameAndLeavesItOpen(t *testing.T) {
	repo := fixture(t)
	reviewTarget, diff := resolve(t, repo, target.Options{Base: "main"})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 99)}, runner.ExecRunner{})
	if tagged[0].PreExisting {
		t.Fatalf("tagged = %#v", tagged)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "ROAST-BLAME") || !strings.Contains(notices[0], "lib.go:99") {
		t.Fatalf("notices = %v", notices)
	}
}

func output(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	out, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func TestTagLeavesADeletedOldSideLineUntaggedWithANotice(t *testing.T) {
	repo := fixture(t)
	write(t, repo, "lib.go", "line one\nline three\n")
	git(t, repo, "commit", "-qam", "delete line two")
	reviewTarget, diff := resolve(t, repo, target.Options{Base: "main"})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 2), finding("lib.go", 1)}, runner.ExecRunner{})
	if tagged[0].PreExisting {
		t.Fatalf("a finding on a line this change deleted must not be tagged: %#v", tagged[0])
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "lib.go:2") || !strings.Contains(notices[0], "deleted") {
		t.Fatalf("notices = %v", notices)
	}
	if !tagged[1].PreExisting {
		t.Fatalf("line one predates the change: %#v", tagged[1])
	}
}

// A dirty review in land-pr's loop sits on top of the task's own commits, so
// those commits are the change too, not history.
func TestTagCountsTheBranchCommitsAsTheChangeInADirtyReview(t *testing.T) {
	repo := fixture(t)
	write(t, repo, "lib.go", "line one\nline two changed\nline three\nuncommitted fix\n")
	reviewTarget, diff := resolve(t, repo, target.Options{Dirty: true})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 1), finding("lib.go", 2), finding("lib.go", 4)}, runner.ExecRunner{})
	if len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	if !tagged[0].PreExisting {
		t.Fatalf("line one is the base's: %#v", tagged[0])
	}
	if tagged[1].PreExisting {
		t.Fatalf("line two is the task's own commit on the branch: %#v", tagged[1])
	}
	if tagged[2].PreExisting {
		t.Fatalf("line four is uncommitted: %#v", tagged[2])
	}
}

func TestTagLeavesCommittedLinesUntaggedWithANoticeWhenADirtyReviewHasNoBase(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-b", "trunk")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Roast Test")
	write(t, repo, "lib.go", "line one\n")
	git(t, repo, "add", "lib.go")
	git(t, repo, "commit", "-m", "base")
	write(t, repo, "lib.go", "line one\nuncommitted\n")
	reviewTarget, diff := resolve(t, repo, target.Options{Dirty: true})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 1), finding("lib.go", 2)}, runner.ExecRunner{})
	if tagged[0].PreExisting || tagged[1].PreExisting {
		t.Fatalf("no base means no tag, never a wrong one: %#v", tagged)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "ROAST-BLAME") || !strings.Contains(notices[0], "origin/main") {
		t.Fatalf("notices = %v", notices)
	}
}

func TestTagInADirtyReviewTreatsLinesTheBranchCommitsDeletedAsAmbiguous(t *testing.T) {
	repo := fixture(t)
	write(t, repo, "lib.go", "line one\nline three\n")
	git(t, repo, "commit", "-qam", "task deletes line two")
	write(t, repo, "lib.go", "line one\nline three\nuncommitted\n")
	reviewTarget, diff := resolve(t, repo, target.Options{Dirty: true})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 2), finding("lib.go", 1)}, runner.ExecRunner{})
	if tagged[0].PreExisting {
		t.Fatalf("line 2 was deleted by a branch commit, so the finding may mean the deleted line: %#v", tagged[0])
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "lib.go:2") || !strings.Contains(notices[0], "deleted") {
		t.Fatalf("notices = %v", notices)
	}
	if !tagged[1].PreExisting {
		t.Fatalf("line one predates the change: %#v", tagged[1])
	}
}

func TestTagInADirtyReviewIgnoresEditsTheBaseMadeAfterTheBranchDiverged(t *testing.T) {
	repo := fixture(t)
	git(t, repo, "checkout", "-q", "main")
	write(t, repo, "lib.go", "line one\nline two\nline three upstream\n")
	git(t, repo, "commit", "-qam", "upstream edits line three")
	git(t, repo, "checkout", "-q", "task")
	write(t, repo, "lib.go", "line one\nline two changed\nline three\nuncommitted\n")
	reviewTarget, diff := resolve(t, repo, target.Options{Dirty: true})
	tagged, notices := Tag(context.Background(), reviewTarget, diff, []verdict.Finding{finding("lib.go", 3)}, runner.ExecRunner{})
	if len(notices) != 0 || !tagged[0].PreExisting {
		t.Fatalf("an upstream-only edit is not the branch's deletion: tagged = %#v, notices = %v", tagged, notices)
	}
}
