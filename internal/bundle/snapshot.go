package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/secrets"
	"github.com/janiorvalle/roast/internal/target"
)

type snapshotFile struct {
	Name    string
	Mode    int64
	Content []byte
}

type worktreePath struct {
	Name              string
	Sparse            bool
	IndexMode         int64
	IndexObject       string
	UseFilesystemMode bool
}

func BuildSnapshot(ctx context.Context, reviewTarget target.Target, commands runner.Runner) ([]byte, error) {
	return buildSnapshot(ctx, reviewTarget, commands, false)
}

// BuildSecretScanSnapshot captures the reviewed current tree without the
// sensitive-path filtering used by the review-engine snapshot. It is kept in
// memory only so local secret scanning sees the same bytes the bundle builder
// captured without exposing those paths to the engine.
func BuildSecretScanSnapshot(ctx context.Context, reviewTarget target.Target, commands runner.Runner) ([]byte, error) {
	return buildSnapshot(ctx, reviewTarget, commands, true)
}

func filterSensitiveSnapshot(data []byte) ([]byte, error) {
	var filtered bytes.Buffer
	reader := tar.NewReader(bytes.NewReader(data))
	archive := tar.NewWriter(&filtered)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			if err := archive.Close(); err != nil {
				return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] finalize filtered snapshot: %w", err)
			}
			return filtered.Bytes(), nil
		}
		if err != nil {
			_ = archive.Close()
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] read captured snapshot: %w", err)
		}
		if secrets.IsSensitivePath(header.Name) {
			continue
		}
		headerCopy := *header
		if err := archive.WriteHeader(&headerCopy); err != nil {
			_ = archive.Close()
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] write captured file %q: %w", header.Name, err)
		}
		if _, err := io.Copy(archive, reader); err != nil {
			_ = archive.Close()
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] copy captured file %q: %w", header.Name, err)
		}
	}
}

func buildSnapshot(ctx context.Context, reviewTarget target.Target, commands runner.Runner, includeSensitive bool) ([]byte, error) {
	if reviewTarget.RepoDir == "" {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] target has no repository directory; resolve a target before building a snapshot")
	}
	if commands == nil {
		commands = runner.ExecRunner{}
	}
	if reviewTarget.Kind == target.KindDirty {
		return buildWorktreeSnapshot(ctx, reviewTarget, commands, includeSensitive)
	}
	if reviewTarget.SnapshotRef == "" {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] target has no snapshot ref; resolve a commit-backed target before building a snapshot")
	}
	return buildRefSnapshot(ctx, reviewTarget, commands, includeSensitive)
}

func buildRefSnapshot(ctx context.Context, reviewTarget target.Target, commands runner.Runner, includeSensitive bool) ([]byte, error) {
	// Git archive attributes are intentionally bypassed: export-ignore can
	// hide tracked context and export-subst can rewrite reviewed bytes.
	listArgs := []string{"ls-tree", "-r", "-z", "--full-tree", reviewTarget.SnapshotRef}
	listResult, err := commands.Run(ctx, reviewTarget.RepoDir, "git", listArgs...)
	if err != nil {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] cannot list tracked files at %s: %w", reviewTarget.SnapshotRef, err)
	}
	if listResult.ExitCode != 0 {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] cannot list tracked files at %s: %s; fetch the reviewed commit and retry", reviewTarget.SnapshotRef, runner.Failure("git", listArgs, listResult))
	}

	entries := make([]treeEntry, 0)
	for _, rawEntry := range bytes.Split(listResult.Stdout, []byte{0}) {
		if len(rawEntry) == 0 {
			continue
		}
		entry, err := parseTreeEntry(rawEntry)
		if err != nil {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid Git tree entry: %w", err)
		}
		if entry.Type != "blob" {
			continue
		}
		if !includeSensitive && secrets.IsSensitivePath(entry.Name) {
			continue
		}
		entries = append(entries, entry)
	}
	var snapshot bytes.Buffer
	archive := tar.NewWriter(&snapshot)
	if err := writeGitBlobs(ctx, reviewTarget.RepoDir, entries, archive, commands); err != nil {
		_ = archive.Close()
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] cannot read tracked files at %s: %w", reviewTarget.SnapshotRef, err)
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] finalize tracked snapshot: %w", err)
	}
	return snapshot.Bytes(), nil
}

