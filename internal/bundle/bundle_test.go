package bundle

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
)

func TestSanitizeDiffForPromptReplacesInvalidUTF8AndNamesFiles(t *testing.T) {
	diff := []byte("diff --git a/README.md b/README.md\nindex 1111111..2222222 100644\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-")
	diff = append(diff, []byte("+R")...)
	diff = append(diff, 0xe9)
	diff = append(diff, []byte("sum")...)
	diff = append(diff, 0xe9, '\n')

	sanitized, paths := SanitizeDiffForPrompt(diff)
	if !utf8.ValidString(sanitized) {
		t.Fatalf("sanitized diff is not valid UTF-8: %q", sanitized)
	}
	if !strings.Contains(sanitized, "R\uFFFDsum\uFFFD") {
		t.Fatalf("sanitized diff = %q, want replacement characters", sanitized)
	}
	if len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("invalid UTF-8 paths = %#v, want README.md", paths)
	}
}

func TestSanitizeDiffForPromptPreservesValidDiff(t *testing.T) {
	diff := []byte("diff --git a/main.go b/main.go\n@@ -1 +1 @@\n-package main\n+package review\n")
	got, paths := SanitizeDiffForPrompt(diff)
	if got != string(diff) {
		t.Fatalf("sanitized valid diff = %q, want original", got)
	}
	if len(paths) != 0 {
		t.Fatalf("invalid UTF-8 paths = %#v, want none", paths)
	}
}

