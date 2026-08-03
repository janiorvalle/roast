package secrets

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
)

func TestScanBlocksVerifiedSecretAndDoesNotExposeSecretValue(t *testing.T) {
	const fixtureSecret = "ghs_TEST_FIXTURE_ONLY"
	scanner := &captureScannerRunner{result: runner.Result{
		ExitCode: verifiedExitCode,
		Stdout:   []byte(`{"Verified":true,"DetectorName":"GitHub","SourceMetadata":{"Data":{"Filesystem":{"file":"/tmp/roast-secret-scan/config.txt"}}}}` + "\n"),
	}}
	diff := []byte("diff --git a/config.txt b/config.txt\n--- a/config.txt\n+++ b/config.txt\n@@ -1,2 +1,2 @@\n access_key_id=fixture-only\n-existing=fixture-only\n+token=\"" + fixtureSecret + "\"\n")

	err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner)
	t.Logf("gate result: %v", err)
	if err == nil || !strings.Contains(err.Error(), "ROAST-SECRET-VERIFIED") || !strings.Contains(err.Error(), "config.txt") {
		t.Fatalf("error = %v, want verified-secret guidance", err)
	}
	if strings.Contains(err.Error(), fixtureSecret) {
		t.Fatalf("error exposed fixture secret: %v", err)
	}
	if got := string(scanner.files["config.txt"]); got != "access_key_id=fixture-only\n"+"existing=fixture-only\n"+"token=\""+fixtureSecret+"\"\n" {
		t.Fatalf("scanned added content = %q", got)
	}
}

func TestScanPassesCleanAddedContentAndScansSensitivePathsLocally(t *testing.T) {
	scanner := &captureScannerRunner{}
	diff := []byte("diff --git a/.env.local b/.env.local\n--- /dev/null\n+++ b/.env.local\n@@ -0,0 +1 @@\n+TOKEN=not-for-review\n" +
		"diff --git a/config.txt b/config.txt\n--- /dev/null\n+++ b/config.txt\n@@ -0,0 +1 @@\n+safe\n")

	if err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if got := string(scanner.files["config.txt"]); got != "safe\n" {
		t.Fatalf("scanned config content = %q", got)
	}
	if got := string(scanner.files[".env.local"]); got != "TOKEN=not-for-review\n" {
		t.Fatalf("sensitive path was not scanned locally: %#v", scanner.files)
	}
	if scanner.program != "fake-trufflehog" || len(scanner.args) != 8 || scanner.args[0] != "filesystem" || scanner.args[5] != "--fail" {
		t.Fatalf("TruffleHog invocation = %s %#v", scanner.program, scanner.args)
	}
}

func TestScanIncludesDeletedContent(t *testing.T) {
	scanner := &captureScannerRunner{}
	diff := []byte("diff --git a/deleted.txt b/deleted.txt\n--- a/deleted.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-deleted-secret-fixture\n")

	if err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if got := string(scanner.files["deleted.txt"]); got != "deleted-secret-fixture\n" {
		t.Fatalf("deleted content = %q, want the removed file content", got)
	}
}

func TestScanDecodesGitQuotedPath(t *testing.T) {
	scanner := &captureScannerRunner{}
	diff := []byte("diff --git \"a/caf\\303\\251.txt\" \"b/caf\\303\\251.txt\"\n--- \"a/caf\\303\\251.txt\"\n+++ \"b/caf\\303\\251.txt\"\n@@ -0,0 +1 @@\n+safe\n")

	if err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if got := string(scanner.files["café.txt"]); got != "safe\n" {
		t.Fatalf("quoted path content = %q, want decoded Git path", got)
	}
}

func TestScanMaterializesBinaryPostimage(t *testing.T) {
	const fixtureSecret = "binary-secret-fixture"
	scanner := &captureScannerRunner{}
	commands := &binaryPostimageRunner{scanner: scanner, content: []byte(fixtureSecret)}
	diff := []byte("diff --git a/image.bin b/image.bin\nindex 1111111..2222222\nGIT binary patch\nliteral 20\nnot-used-by-scanner\n")
	reviewTarget := target.Target{Kind: target.KindCommit, RepoDir: t.TempDir(), SnapshotRef: "head"}

	if err := scanWithTarget(context.Background(), reviewTarget, diff, nil, "fake-trufflehog", commands); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if !containsFileContent(scanner.files, fixtureSecret) {
		t.Fatalf("binary postimage files = %#v, want target bytes", scanner.files)
	}
	if len(commands.args) != 3 || commands.args[0] != "cat-file" || commands.args[1] != "blob" || commands.args[2] != "head:image.bin" {
		t.Fatalf("binary materialization command = %#v", commands.args)
	}
}