func writeGitBlobs(ctx context.Context, repoDir string, entries []treeEntry, archive *tar.Writer, commands runner.Runner) error {
	if len(entries) == 0 {
		return nil
	}
	var input bytes.Buffer
	for _, entry := range entries {
		input.WriteString(entry.Object)
		input.WriteByte('\n')
	}
	args := []string{"cat-file", "--batch"}
	stream := newBatchArchiveWriter(archive, entries)
	result, err := commands.RunWithInputStream(ctx, repoDir, input.Bytes(), stream, "git", args...)
	if err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] cannot run git cat-file --batch: %w; retry the snapshot", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] git cat-file --batch failed: %s; fetch the reviewed commit and retry", runner.Failure("git", args, result))
	}
	return stream.finish()
}

const maxGitBatchHeaderBytes = 256

type batchArchiveWriter struct {
	archive            *tar.Writer
	entries            []treeEntry
	index              int
	header             []byte
	remaining          int64
	expectingSeparator bool
}

func newBatchArchiveWriter(archive *tar.Writer, entries []treeEntry) *batchArchiveWriter {
	return &batchArchiveWriter{archive: archive, entries: entries, remaining: -1}
}

func (writer *batchArchiveWriter) Write(data []byte) (int, error) {
	consumed := 0
	for len(data) > 0 {
		if writer.index >= len(writer.entries) {
			return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] response has trailing bytes after the last tracked file; retry the snapshot")
		}
		if writer.expectingSeparator {
			if data[0] != '\n' {
				return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] missing record separator after %q; retry the snapshot", writer.entries[writer.index].Name)
			}
			data = data[1:]
			consumed++
			writer.index++
			writer.header = writer.header[:0]
			writer.remaining = -1
			writer.expectingSeparator = false
			continue
		}
		if writer.remaining >= 0 {
			take := int64(len(data))
			if take > writer.remaining {
				take = writer.remaining
			}
			if take > 0 {
				written, err := writer.archive.Write(data[:int(take)])
				if err != nil {
					return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] cannot write tracked file %q: %w", writer.entries[writer.index].Name, err)
				}
				if written != int(take) {
					return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] short write for tracked file %q; retry the snapshot", writer.entries[writer.index].Name)
				}
				data = data[int(take):]
				consumed += int(take)
				writer.remaining -= take
			}
			if writer.remaining == 0 {
				writer.expectingSeparator = true
			}
			continue
		}

		lineEnd := bytes.IndexByte(data, '\n')
		if lineEnd < 0 {
			if len(writer.header)+len(data) > maxGitBatchHeaderBytes {
				return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] response header is too long; retry the snapshot")
			}
			writer.header = append(writer.header, data...)
			consumed += len(data)
			return consumed, nil
		}
		if len(writer.header)+lineEnd > maxGitBatchHeaderBytes {
			return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] response header is too long; retry the snapshot")
		}
		writer.header = append(writer.header, data[:lineEnd]...)
		data = data[lineEnd+1:]
		consumed += lineEnd + 1
		size, err := parseGitBatchHeader(writer.header, writer.entries[writer.index])
		if err != nil {
			return consumed, err
		}
		entry := writer.entries[writer.index]
		if err := writeSnapshotHeader(writer.archive, entry.Name, entry.Mode, size); err != nil {
			return consumed, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] cannot start tracked file %q: %w", writer.entries[writer.index].Name, err)
		}
		writer.remaining = size
		if writer.remaining == 0 {
			writer.expectingSeparator = true
		}
	}
	return consumed, nil
}

func (writer *batchArchiveWriter) finish() error {
	if writer.index != len(writer.entries) || writer.remaining >= 0 || writer.expectingSeparator || len(writer.header) != 0 {
		return fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] response ended before all tracked files were read; retry the snapshot")
	}
	return nil
}

