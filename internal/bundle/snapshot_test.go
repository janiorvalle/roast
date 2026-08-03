package bundle

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
)

func TestBuildSnapshotRejectsMissingSnapshotRef(t *testing.T) {
	_, err := BuildSnapshot(context.Background(), target.Target{RepoDir: t.TempDir()}, runner.ExecRunner{})
	if err == nil || !strings.Contains(err.Error(), "ROAST-BUNDLE-SNAPSHOT") {
		t.Fatalf("error = %v, want missing snapshot ref", err)
	}
}

func TestBuildSnapshotIsAReadableTarArchive(t *testing.T) {
	repo := initBundleRepository(t)
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Base: "HEAD", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := BuildSnapshot(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(strings.NewReader(string(snapshot)))
	foundBase := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "base.txt" {
			foundBase = true
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatal(err)
		}
	}
	if !foundBase {
		t.Fatal("snapshot archive does not contain base.txt")
	}
}

func TestBuildSnapshotUsesOneBatchBlobRead(t *testing.T) {
	firstObject := strings.Repeat("a", 40)
	secondObject := strings.Repeat("b", 40)
	tree := []byte("100644 blob " + firstObject + "\ta.txt\x00" + "100644 blob " + secondObject + "\tb.txt\x00")
	batchOutput := appendBatchRecord(nil, firstObject, "first\n")
	batchOutput = appendBatchRecord(batchOutput, secondObject, "second\n")
	commands := &fakeBundleRunner{responses: map[string]runner.Result{
		"git ls-tree -r -z --full-tree HEAD": {Stdout: tree},
		"git cat-file --batch":               {Stdout: batchOutput},
	}}

	snapshot, err := BuildSnapshot(context.Background(), target.Target{RepoDir: t.TempDir(), SnapshotRef: "HEAD"}, commands)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands.inputs) != 1 {
		t.Fatalf("batch input calls = %d, want one", len(commands.inputs))
	}
	if got, want := string(commands.inputs[0]), firstObject+"\n"+secondObject+"\n"; got != want {
		t.Fatalf("batch input = %q, want %q", got, want)
	}
	files := snapshotFiles(t, snapshot)
	if got := string(files["a.txt"]); got != "first\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := string(files["b.txt"]); got != "second\n" {
		t.Fatalf("b.txt = %q", got)
	}
}

func TestWriteGitBlobsPreservesBatchParserErrorWhenGitExitsAfterBrokenPipe(t *testing.T) {
	firstObject := strings.Repeat("a", 40)
	missingObject := strings.Repeat("b", 40)
	entries := []treeEntry{
		{Mode: 0o100644, Type: "blob", Object: firstObject, Name: "first.txt"},
		{Mode: 0o100644, Type: "blob", Object: missingObject, Name: "missing.txt"},
	}
	batchOutput := appendBatchRecord(nil, firstObject, "first\n")
	batchOutput = append(batchOutput, missingObject+" missing\n"...)
	commands := batchOutputRunner{output: batchOutput}
	archive := tar.NewWriter(io.Discard)

	err := writeGitBlobs(context.Background(), t.TempDir(), entries, archive, commands)
	if err == nil || !strings.Contains(err.Error(), `object "`+missingObject+`" for tracked file "missing.txt" is missing`) {
		t.Fatalf("error = %v, want the specific missing-object diagnostic", err)
	}
}

func TestBuildSnapshotPreservesArchiveAttributeFilesAndBytes(t *testing.T) {
	repo := initBundleRepository(t)
	writeBundleFile(t, repo, ".gitattributes", "hidden.txt export-ignore\nsource.txt export-subst\n")
	writeBundleFile(t, repo, "hidden.txt", "tracked context\n")
	writeBundleFile(t, repo, "source.txt", "$Format:%H$\n")
	runBundleGit(t, repo, "add", ".gitattributes", "hidden.txt", "source.txt")
	runBundleGit(t, repo, "commit", "-m", "archive attributes")
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Base: "HEAD", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := BuildSnapshot(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	files := snapshotFiles(t, snapshot)
	if got, ok := files["hidden.txt"]; !ok || string(got) != "tracked context\n" {
		t.Fatalf("hidden.txt = %q, present=%v", got, ok)
	}
	if got := string(files["source.txt"]); got != "$Format:%H$\n" {
		t.Fatalf("source.txt = %q, want unexpanded bytes", got)
	}
}

func TestBuildSnapshotThousandsOfFiles(t *testing.T) {
	const fileCount = 2048

	repo := initBundleRepository(t)
	for index := 0; index < fileCount; index++ {
		writeBundleFile(t, repo, fmt.Sprintf("files/%04d.txt", index), "content\n")
	}
	runBundleGit(t, repo, "add", "files")
	runBundleGit(t, repo, "commit", "-m", "thousands of files")
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Base: "HEAD", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	snapshot, err := BuildSnapshot(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot timing: files=%d duration=%s bytes=%d", fileCount, time.Since(started).Round(time.Millisecond), len(snapshot))
	if len(snapshot) == 0 {
		t.Fatal("snapshot is empty")
	}
}

func TestBuildSnapshotStoresSymlinkAsRegularFile(t *testing.T) {
	repo := initBundleRepository(t)
	linkPath := filepath.Join(repo, "link.txt")
	if err := os.Symlink("../outside", linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runBundleGit(t, repo, "add", "link.txt")
	runBundleGit(t, repo, "commit", "-m", "symlink")
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Base: "HEAD", RepoDir: repo})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := BuildSnapshot(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}

	reader := tar.NewReader(strings.NewReader(string(snapshot)))
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "link.txt" {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatal(err)
			}
			continue
		}
		found = true
		if header.Typeflag != tar.TypeReg {
			t.Fatalf("link.txt type = %d, want regular file", header.Typeflag)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "../outside" {
			t.Fatalf("link.txt content = %q, want link target", content)
		}
	}
	if !found {
		t.Fatal("snapshot archive does not contain link.txt")
	}
}

func TestRunTrackedPathListKeepsLiteralPathspecNames(t *testing.T) {
	object := strings.Repeat("a", 40)
	commands := &fakeBundleRunner{responses: map[string]runner.Result{
		"git ls-files -t -z":      {Stdout: []byte("S :(top)foo\x00")},
		"git ls-files --stage -z": {Stdout: []byte("100644 " + object + " 0\t:(top)foo\x00")},
	}}
	paths, err := runTrackedPathList(context.Background(), t.TempDir(), commands)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0].Name != ":(top)foo" || !paths[0].Sparse || paths[0].IndexObject != object {
		t.Fatalf("tracked paths = %#v, want literal sparse index entry", paths)
	}
}

func snapshotFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	reader := tar.NewReader(strings.NewReader(string(data)))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = content
	}
}

func appendBatchRecord(output []byte, object, content string) []byte {
	output = append(output, fmt.Sprintf("%s blob %d\n", object, len(content))...)
	output = append(output, content...)
	return append(output, '\n')
}

type batchOutputRunner struct {
	output []byte
}

func (batchOutputRunner) Run(context.Context, string, string, ...string) (runner.Result, error) {
	return runner.Result{}, nil
}

func (r batchOutputRunner) RunWithInputStream(_ context.Context, _ string, _ []byte, stdout io.Writer, _ string, _ ...string) (runner.Result, error) {
	_, _ = stdout.Write(r.output)
	return runner.Result{ExitCode: -1, Stderr: []byte("broken pipe")}, nil
}
