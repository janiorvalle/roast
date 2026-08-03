package target

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janiorvalle/roast/internal/runner"
)

func TestResolveRejectsConflictingSelectors(t *testing.T) {
	_, err := Resolve(context.Background(), Options{
		Dirty:   true,
		Base:    "main",
		RepoDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "ROAST-TARGET-CONFLICT") {
		t.Fatalf("error = %v, want selector conflict", err)
	}
}

func TestResolveDirtyUsesHEADAsDiffBase(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, repo, "tracked.txt", "changed\n")
	writeFile(t, repo, "new.txt", "new\n")

	got, err := Resolve(context.Background(), Options{Dirty: true, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindDirty || got.HeadRef != "WORKTREE" || got.SnapshotRef != got.BaseSHA || got.Range != "HEAD (uncommitted)" {
		t.Fatalf("target = %#v", got)
	}
}

func TestResolveBaseAndCommitTargets(t *testing.T) {
	repo := initRepository(t)
	baseSHA := gitOutput(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "switch", "-c", "feature")
	writeFile(t, repo, "feature.txt", "feature\n")
	runGit(t, repo, "add", "feature.txt")
	runGit(t, repo, "commit", "-m", "feature")
	headSHA := gitOutput(t, repo, "rev-parse", "HEAD")

	baseTarget, err := Resolve(context.Background(), Options{Base: "main", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if baseTarget.Kind != KindBase || baseTarget.BaseSHA != baseSHA || baseTarget.HeadSHA != headSHA || !baseTarget.ThreeDotDiff {
		t.Fatalf("base target = %#v", baseTarget)
	}

	commitTarget, err := Resolve(context.Background(), Options{Commit: "HEAD", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if commitTarget.Kind != KindCommit || commitTarget.BaseSHA != baseSHA || commitTarget.HeadSHA != headSHA || commitTarget.RootCommit {
		t.Fatalf("commit target = %#v", commitTarget)
	}
}

func TestResolveCommitRejectsShallowParentBoundary(t *testing.T) {
	origin := t.TempDir()
	runGit(t, origin, "init", "-b", "main")
	runGit(t, origin, "config", "user.email", "test@example.com")
	runGit(t, origin, "config", "user.name", "Roast Test")
	writeFile(t, origin, "base.txt", "base\n")
	runGit(t, origin, "add", "base.txt")
	runGit(t, origin, "commit", "-m", "base")
	writeFile(t, origin, "base.txt", "second\n")
	runGit(t, origin, "commit", "-am", "second")

	cloneParent := t.TempDir()
	shallow := filepath.Join(cloneParent, "shallow")
	runGit(t, cloneParent, "clone", "--depth", "1", "--no-local", origin, shallow)
	_, err := Resolve(context.Background(), Options{Commit: "HEAD", RepoDir: shallow})
	if err == nil || !strings.Contains(err.Error(), "ROAST-TARGET-SHALLOW") {
		t.Fatalf("error = %v, want shallow-boundary guidance", err)
	}
}

func TestResolvePullRequestParsesGHMetadata(t *testing.T) {
	repo := t.TempDir()
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	ghJSON, err := json.Marshal(map[string]any{
		"number":      17,
		"title":       "Add a better gate",
		"baseRefName": "main",
		"headRefName": "feature",
		"baseRefOid":  baseSHA,
		"headRefOid":  headSHA,
		"url":         "https://github.com/example/roast/pull/17",
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := fakeRunner{responses: map[string]runner.Result{
		"git rev-parse --show-toplevel": {Stdout: []byte(repo + "\n")},
		"gh pr view 17 --json number,title,baseRefName,headRefName,baseRefOid,headRefOid,url": {Stdout: ghJSON},
	}}

	got, err := Resolve(context.Background(), Options{PullRequest: "17", RepoDir: repo, Runner: commands})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindPullRequest || got.PRNumber != 17 || got.BaseSHA != baseSHA || got.HeadSHA != headSHA || got.SnapshotRef != headSHA {
		t.Fatalf("target = %#v", got)
	}
}

func TestResolveAutoPrefersCurrentPullRequestBeforeOriginMain(t *testing.T) {
	repo := t.TempDir()
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	commands := fakeRunner{responses: map[string]runner.Result{
		"git rev-parse --show-toplevel":                                                    {Stdout: []byte(repo + "\n")},
		"git status --porcelain=v1 --untracked-files=all":                                  {},
		"gh pr view --json number,title,baseRefName,headRefName,baseRefOid,headRefOid,url": {Stdout: []byte(fmt.Sprintf(`{"number":17,"title":"Auto PR","baseRefName":"main","headRefName":"feature","baseRefOid":"%s","headRefOid":"%s","url":"https://github.com/example/roast/pull/17"}`, baseSHA, headSHA))},
	}}

	got, err := Resolve(context.Background(), Options{RepoDir: repo, Runner: commands})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindPullRequest || got.PRNumber != 17 {
		t.Fatalf("target = %#v", got)
	}
}

func TestResolveAutoFallsThroughForGenuineNoPullRequest(t *testing.T) {
	repo := t.TempDir()
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	commands := fakeRunner{responses: map[string]runner.Result{
		"git rev-parse --show-toplevel":                                                    {Stdout: []byte(repo + "\n")},
		"git status --porcelain=v1 --untracked-files=all":                                  {},
		"gh pr view --json number,title,baseRefName,headRefName,baseRefOid,headRefOid,url": {ExitCode: 1, Stderr: []byte(`no pull requests found for branch "feature"`)},
		"git rev-parse --verify --quiet --end-of-options origin/main^{commit}":             {Stdout: []byte(baseSHA + "\n")},
		"git rev-parse --verify --quiet --end-of-options HEAD^{commit}":                    {Stdout: []byte(headSHA + "\n")},
	}}

	got, err := Resolve(context.Background(), Options{RepoDir: repo, Runner: commands})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindBase || got.BaseRef != "origin/main" || got.BaseSHA != baseSHA || got.HeadSHA != headSHA {
		t.Fatalf("target = %#v", got)
	}
}

func TestResolveAutoSurfacesPullRequestLookupFailure(t *testing.T) {
	repo := t.TempDir()
	commands := fakeRunner{responses: map[string]runner.Result{
		"git rev-parse --show-toplevel":                                                    {Stdout: []byte(repo + "\n")},
		"git status --porcelain=v1 --untracked-files=all":                                  {},
		"gh pr view --json number,title,baseRefName,headRefName,baseRefOid,headRefOid,url": {ExitCode: 1, Stderr: []byte("network timeout")},
	}}

	_, err := Resolve(context.Background(), Options{RepoDir: repo, Runner: commands})
	if err == nil || !strings.Contains(err.Error(), "ROAST-TARGET-GH") || !strings.Contains(err.Error(), "network timeout") {
		t.Fatalf("error = %v, want the GitHub CLI failure", err)
	}
}

func TestIsNoCurrentPullRequestRecognizesGHDiagnosticVariants(t *testing.T) {
	for _, message := range []string{
		`no pull requests found for branch "feature"`,
		`no open pull requests found for branch "feature"`,
		"could not determine current branch: failed to run git: not on any branch",
	} {
		if !isNoCurrentPullRequest(runner.Result{Stderr: []byte(message)}) {
			t.Errorf("diagnostic %q was not recognized as no current pull request", message)
		}
	}
	if isNoCurrentPullRequest(runner.Result{Stderr: []byte("no git remotes found")}) {
		t.Error("operational GitHub CLI failure was treated as no current pull request")
	}
}

type fakeRunner struct {
	responses map[string]runner.Result
}

func (f fakeRunner) Run(_ context.Context, _ string, program string, args ...string) (runner.Result, error) {
	key := strings.TrimSpace(program + " " + strings.Join(args, " "))
	result, ok := f.responses[key]
	if !ok {
		return runner.Result{ExitCode: 1, Stderr: []byte("unexpected command: " + key)}, nil
	}
	return result, nil
}

func (f fakeRunner) RunWithInputStream(ctx context.Context, _ string, _ []byte, _ io.Writer, program string, args ...string) (runner.Result, error) {
	return f.Run(ctx, "", program, args...)
}

func initRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Roast Test")
	writeFile(t, repo, "base.txt", "base\n")
	runGit(t, repo, "add", "base.txt")
	runGit(t, repo, "commit", "-m", "base")
	return repo
}

func writeFile(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