func TestBuildDirtyBundleIncludesUntrackedDiffButNotIgnoredSnapshotFiles(t *testing.T) {
	repo := initBundleRepository(t)
	runBundleGit(t, repo, "config", "diff.noprefix", "true")
	writeBundleFile(t, repo, ".gitignore", "ignored.txt\n")
	runBundleGit(t, repo, "add", ".gitignore")
	runBundleGit(t, repo, "commit", "-m", "ignore rules")
	writeBundleFile(t, repo, "base.txt", "changed\n")
	writeBundleFile(t, repo, "new.txt", "new\n")
	writeBundleFile(t, repo, "ignored.txt", "do not include\n")

	reviewTarget, err := target.Resolve(context.Background(), target.Options{Dirty: true, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	diff := string(reviewBundle.Diff)
	if !strings.Contains(diff, "base.txt") || !strings.Contains(diff, "new.txt") {
		t.Fatalf("diff does not contain tracked and untracked changes:\n%s", diff)
	}
	if strings.Contains(diff, "ignored.txt") {
		t.Fatalf("ignored file leaked into diff:\n%s", diff)
	}

	entries := tarEntries(t, reviewBundle.Snapshot)
	if !entries["base.txt"] || !entries[".gitignore"] {
		t.Fatalf("snapshot entries = %#v", entries)
	}
	if entries["ignored.txt"] || entries[".git"] {
		t.Fatalf("ignored files leaked into snapshot: %#v", entries)
	}
	files := snapshotFiles(t, reviewBundle.Snapshot)
	if got := string(files["base.txt"]); got != "changed\n" {
		t.Fatalf("base.txt snapshot = %q, want worktree bytes", got)
	}
	if got := string(files["new.txt"]); got != "new\n" {
		t.Fatalf("new.txt snapshot = %q, want untracked worktree bytes", got)
	}
	if _, ok := files["ignored.txt"]; ok {
		t.Fatal("ignored.txt leaked into snapshot content")
	}
}

func TestBuildExcludesSensitiveFilesFromDiffAndSnapshot(t *testing.T) {
	repo := initBundleRepository(t)
	writeBundleFile(t, repo, ".env.local", "TOKEN=fixture-only\n")
	writeBundleFile(t, repo, filepath.Join(".aws", "credentials"), "[default]\naws_secret_access_key=fixture-only\n")
	runBundleGit(t, repo, "add", ".env.local", ".aws/credentials")
	runBundleGit(t, repo, "commit", "-m", "local credential stores")
	writeBundleFile(t, repo, ".env.local", "TOKEN=changed-fixture-only\n")
	writeBundleFile(t, repo, filepath.Join(".aws", "credentials"), "[default]\naws_secret_access_key=changed-fixture-only\n")
	writeBundleFile(t, repo, ".env.untracked", "TOKEN=untracked-fixture-only\n")
	writeBundleFile(t, repo, "safe.txt", "safe\n")

	reviewTarget, err := target.Resolve(context.Background(), target.Options{Dirty: true, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	diff := string(reviewBundle.Diff)
	for _, forbidden := range []string{".env.local", ".aws/credentials", ".env.untracked", "changed-fixture-only", "untracked-fixture-only"} {
		if strings.Contains(diff, forbidden) {
			t.Fatalf("sensitive content %q leaked into diff:\n%s", forbidden, diff)
		}
	}
	if !strings.Contains(diff, "safe.txt") {
		t.Fatalf("safe change missing from diff:\n%s", diff)
	}
	if !strings.Contains(string(reviewBundle.SecretScanDiff), "changed-fixture-only") || !strings.Contains(string(reviewBundle.SecretScanDiff), "untracked-fixture-only") {
		t.Fatal("secret scan diff did not retain changed sensitive content for local scanning")
	}
	excluded := make(map[string]struct{}, len(reviewBundle.ExcludedSensitivePaths))
	for _, name := range reviewBundle.ExcludedSensitivePaths {
		excluded[name] = struct{}{}
	}
	for _, name := range []string{".env.local", ".aws/credentials", ".env.untracked"} {
		if _, exists := excluded[name]; !exists {
			t.Fatalf("excluded sensitive paths = %#v, missing %q", reviewBundle.ExcludedSensitivePaths, name)
		}
	}
	files := snapshotFiles(t, reviewBundle.Snapshot)
	for _, forbidden := range []string{".env.local", ".aws/credentials", ".env.untracked"} {
		if _, exists := files[forbidden]; exists {
			t.Fatalf("sensitive path %q leaked into snapshot", forbidden)
		}
	}
}

func TestFilterSensitiveDiffUsesFileHeadersForAmbiguousRename(t *testing.T) {
	diff := []byte("diff --git a/old b/file b/.env.local\nindex 1111111..2222222 100644\n--- a/old b/file\n+++ b/.env.local\n@@ -1 +1 @@\n-TOKEN=old\n+TOKEN=changed\n")

	if _, _, err := filterSensitiveDiff(diff); err == nil || !strings.Contains(err.Error(), "ROAST-BUNDLE-SENSITIVE-RENAME") {
		t.Fatalf("error = %v, want sensitive rename rejection", err)
	}
}

func TestFilterSensitiveDiffAllowsDeletingSensitiveFileAfterLocalScan(t *testing.T) {
	diff := []byte("diff --git a/.env.local b/.env.local\ndeleted file mode 100644\nindex 1111111..0000000\n--- a/.env.local\n+++ /dev/null\n@@ -1 +0,0 @@\n-TOKEN=revoked-fixture\n")

	filtered, excluded, err := filterSensitiveDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 || len(excluded) != 0 {
		t.Fatalf("filtered deletion = %q, excluded = %#v; want no review content or fatal exclusion", filtered, excluded)
	}
}

func TestFilterSensitiveDiffDoesNotTreatHunkContentAsDeletion(t *testing.T) {
	diff := []byte("diff --git a/.env.local b/.env.local\nindex 1111111..2222222 100644\n--- a/.env.local\n+++ b/.env.local\n@@ -1 +1 @@\n-OLD=fixture\n+++ /dev/null\n")

	filtered, excluded, err := filterSensitiveDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 || len(excluded) != 1 || excluded[0] != ".env.local" {
		t.Fatalf("filtered sensitive change = %q, excluded = %#v; want a fatal sensitive exclusion", filtered, excluded)
	}
}

func TestFilterSensitiveDiffParsesQuotedPathEndingInBackslash(t *testing.T) {
	diff := []byte(`diff --git "a/deleted\\" "b/deleted\\"` + "\n")

	filtered, excluded, err := filterSensitiveDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if string(filtered) != string(diff) {
		t.Fatalf("filtered diff = %q, want original diff", filtered)
	}
	if len(excluded) != 0 {
		t.Fatalf("excluded paths = %#v, want none", excluded)
	}
}

func TestFilterSensitiveDiffUsesRenameHeadersForAmbiguousRenameOnly(t *testing.T) {
	diff := []byte("diff --git a/old b/file b/.env.local\nsimilarity index 100%\nrename from old b/file\nrename to .env.local\n")

	if _, _, err := filterSensitiveDiff(diff); err == nil || !strings.Contains(err.Error(), "ROAST-BUNDLE-SENSITIVE-RENAME") {
		t.Fatalf("error = %v, want sensitive rename rejection", err)
	}
}

func TestFilterSensitiveDiffUsesFileHeadersForAmbiguousSensitivePath(t *testing.T) {
	diff := []byte("diff --git a/dir b/.env.local b/dir b/.env.local\nindex 1111111..2222222 100644\n--- a/dir b/.env.local\n+++ b/dir b/.env.local\n@@ -1 +1 @@\n-TOKEN=old\n+TOKEN=changed\n")

	filtered, _, err := filterSensitiveDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered sensitive modification = %q, want empty diff", filtered)
	}
}

func TestBuildBranchBundleUsesThreeDotDiffAndHeadSnapshot(t *testing.T) {
	repo := initBundleRepository(t)
	baseSHA := bundleGitOutput(t, repo, "rev-parse", "HEAD")
	runBundleGit(t, repo, "switch", "-c", "feature")
	writeBundleFile(t, repo, "feature.txt", "feature\n")
	runBundleGit(t, repo, "add", "feature.txt")
	runBundleGit(t, repo, "commit", "-m", "feature")
	headSHA := bundleGitOutput(t, repo, "rev-parse", "HEAD")

	reviewTarget := target.Target{
		Kind:         target.KindBase,
		RepoDir:      repo,
		BaseSHA:      baseSHA,
		HeadSHA:      headSHA,
		SnapshotRef:  headSHA,
		Range:        "main...HEAD",
		ThreeDotDiff: true,
	}
	reviewBundle, err := Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewBundle.Diff), "feature.txt") {
		t.Fatalf("diff = %s", reviewBundle.Diff)
	}
	if !tarEntries(t, reviewBundle.Snapshot)["feature.txt"] {
		t.Fatalf("head snapshot does not contain feature.txt")
	}
}

func TestBundleWriteProducesReviewArtifacts(t *testing.T) {
	repo := initBundleRepository(t)
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Base: "HEAD", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "bundle")
	if err := reviewBundle.Write(destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"diff.patch", "snapshot.tar", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestBundleWriteReplacesArtifactsWithPrivateFiles(t *testing.T) {
	destination := t.TempDir()
	for _, name := range []string{"diff.patch", "snapshot.tar", "manifest.json"} {
		path := filepath.Join(destination, name)
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Bundle{Diff: []byte("diff"), Snapshot: []byte("snapshot")}).Write(destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"diff.patch", "snapshot.tar", "manifest.json"} {
		if runtime.GOOS == "windows" {
			continue
		}
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, got)
		}
	}
}