func parseGitBatchHeader(header []byte, entry treeEntry) (int64, error) {
	fields := bytes.Split(header, []byte{' '})
	if len(fields) == 2 && string(fields[1]) == "missing" {
		return 0, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] object %q for tracked file %q is missing; fetch the reviewed commit and retry", entry.Object, entry.Name)
	}
	if len(fields) != 3 {
		return 0, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] malformed response header for %q; retry the snapshot", entry.Name)
	}
	if string(fields[0]) != entry.Object {
		return 0, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] response object %q did not match requested object %q for %q; retry the snapshot", fields[0], entry.Object, entry.Name)
	}
	if string(fields[1]) != "blob" {
		return 0, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] object %q for %q has type %q, not blob; retry the snapshot", entry.Object, entry.Name, fields[1])
	}
	size, err := strconv.ParseInt(string(fields[2]), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT-BATCH] invalid content size %q for %q; retry the snapshot", fields[2], entry.Name)
	}
	return size, nil
}

func buildWorktreeSnapshot(ctx context.Context, reviewTarget target.Target, commands runner.Runner, includeSensitive bool) ([]byte, error) {
	paths, err := listWorktreePaths(ctx, reviewTarget.RepoDir, commands, includeSensitive)
	if err != nil {
		return nil, err
	}

	var snapshot bytes.Buffer
	archive := tar.NewWriter(&snapshot)
	for _, worktreePath := range paths {
		if !includeSensitive && secrets.IsSensitivePath(worktreePath.Name) {
			continue
		}
		entry, include, err := readWorktreeFile(ctx, reviewTarget, worktreePath, commands)
		if err != nil {
			_ = archive.Close()
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] cannot read worktree file %q: %w", worktreePath.Name, err)
		}
		if !include {
			continue
		}
		if err := writeSnapshotFile(archive, entry); err != nil {
			_ = archive.Close()
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] write worktree file %q: %w", worktreePath.Name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] finalize worktree snapshot: %w", err)
	}
	return snapshot.Bytes(), nil
}

func listWorktreePaths(ctx context.Context, repoDir string, commands runner.Runner, includeSensitive bool) ([]worktreePath, error) {
	useFilesystemMode := gitObservesFileMode(ctx, repoDir, commands)
	tracked, err := runTrackedPathList(ctx, repoDir, commands)
	if err != nil {
		return nil, err
	}
	untracked, err := runPathList(ctx, repoDir, commands, []string{"ls-files", "--others", "--exclude-standard", "-z"}, "nonignored untracked worktree files")
	if err != nil {
		return nil, err
	}

	paths := make([]worktreePath, 0, len(tracked)+len(untracked))
	seen := make(map[string]struct{}, len(tracked)+len(untracked))
	for _, group := range [][]worktreePath{tracked, untracked} {
		for _, item := range group {
			if !includeSensitive && secrets.IsSensitivePath(item.Name) {
				continue
			}
			if _, exists := seen[item.Name]; exists {
				continue
			}
			seen[item.Name] = struct{}{}
			paths = append(paths, item)
		}
	}
	for i := range paths {
		paths[i].UseFilesystemMode = useFilesystemMode
	}
	return paths, nil
}

func runTrackedPathList(ctx context.Context, repoDir string, commands runner.Runner) ([]worktreePath, error) {
	args := []string{"ls-files", "-t", "-z"}
	rawPaths, err := runGitPathList(ctx, repoDir, commands, args, "tracked worktree files")
	if err != nil {
		return nil, err
	}

	paths := make([]worktreePath, 0)
	for _, rawPath := range bytes.Split(rawPaths, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		if len(rawPath) < 3 || rawPath[1] != ' ' {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid tracked worktree path record %q", rawPath)
		}
		name := string(rawPath[2:])
		if err := validateSnapshotPath(name); err != nil {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid worktree path: %w", err)
		}
		// Git's S tag marks a skip-worktree entry in the index.
		paths = append(paths, worktreePath{Name: name, Sparse: rawPath[0] == 'S'})
	}
	indexArgs := []string{"ls-files", "--stage", "-z"}
	rawIndex, err := runGitPathList(ctx, repoDir, commands, indexArgs, "tracked index entries")
	if err != nil {
		return nil, err
	}
	indexEntries, err := parseIndexPathList(rawIndex)
	if err != nil {
		return nil, err
	}
	for i := range paths {
		if entry, ok := indexEntries[paths[i].Name]; ok {
			paths[i].IndexMode = entry.Mode
			paths[i].IndexObject = entry.Object
		}
	}
	return paths, nil
}