func TestBinaryPostimagePathsHandleSpacesInUnquotedGitHeader(t *testing.T) {
	diff := []byte("diff --git a/assets b/image.bin b/assets b/image.bin\nindex 1111111..2222222\nGIT binary patch\nliteral 20\nnot-used-by-scanner\n")

	files, err := reviewDiffFiles(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].newPath != "assets b/image.bin" {
		t.Fatalf("binary diff files = %#v, want the complete path", files)
	}
}

func TestBinaryPostimagePathsHandleMixedGitHeaderQuoting(t *testing.T) {
	diff := []byte("diff --git \"a/old\\303\\251.bin\" b/new.bin\nindex 1111111..2222222\nGIT binary patch\nliteral 20\nnot-used-by-scanner\n")

	files, err := reviewDiffFiles(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].newPath != "new.bin" {
		t.Fatalf("binary diff files = %#v, want the unquoted postimage path", files)
	}
}

func TestReviewDiffFilesPreservesTrailingPathSpaces(t *testing.T) {
	diff := []byte("diff --git a/image.bin  b/image.bin \nindex 1111111..2222222\nGIT binary patch\nliteral 20\nnot-used-by-scanner\n")

	files, err := reviewDiffFiles(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].newPath != "image.bin " {
		t.Fatalf("binary diff files = %#v, want trailing path space preserved", files)
	}
}

func TestScanSnapshotFilesPreservesSymlinkTarget(t *testing.T) {
	var snapshot bytes.Buffer
	archive := tar.NewWriter(&snapshot)
	if err := archive.WriteHeader(&tar.Header{Name: "link.txt", Typeflag: tar.TypeSymlink, Linkname: "../outside"}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := scanSnapshotFiles(snapshot.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(files["link.txt"]); got != "../outside" {
		t.Fatalf("symlink scan content = %q, want link target", got)
	}
}

func TestBoundedScanNameDoesNotUseGitBasename(t *testing.T) {
	name := boundedScanName("head", 0, "config:prod?.txt")
	if strings.ContainsAny(name, `:/\\?*[]`) || len(name) > 200 {
		t.Fatalf("bounded scan name = %q, want an OS-safe short name", name)
	}
}

func TestReviewDiffFilesSkipsGitlinks(t *testing.T) {
	diff := []byte("diff --git a/vendor/tool b/vendor/tool\nindex 1111111..2222222 160000\n--- a/vendor/tool\n+++ b/vendor/tool\n@@ -1 +1 @@\n-old\n+new\n")

	files, err := reviewDiffFiles(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].oldGitlink || !files[0].newGitlink {
		t.Fatalf("diff files = %#v, want a gitlink entry", files)
	}
}

func TestReviewDiffFilesTracksGitlinkModePerSide(t *testing.T) {
	diff := []byte("diff --git a/vendor/tool b/vendor/tool\nold mode 160000\nnew mode 100644\n--- a/vendor/tool\n+++ b/vendor/tool\n@@ -1 +1 @@\n-old\n+new\n")

	files, err := reviewDiffFiles(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].oldGitlink || files[0].newGitlink {
		t.Fatalf("diff files = %#v, want only the old image marked as a gitlink", files)
	}
}

func TestScanScansRegularSideOfGitlinkTransition(t *testing.T) {
	scanner := &captureScannerRunner{}
	commands := &binaryPostimageRunner{scanner: scanner, content: []byte("regular-file")}
	diff := []byte("diff --git a/vendor/tool b/vendor/tool\nold mode 160000\nnew mode 100644\n--- a/vendor/tool\n+++ b/vendor/tool\n@@ -1 +1 @@\n-old\n+new\n")
	reviewTarget := target.Target{Kind: target.KindCommit, RepoDir: t.TempDir(), BaseSHA: "base", SnapshotRef: "head"}

	if err := scanWithTarget(context.Background(), reviewTarget, diff, nil, "fake-trufflehog", commands); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if !containsFileContent(scanner.files, "regular-file") {
		t.Fatalf("scanned files = %#v, want the regular postimage", scanner.files)
	}
}

func TestReviewDiffFilesUsesRenameHeaders(t *testing.T) {
	diff := []byte("diff --git a/old b/name b/new b/name\nsimilarity index 100%\nrename from old b/name\nrename to new b/name\n")

	files, err := reviewDiffFiles(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].oldPath != "old b/name" || files[0].newPath != "new b/name" {
		t.Fatalf("diff files = %#v, want authoritative rename paths", files)
	}
}

func TestVerifiedLocationIsTerminalEscaped(t *testing.T) {
	scanner := &captureScannerRunner{result: runner.Result{
		ExitCode: verifiedExitCode,
		Stdout:   []byte(`{"Verified":true,"DetectorName":"fixture","SourceMetadata":{"Data":{"Filesystem":{"file":"/tmp/roast-secret-scan/bad\n\u001b[31m.txt"}}}}` + "\n"),
	}}
	diff := []byte("diff --git a/config.txt b/config.txt\n--- a/config.txt\n+++ b/config.txt\n@@ -0,0 +1 @@\n+safe\n")

	err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner)
	if err == nil || strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `\n`) {
		t.Fatalf("error = %q, want terminal-safe escaped location", err)
	}
}