func TestBundleWriteDoesNotDeleteArtifactDirectories(t *testing.T) {
	destination := t.TempDir()
	artifactDirectory := filepath.Join(destination, "diff.patch")
	if err := os.Mkdir(artifactDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	err := (Bundle{Diff: []byte("diff"), Snapshot: []byte("snapshot")}).Write(destination)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("error = %v, want directory collision rejection", err)
	}
	if info, statErr := os.Stat(artifactDirectory); statErr != nil || !info.IsDir() {
		t.Fatalf("artifact directory was removed: info=%v err=%v", info, statErr)
	}
}

func TestBundleWriteRejectsRepositoryDestination(t *testing.T) {
	repo := initBundleRepository(t)
	err := (Bundle{Target: target.Target{RepoDir: repo}}).Write(filepath.Join(repo, ".roast"))
	if err == nil || !strings.Contains(err.Error(), "inside repository") {
		t.Fatalf("error = %v, want repository destination rejection", err)
	}
}

func TestBundleWriteAllowsSymlinkedRepositoryDestination(t *testing.T) {
	repo := initBundleRepository(t)
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	destination := filepath.Join(link, "generated")
	if err := (Bundle{Target: target.Target{RepoDir: repo}}).Write(destination); err != nil {
		t.Fatalf("error = %v, want symlinked destination accepted by the simple path check", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "manifest.json")); err != nil {
		t.Fatalf("manifest = %v", err)
	}
}