func gitObservesFileMode(ctx context.Context, repoDir string, commands runner.Runner) bool {
	result, err := commands.Run(ctx, repoDir, "git", "config", "--bool", "core.filemode")
	if err == nil && result.ExitCode == 0 {
		return strings.TrimSpace(string(result.Stdout)) == "true"
	}
	return runtime.GOOS != "windows"
}

func runPathList(ctx context.Context, repoDir string, commands runner.Runner, args []string, description string) ([]worktreePath, error) {
	rawPaths, err := runGitPathList(ctx, repoDir, commands, args, description)
	if err != nil {
		return nil, err
	}

	paths := make([]worktreePath, 0)
	for _, rawPath := range bytes.Split(rawPaths, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		name := string(rawPath)
		if strings.HasSuffix(name, "/") {
			continue
		}
		if err := validateSnapshotPath(name); err != nil {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid worktree path: %w", err)
		}
		paths = append(paths, worktreePath{Name: name})
	}
	return paths, nil
}

func runGitPathList(ctx context.Context, repoDir string, commands runner.Runner, args []string, description string) ([]byte, error) {
	result, err := commands.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] cannot list %s: %w", description, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] cannot list %s: %s; retry from a writable Git worktree", description, runner.Failure("git", args, result))
	}
	return result.Stdout, nil
}

func readWorktreeFile(ctx context.Context, reviewTarget target.Target, worktreePath worktreePath, commands runner.Runner) (snapshotFile, bool, error) {
	if err := validateWorktreeParents(reviewTarget.RepoDir, worktreePath.Name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return readSparseSnapshotFile(ctx, reviewTarget, worktreePath, commands)
		}
		if errors.Is(err, errWorktreeParentNotDirectory) || errors.Is(err, errWorktreeParentSymlink) {
			return snapshotFile{}, false, nil
		}
		return snapshotFile{}, false, err
	}

	filePath := filepath.Join(reviewTarget.RepoDir, filepath.FromSlash(worktreePath.Name))
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return readSparseSnapshotFile(ctx, reviewTarget, worktreePath, commands)
	}
	if err != nil {
		return snapshotFile{}, false, err
	}
	if info.IsDir() {
		return snapshotFile{}, false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err := os.Readlink(filePath)
		if err != nil {
			return snapshotFile{}, false, err
		}
		return snapshotFile{Name: worktreePath.Name, Mode: 0o644, Content: []byte(linkTarget)}, true, nil
	}
	if !info.Mode().IsRegular() {
		return snapshotFile{}, false, fmt.Errorf("unsupported file type %s; replace it with a regular file or symlink", info.Mode().String())
	}
	// Decision 19 treats reviewed worktrees as trusted first-party code; parent
	// symlink entries were neutralized above, so keep the file read plain.
	file, err := os.Open(filePath)
	if err != nil {
		return snapshotFile{}, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return snapshotFile{}, false, err
	}
	mode := int64(info.Mode().Perm())
	if worktreePath.IndexMode != 0 && !worktreePath.UseFilesystemMode {
		mode = worktreePath.IndexMode & 0o777
	}
	return snapshotFile{Name: worktreePath.Name, Mode: mode, Content: content}, true, nil
}

type indexEntry struct {
	Mode   int64
	Object string
}