func TestScanHandlesCaseOnlyPathsWithoutOverwriting(t *testing.T) {
	scanner := &captureScannerRunner{}
	diff := []byte("diff --git a/config/A.txt b/config/A.txt\n--- /dev/null\n+++ b/config/A.txt\n@@ -0,0 +1 @@\n+upper\n" +
		"diff --git a/config/a.txt b/config/a.txt\n--- /dev/null\n+++ b/config/a.txt\n@@ -0,0 +1 @@\n+lower\n")

	if err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if len(scanner.files) != 2 || !containsFileContent(scanner.files, "upper\n") || !containsFileContent(scanner.files, "lower\n") {
		t.Fatalf("scanned files = %#v, want both case-distinct paths", scanner.files)
	}
}

func TestScanHandlesFileDirectoryTransition(t *testing.T) {
	scanner := &captureScannerRunner{}
	diff := []byte("diff --git a/config b/config\ndeleted file mode 100644\n--- a/config\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n" +
		"diff --git a/config/app.yml b/config/app.yml\nnew file mode 100644\n--- /dev/null\n+++ b/config/app.yml\n@@ -0,0 +1 @@\n+new\n")

	if err := scanWithBinary(context.Background(), t.TempDir(), diff, "fake-trufflehog", scanner); err != nil {
		t.Fatalf("scan error = %v", err)
	}
	if len(scanner.files) != 2 || !containsFileContent(scanner.files, "old\n") || !containsFileContent(scanner.files, "new\n") {
		t.Fatalf("scanned files = %#v, want both transition sides", scanner.files)
	}
}

func TestScanFailsClosedWhenScannerFails(t *testing.T) {
	scanner := &captureScannerRunner{result: runner.Result{ExitCode: 2, Stderr: []byte("scanner unavailable")}}
	err := scanWithBinary(context.Background(), t.TempDir(), nil, "fake-trufflehog", scanner)
	if err == nil || !strings.Contains(err.Error(), "ROAST-SECRET-SCAN") || !strings.Contains(err.Error(), "no review engine was called") {
		t.Fatalf("error = %v, want scan failure guidance", err)
	}
}

func TestFindBinaryReportsInstallAction(t *testing.T) {
	t.Setenv("ROAST_TRUFFLEHOG", "")
	t.Setenv("TRUFFLEHOG", "")
	t.Setenv("PATH", t.TempDir())
	_, err := FindBinary()
	if err == nil || !strings.Contains(err.Error(), "ROAST-SECRET-BINARY") || !strings.Contains(err.Error(), "brew install trufflehog") || !strings.Contains(err.Error(), "ROAST_TRUFFLEHOG=") {
		t.Fatalf("error = %v, want install guidance", err)
	}
}

type captureScannerRunner struct {
	program string
	args    []string
	files   map[string][]byte
	result  runner.Result
	err     error
}

func containsFileContent(files map[string][]byte, wanted string) bool {
	for _, content := range files {
		if string(content) == wanted {
			return true
		}
	}
	return false
}

func (r *captureScannerRunner) Run(_ context.Context, _ string, program string, args ...string) (runner.Result, error) {
	r.program = program
	r.args = append([]string(nil), args...)
	r.files = make(map[string][]byte)
	if len(args) > 0 {
		directory := args[len(args)-1]
		_ = filepath.Walk(directory, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info == nil || info.IsDir() {
				return nil
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			relative, relErr := filepath.Rel(directory, path)
			if relErr == nil {
				r.files[filepath.ToSlash(relative)] = content
			}
			return nil
		})
	}
	return r.result, r.err
}

func (r *captureScannerRunner) RunWithInputStream(_ context.Context, _ string, _ []byte, _ io.Writer, _ string, _ ...string) (runner.Result, error) {
	return r.result, r.err
}

type binaryPostimageRunner struct {
	scanner *captureScannerRunner
	content []byte
	args    []string
}

func (r *binaryPostimageRunner) Run(ctx context.Context, dir, program string, args ...string) (runner.Result, error) {
	if program == "git" {
		r.args = append([]string(nil), args...)
		return runner.Result{Stdout: r.content}, nil
	}
	return r.scanner.Run(ctx, dir, program, args...)
}

func (r *binaryPostimageRunner) RunWithInputStream(ctx context.Context, dir string, input []byte, stdout io.Writer, program string, args ...string) (runner.Result, error) {
	return r.Run(ctx, dir, program, args...)
}