func TestFetchRemoteMatchesPRRepository(t *testing.T) {
	repo := t.TempDir()
	commands := &fakeBundleRunner{responses: map[string]runner.Result{
		"git remote":                  {Stdout: []byte("origin\nupstream\n")},
		"git remote get-url origin":   {Stdout: []byte("git@github.com:contributor/roast.git\n")},
		"git remote get-url upstream": {Stdout: []byte("https://github.com/example/roast.git\n")},
	}}
	got, err := fetchRemote(context.Background(), target.Target{RepoDir: repo, PRURL: "https://github.com/example/roast/pull/17"}, commands)
	if err != nil {
		t.Fatal(err)
	}
	if got != "upstream" {
		t.Fatalf("remote = %q, want upstream", got)
	}
}

func TestBuildRejectsThreeDotTargetWithoutMergeBase(t *testing.T) {
	repo := t.TempDir()
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	commands := &fakeBundleRunner{responses: map[string]runner.Result{
		"git merge-base " + baseSHA + " " + headSHA: {ExitCode: 1},
	}}
	_, err := Build(context.Background(), target.Target{
		Kind:         target.KindBase,
		RepoDir:      repo,
		BaseSHA:      baseSHA,
		HeadSHA:      headSHA,
		SnapshotRef:  headSHA,
		ThreeDotDiff: true,
	}, commands)
	if err == nil || !strings.Contains(err.Error(), "ROAST-BUNDLE-MERGE-BASE") {
		t.Fatalf("error = %v, want merge-base guidance", err)
	}
}

func TestRootCommitDiffRecursesIntoSubdirectories(t *testing.T) {
	repo := t.TempDir()
	runBundleGit(t, repo, "init", "-b", "main")
	runBundleGit(t, repo, "config", "user.email", "test@example.com")
	runBundleGit(t, repo, "config", "user.name", "Roast Test")
	writeBundleFile(t, repo, filepath.Join("cmd", "roast", "main.go"), "package main\n")
	runBundleGit(t, repo, "add", ".")
	runBundleGit(t, repo, "commit", "-m", "root")
	rootSHA := bundleGitOutput(t, repo, "rev-parse", "HEAD")
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Commit: rootSHA, RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewBundle.Diff), "cmd/roast/main.go") {
		t.Fatalf("root commit diff = %s", reviewBundle.Diff)
	}
}

func initBundleRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runBundleGit(t, repo, "init", "-b", "main")
	runBundleGit(t, repo, "config", "user.email", "test@example.com")
	runBundleGit(t, repo, "config", "user.name", "Roast Test")
	writeBundleFile(t, repo, "base.txt", "base\n")
	runBundleGit(t, repo, "add", "base.txt")
	runBundleGit(t, repo, "commit", "-m", "base")
	return repo
}

func writeBundleFile(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bundleGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func runBundleGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

type fakeBundleRunner struct {
	responses map[string]runner.Result
	inputs    [][]byte
}

func (f fakeBundleRunner) Run(_ context.Context, _ string, program string, args ...string) (runner.Result, error) {
	key := strings.TrimSpace(program + " " + strings.Join(args, " "))
	result, ok := f.responses[key]
	if !ok {
		return runner.Result{ExitCode: 1, Stderr: []byte("unexpected command: " + key)}, nil
	}
	return result, nil
}

func (f *fakeBundleRunner) RunWithInput(ctx context.Context, _ string, input []byte, program string, args ...string) (runner.Result, error) {
	f.inputs = append(f.inputs, append([]byte(nil), input...))
	return f.Run(ctx, "", program, args...)
}

func (f *fakeBundleRunner) RunWithInputStream(ctx context.Context, _ string, input []byte, stdout io.Writer, program string, args ...string) (runner.Result, error) {
	f.inputs = append(f.inputs, append([]byte(nil), input...))
	result, err := f.Run(ctx, "", program, args...)
	if err != nil {
		return result, err
	}
	if _, err := stdout.Write(result.Stdout); err != nil {
		return runner.Result{}, err
	}
	result.Stdout = nil
	return result, nil
}

func tarEntries(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	entries := map[string]bool{}
	reader := tar.NewReader(strings.NewReader(string(data)))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = true
	}
}