func parseIndexPathList(raw []byte) (map[string]indexEntry, error) {
	entries := make(map[string]indexEntry)
	for _, rawEntry := range bytes.Split(raw, []byte{0}) {
		if len(rawEntry) == 0 {
			continue
		}
		separator := bytes.IndexByte(rawEntry, '\t')
		if separator <= 0 || separator == len(rawEntry)-1 {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid Git index entry %q", rawEntry)
		}
		fields := strings.Fields(string(rawEntry[:separator]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid Git index entry metadata %q", rawEntry[:separator])
		}
		mode, err := parseTreeMode(fields[0])
		if err != nil {
			return nil, err
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 0 || stage > 3 {
			return nil, fmt.Errorf("[ROAST-BUNDLE-SNAPSHOT] invalid Git index stage %q", fields[2])
		}
		name := string(rawEntry[separator+1:])
		if err := validateSnapshotPath(name); err != nil {
			return nil, err
		}
		if current, ok := entries[name]; ok && current.Object != "" {
			if stage != 0 {
				continue
			}
		}
		entries[name] = indexEntry{Mode: mode, Object: fields[1]}
	}
	return entries, nil
}

func readSparseSnapshotFile(ctx context.Context, reviewTarget target.Target, worktreePath worktreePath, commands runner.Runner) (snapshotFile, bool, error) {
	if !worktreePath.Sparse {
		return snapshotFile{}, false, nil
	}
	if worktreePath.IndexMode == 0o160000 {
		return snapshotFile{}, false, nil
	}
	if worktreePath.IndexObject == "" {
		return snapshotFile{}, false, fmt.Errorf("skip-worktree path has no index blob; refresh the Git index and retry")
	}
	content, err := readGitBlob(ctx, reviewTarget.RepoDir, worktreePath.IndexObject, worktreePath.Name, commands)
	if err != nil {
		return snapshotFile{}, false, err
	}
	mode := worktreePath.IndexMode & 0o777
	if worktreePath.IndexMode == 0o120000 {
		mode = 0o644
	}
	return snapshotFile{Name: worktreePath.Name, Mode: mode, Content: content}, true, nil
}

func readGitBlob(ctx context.Context, repoDir, object, name string, commands runner.Runner) ([]byte, error) {
	contentArgs := []string{"cat-file", "blob", object}
	contentResult, err := commands.Run(ctx, repoDir, "git", contentArgs...)
	if err != nil {
		return nil, err
	}
	if contentResult.ExitCode != 0 {
		return nil, fmt.Errorf("cannot read tracked file %q: %s; fetch the reviewed commit and retry", name, runner.Failure("git", contentArgs, contentResult))
	}
	return contentResult.Stdout, nil
}

var errWorktreeParentNotDirectory = errors.New("worktree parent is not a directory")
var errWorktreeParentSymlink = errors.New("worktree parent is a symlink")

func validateWorktreeParents(repoDir, name string) error {
	parts := strings.Split(name, "/")
	current := repoDir
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: parent directory %q is a symlink", errWorktreeParentSymlink, part)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: parent path %q is not a directory", errWorktreeParentNotDirectory, part)
		}
	}
	return nil
}

func writeSnapshotFile(archive *tar.Writer, entry snapshotFile) error {
	if err := writeSnapshotHeader(archive, entry.Name, entry.Mode, int64(len(entry.Content))); err != nil {
		return err
	}
	_, err := archive.Write(entry.Content)
	return err
}

func writeSnapshotHeader(archive *tar.Writer, name string, sourceMode, size int64) error {
	mode := sourceMode & 0o777
	if sourceMode == 0o120000 || mode == 0 {
		mode = 0o644
	}
	header := tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     size,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
	}
	return archive.WriteHeader(&header)
}

type treeEntry struct {
	Mode   int64
	Type   string
	Object string
	Name   string
}

func parseTreeEntry(raw []byte) (treeEntry, error) {
	separator := bytes.IndexByte(raw, '\t')
	if separator <= 0 || separator == len(raw)-1 {
		return treeEntry{}, fmt.Errorf("missing mode, object, or path")
	}
	fields := strings.Fields(string(raw[:separator]))
	if len(fields) != 3 {
		return treeEntry{}, fmt.Errorf("expected three tree fields")
	}
	mode, err := parseTreeMode(fields[0])
	if err != nil {
		return treeEntry{}, err
	}
	name := string(raw[separator+1:])
	if err := validateSnapshotPath(name); err != nil {
		return treeEntry{}, err
	}
	return treeEntry{Mode: mode, Type: fields[1], Object: fields[2], Name: name}, nil
}

func validateSnapshotPath(name string) error {
	cleanName := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if cleanName != name || cleanName == "." || strings.HasPrefix(cleanName, "/") || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
		return fmt.Errorf("unsafe tracked path %q", name)
	}
	return nil
}

func parseTreeMode(raw string) (int64, error) {
	var mode int64
	if _, err := fmt.Sscanf(raw, "%o", &mode); err != nil {
		return 0, fmt.Errorf("invalid file mode %q", raw)
	}
	return mode, nil
}
