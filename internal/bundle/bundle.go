package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/secrets"
	"github.com/janiorvalle/roast/internal/target"
)

type Bundle struct {
	Target                 target.Target
	Diff                   []byte
	SecretScanDiff         []byte
	SecretScanSnapshot     []byte
	ExcludedSensitivePaths []string
	Snapshot               []byte
}

type Manifest struct {
	Target        target.Target `json:"target"`
	DiffBytes     int           `json:"diff_bytes"`
	SnapshotBytes int           `json:"snapshot_bytes"`
}

func Build(ctx context.Context, reviewTarget target.Target, commands runner.Runner) (Bundle, error) {
	if reviewTarget.RepoDir == "" {
		return Bundle{}, fmt.Errorf("[ROAST-BUNDLE] target has no repository directory; resolve a target before building a bundle")
	}
	if commands == nil {
		commands = runner.ExecRunner{}
	}
	if err := ensureReviewObjects(ctx, reviewTarget, commands); err != nil {
		return Bundle{}, err
	}
	if reviewTarget.ThreeDotDiff {
		if err := ensureMergeBase(ctx, reviewTarget, commands); err != nil {
			return Bundle{}, err
		}
	}
	diff, err := buildDiff(ctx, reviewTarget, commands)
	if err != nil {
		return Bundle{}, err
	}
	secretScanDiff := append([]byte(nil), diff...)
	var excludedSensitivePaths []string
	diff, excludedSensitivePaths, err = filterSensitiveDiff(diff)
	if err != nil {
		return Bundle{}, err
	}
	secretScanSnapshot, err := BuildSecretScanSnapshot(ctx, reviewTarget, commands)
	if err != nil {
		return Bundle{}, err
	}
	snapshot, err := filterSensitiveSnapshot(secretScanSnapshot)
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Target: reviewTarget, Diff: diff, SecretScanDiff: secretScanDiff, SecretScanSnapshot: secretScanSnapshot, ExcludedSensitivePaths: excludedSensitivePaths, Snapshot: snapshot}, nil
}

// DiffContribution describes one file section's contribution to the prompt
// copy of a diff.
type DiffContribution struct {
	Path  string
	Bytes int
}

// SanitizeDiffForPrompt removes binary patch payloads and replaces invalid
// UTF-8 sequences in review diff text with U+FFFD. The raw diff remains in
// Bundle.Diff for bundle fingerprints and local secret scanning; this function
// only prepares the prompt copy.
func SanitizeDiffForPrompt(diff, snapshot []byte) (string, []string) {
	diff = stubBinaryPatches(diff, snapshotFileSizes(snapshot))
	if utf8.Valid(diff) {
		return string(diff), nil
	}

	lines := bytes.SplitAfter(diff, []byte("\n"))
	var sanitized bytes.Buffer
	invalidPaths := make(map[string]struct{})
	currentPaths := []string(nil)
	inFileHeader := false
	for _, line := range lines {
		lineText := trimDiffPromptLine(line)
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			currentPaths, _ = bundleDiffHeaderPaths(strings.TrimPrefix(lineText, "diff --git "))
			inFileHeader = true
		} else if inFileHeader {
			switch {
			case strings.HasPrefix(lineText, "@@ "), strings.HasPrefix(lineText, "GIT binary patch"):
				inFileHeader = false
			default:
				currentPaths = appendDiffPromptPath(currentPaths, lineText)
			}
		}

		if !utf8.Valid(line) {
			for _, path := range currentPaths {
				invalidPaths[strings.ToValidUTF8(path, "\uFFFD")] = struct{}{}
			}
			if len(currentPaths) == 0 {
				invalidPaths["diff metadata"] = struct{}{}
			}
			_, _ = sanitized.Write(bytes.ToValidUTF8(line, []byte("\uFFFD")))
			continue
		}
		_, _ = sanitized.Write(line)
	}

	paths := make([]string, 0, len(invalidPaths))
	for path := range invalidPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return sanitized.String(), paths
}

// PromptDiffContributions returns file sections ordered by their appearance in
// the prompt diff. Callers may sort the result when presenting size details.
func PromptDiffContributions(diff string) []DiffContribution {
	sections := splitDiffSections([]byte(diff))
	contributions := make([]DiffContribution, 0, len(sections))
	for _, section := range sections {
		if !bytes.HasPrefix(section, []byte("diff --git ")) {
			continue
		}
		lineEnd := bytes.IndexByte(section, '\n')
		if lineEnd < 0 {
			lineEnd = len(section)
		}
		paths, ok := bundleDiffHeaderPaths(strings.TrimPrefix(string(section[:lineEnd]), "diff --git "))
		path := "unidentified diff section"
		if ok && len(paths) > 0 {
			path = paths[len(paths)-1]
		}
		contributions = append(contributions, DiffContribution{Path: path, Bytes: len(section)})
	}
	return contributions
}

func stubBinaryPatches(diff []byte, snapshotSizes map[string]int) []byte {
	sections := splitDiffSections(diff)
	if len(sections) == 0 {
		return diff
	}
	var sanitized bytes.Buffer
	for _, section := range sections {
		marker := []byte("GIT binary patch\n")
		markerStart := binaryPatchMarkerStart(section)
		if markerStart < 0 {
			_, _ = sanitized.Write(section)
			continue
		}
		operation := binaryPatchOperation(section[:markerStart])
		path := binaryPatchPath(section[:markerStart], operation)
		_, _ = sanitized.Write(section[:markerStart])
		if size, ok := binaryFileSize(section[markerStart+len(marker):], operation, path, snapshotSizes); ok {
			fmt.Fprintf(&sanitized, "Binary patch omitted: %q, %s binary, %d bytes.\n", path, operation, size)
		} else {
			fmt.Fprintf(&sanitized, "Binary patch omitted: %q, %s binary, file size unavailable.\n", path, operation)
		}
	}
	return sanitized.Bytes()
}

func binaryPatchMarkerStart(section []byte) int {
	offset := 0
	for _, line := range bytes.SplitAfter(section, []byte("\n")) {
		lineText := trimDiffPromptLine(line)
		if strings.HasPrefix(lineText, "@@ ") {
			return -1
		}
		if lineText == "GIT binary patch" {
			return offset
		}
		offset += len(line)
	}
	return -1
}

func splitDiffSections(diff []byte) [][]byte {
	marker := []byte("diff --git ")
	first := nextDiffHeader(diff, 0)
	if first < 0 {
		return [][]byte{diff}
	}
	sections := make([][]byte, 0, 4)
	if first > 0 {
		sections = append(sections, diff[:first])
	}
	for start := first; start < len(diff); {
		next := nextDiffHeader(diff, start+len(marker))
		if next < 0 {
			sections = append(sections, diff[start:])
			break
		}
		sections = append(sections, diff[start:next])
		start = next
	}
	return sections
}

func nextDiffHeader(diff []byte, searchFrom int) int {
	if searchFrom == 0 && bytes.HasPrefix(diff, []byte("diff --git ")) {
		return 0
	}
	if searchFrom >= len(diff) {
		return -1
	}
	offset := bytes.Index(diff[searchFrom:], []byte("\ndiff --git "))
	if offset < 0 {
		return -1
	}
	return searchFrom + offset + 1
}

func binaryPatchOperation(header []byte) string {
	switch {
	case bytes.Contains(header, []byte("\nnew file mode ")):
		return "added"
	case bytes.Contains(header, []byte("\ndeleted file mode ")):
		return "deleted"
	default:
		return "modified"
	}
}

func binaryPatchPath(header []byte, operation string) string {
	lineEnd := bytes.IndexByte(header, '\n')
	if lineEnd < 0 {
		lineEnd = len(header)
	}
	paths, ok := bundleDiffHeaderPaths(strings.TrimPrefix(string(header[:lineEnd]), "diff --git "))
	if !ok || len(paths) == 0 {
		return "unidentified binary file"
	}
	if operation == "deleted" {
		return paths[0]
	}
	return paths[len(paths)-1]
}

type binaryPatchHeader struct {
	kind string
	size int
}

func binaryFileSize(payload []byte, operation, path string, snapshotSizes map[string]int) (int, bool) {
	headers := make([]binaryPatchHeader, 0, 2)
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "literal" && fields[0] != "delta") {
			continue
		}
		size, err := strconv.Atoi(fields[1])
		if err == nil {
			headers = append(headers, binaryPatchHeader{kind: fields[0], size: size})
		}
	}
	if operation != "deleted" {
		if size, ok := snapshotSizes[path]; ok {
			return size, true
		}
	}
	if len(headers) == 0 {
		return 0, false
	}
	selected := headers[0]
	if operation == "deleted" {
		selected = headers[len(headers)-1]
	}
	if selected.kind != "literal" {
		return 0, false
	}
	return selected.size, true
}

func trimDiffPromptLine(line []byte) string {
	return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
}

func appendDiffPromptPath(paths []string, line string) []string {
	var candidates []string
	for _, header := range []struct {
		marker string
		prefix string
	}{
		{marker: "--- ", prefix: "a/"},
		{marker: "+++ ", prefix: "b/"},
	} {
		if strings.HasPrefix(line, header.marker) {
			if path, ok := bundleDiffPath(strings.TrimPrefix(line, header.marker), header.prefix); ok {
				candidates = append(candidates, path)
			}
		}
	}
	for _, marker := range []string{"rename from ", "rename to ", "copy from ", "copy to "} {
		if strings.HasPrefix(line, marker) {
			if path, ok := bundleExtendedDiffPath(strings.TrimPrefix(line, marker)); ok {
				candidates = append(candidates, path)
			}
		}
	}
	for _, candidate := range candidates {
		alreadyListed := false
		for _, listed := range paths {
			if listed == candidate {
				alreadyListed = true
				break
			}
		}
		if !alreadyListed {
			paths = append(paths, candidate)
		}
	}
	return paths
}

func ensureMergeBase(ctx context.Context, reviewTarget target.Target, commands runner.Runner) error {
	args := []string{"merge-base", reviewTarget.BaseSHA, reviewTarget.HeadSHA}
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-MERGE-BASE] cannot inspect the merge base: %w; run `git fetch --unshallow` or `git fetch --deepen=1` and retry", err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(string(result.Stdout)) == "" {
		return fmt.Errorf("[ROAST-BUNDLE-MERGE-BASE] Git cannot find a merge base for %s and %s; the clone may be shallow or the refs may be unrelated. Run `git fetch --unshallow` or `git fetch --deepen=1` and retry", reviewTarget.BaseSHA, reviewTarget.HeadSHA)
	}
	return nil
}

func ensureReviewObjects(ctx context.Context, reviewTarget target.Target, commands runner.Runner) error {
	if reviewTarget.Kind != target.KindPullRequest {
		return nil
	}
	missingBase, err := missingCommitObject(ctx, reviewTarget, reviewTarget.BaseSHA, commands)
	if err != nil {
		return err
	}
	missingHead, err := missingCommitObject(ctx, reviewTarget, reviewTarget.HeadSHA, commands)
	if err != nil {
		return err
	}
	if !missingBase && !missingHead {
		return nil
	}
	if reviewTarget.PRNumber <= 0 {
		return fmt.Errorf("[ROAST-BUNDLE-PR] PR commit objects are not available locally and the target has no PR number; rerun with a clone containing the reviewed refs")
	}

	remote, err := fetchRemote(ctx, reviewTarget, commands)
	if err != nil {
		return err
	}
	if missingHead {
		args := []string{"fetch", "--no-tags", remote, "refs/pull/" + strconv.Itoa(reviewTarget.PRNumber) + "/head"}
		if err := runRequired(ctx, reviewTarget, commands, args, "fetch PR head objects"); err != nil {
			return err
		}
	}
	if missingBase {
		args := []string{"fetch", "--no-tags", remote, reviewTarget.BaseSHA}
		if err := runRequired(ctx, reviewTarget, commands, args, "fetch PR base objects"); err != nil {
			return err
		}
	}

	stillMissingBase, err := missingCommitObject(ctx, reviewTarget, reviewTarget.BaseSHA, commands)
	if err != nil {
		return err
	}
	stillMissingHead, err := missingCommitObject(ctx, reviewTarget, reviewTarget.HeadSHA, commands)
	if err != nil {
		return err
	}
	if stillMissingBase || stillMissingHead {
		return fmt.Errorf("[ROAST-BUNDLE-PR] GitHub returned PR #%d, but Git still cannot read its base or head commit; run `git fetch --no-tags %s refs/pull/%d/head` and retry", reviewTarget.PRNumber, remote, reviewTarget.PRNumber)
	}
	return nil
}

func missingCommitObject(ctx context.Context, reviewTarget target.Target, sha string, commands runner.Runner) (bool, error) {
	if sha == "" {
		return true, nil
	}
	args := []string{"cat-file", "-e", sha + "^{commit}"}
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return false, fmt.Errorf("[ROAST-BUNDLE-PR] cannot inspect commit %s: %w", sha, err)
	}
	return result.ExitCode != 0, nil
}

func fetchRemote(ctx context.Context, reviewTarget target.Target, commands runner.Runner) (string, error) {
	args := []string{"remote"}
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return "", fmt.Errorf("[ROAST-BUNDLE-PR] cannot list Git remotes: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("[ROAST-BUNDLE-PR] cannot list Git remotes: %s; add a remote or use --base <ref>", runner.Failure("git", args, result))
	}
	remotes := strings.Fields(string(result.Stdout))
	if expected := repositoryIdentity(reviewTarget.PRURL); expected != "" {
		for _, remote := range remotes {
			remoteURL, urlErr := commands.Run(ctx, reviewTarget.RepoDir, "git", "remote", "get-url", remote)
			if urlErr != nil || remoteURL.ExitCode != 0 {
				continue
			}
			if repositoryIdentity(strings.TrimSpace(string(remoteURL.Stdout))) == expected {
				return remote, nil
			}
		}
		return "", fmt.Errorf("[ROAST-BUNDLE-PR] no Git remote matches PR repository %q; add that remote or fetch the PR objects manually", expected)
	}
	for _, remote := range remotes {
		if remote == "origin" {
			return remote, nil
		}
	}
	if len(remotes) > 0 {
		return remotes[0], nil
	}
	return "", fmt.Errorf("[ROAST-BUNDLE-PR] PR commits are not local and this repository has no Git remotes; add origin or use --base <ref>")
}

func repositoryIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") && strings.Contains(raw, ":") {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) == 2 {
			host := parts[0]
			if at := strings.LastIndex(host, "@"); at >= 0 {
				host = host[at+1:]
			}
			if identity := repositoryPathIdentity(host, parts[1]); identity != "" {
				return identity
			}
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return repositoryPathIdentity(parsed.Hostname(), parsed.Path)
}

func repositoryPathIdentity(host, path string) string {
	parts := strings.Split(strings.Trim(strings.TrimSuffix(path, ".git"), "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
}

func runRequired(ctx context.Context, reviewTarget target.Target, commands runner.Runner, args []string, action string) error {
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-PR] cannot %s: %w", action, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("[ROAST-BUNDLE-PR] cannot %s: %s; authenticate the remote or fetch the PR objects manually", action, runner.Failure("git", args, result))
	}
	return nil
}

func (b Bundle) Write(directory string) error {
	if directory == "" {
		return fmt.Errorf("[ROAST-BUNDLE-WRITE] bundle directory is empty; pass --bundle-dir <path>")
	}
	if _, err := PathOutsideRepository(directory, b.Target.RepoDir); err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-WRITE] cannot use bundle directory %q: %w; choose a directory outside the repository", directory, err)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-WRITE] create %s: %w", directory, err)
	}
	files := map[string][]byte{
		"diff.patch":   b.Diff,
		"snapshot.tar": b.Snapshot,
	}
	for name, content := range files {
		path := filepath.Join(directory, name)
		if err := WritePrivateFile(path, content); err != nil {
			return fmt.Errorf("[ROAST-BUNDLE-WRITE] write %s: %w", path, err)
		}
	}
	manifest, err := json.MarshalIndent(Manifest{
		Target:        b.Target,
		DiffBytes:     len(b.Diff),
		SnapshotBytes: len(b.Snapshot),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-WRITE] encode manifest: %w", err)
	}
	manifest = append(manifest, '\n')
	if err := WritePrivateFile(filepath.Join(directory, "manifest.json"), manifest); err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-WRITE] write manifest: %w", err)
	}
	return nil
}

// WritePrivateFile atomically writes a private file without following a stale
// destination symlink or changing an existing hard-linked file.
func WritePrivateFile(path string, content []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".roast-bundle-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err == nil {
		return nil
	} else {
		if info, statErr := os.Lstat(path); statErr != nil {
			return err
		} else if info.IsDir() {
			return fmt.Errorf("destination is a directory")
		}
	}
	// Windows does not replace an existing file with Rename. Remove only the
	// directory entry, so a stale symlink cannot redirect the replacement.
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

// PathOutsideRepository returns an absolute path when it is not within the
// repository's absolute path. Decision 19 keeps this boundary lexical for
// trusted first-party code instead of walking filesystem identities.
func PathOutsideRepository(pathValue, repoDir string) (string, error) {
	absolutePath, err := filepath.Abs(pathValue)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", pathValue, err)
	}
	if repoDir == "" {
		return absolutePath, nil
	}
	absoluteRepo, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve repository path %q: %w", repoDir, err)
	}
	relative, err := filepath.Rel(absoluteRepo, absolutePath)
	if err != nil {
		return "", fmt.Errorf("cannot compare path %q with repository %q: %w", pathValue, repoDir, err)
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return "", fmt.Errorf("path %q is inside repository %q", pathValue, repoDir)
	}
	return absolutePath, nil
}

func buildDiff(ctx context.Context, reviewTarget target.Target, commands runner.Runner) ([]byte, error) {
	switch reviewTarget.Kind {
	case target.KindDirty:
		return buildDirtyDiff(ctx, reviewTarget, commands)
	case target.KindCommit:
		if reviewTarget.RootCommit {
			return runGitDiff(ctx, reviewTarget, commands, []string{
				"-c", "diff.noprefix=false", "diff-tree", "--root", "-r", "--no-commit-id", "--patch", "--binary",
				"--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/",
				reviewTarget.HeadSHA,
			})
		}
		return runGitDiff(ctx, reviewTarget, commands, rangeArgs(reviewTarget.BaseSHA, reviewTarget.HeadSHA, false))
	case target.KindBase, target.KindPullRequest:
		return runGitDiff(ctx, reviewTarget, commands, rangeArgs(reviewTarget.BaseSHA, reviewTarget.HeadSHA, reviewTarget.ThreeDotDiff))
	default:
		return nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] unsupported target kind %q; resolve a dirty, base, commit, or pull request target", reviewTarget.Kind)
	}
}

func buildDirtyDiff(ctx context.Context, reviewTarget target.Target, commands runner.Runner) ([]byte, error) {
	tracked, err := runGitDiff(ctx, reviewTarget, commands, []string{
		"-c", "diff.noprefix=false", "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "HEAD", "--",
	})
	if err != nil {
		return nil, err
	}
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] cannot list untracked files: %w", err)
	}
	listArgs := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] cannot list untracked files: %s; retry from a clean Git worktree", runner.Failure("git", listArgs, result))
	}
	combined := append([]byte(nil), tracked...)
	for _, pathBytes := range bytes.Split(result.Stdout, []byte{0}) {
		if len(pathBytes) == 0 {
			continue
		}
		path := string(pathBytes)
		untracked, diffErr := runGitDiff(ctx, reviewTarget, commands, []string{
			"-c", "diff.noprefix=false", "diff", "--no-index", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "--", os.DevNull, path,
		})
		if diffErr != nil {
			return nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] cannot include untracked file %q: %w", path, diffErr)
		}
		combined = appendSeparated(combined, untracked)
	}
	return combined, nil
}

func rangeArgs(base, head string, threeDot bool) []string {
	rangeOperator := ".."
	if threeDot {
		rangeOperator = "..."
	}
	return []string{
		"-c", "diff.noprefix=false", "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/",
		base + rangeOperator + head,
	}
}

func runGitDiff(ctx context.Context, reviewTarget target.Target, commands runner.Runner, args []string) ([]byte, error) {
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] run git diff: %w", err)
	}
	if result.ExitCode != 0 {
		// git diff --no-index uses status 1 to report a valid difference.
		if containsArg(args, "--no-index") && result.ExitCode == 1 {
			return result.Stdout, nil
		}
		return nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] %s; verify the reviewed refs exist locally and retry", runner.Failure("git", args, result))
	}
	return result.Stdout, nil
}

func containsArg(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

func appendSeparated(existing, next []byte) []byte {
	if len(next) == 0 {
		return existing
	}
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		existing = append(existing, '\n')
	}
	if len(existing) > 0 && !bytes.HasPrefix(next, []byte("diff --git")) {
		existing = append(existing, '\n')
	}
	return append(existing, next...)
}

func filterSensitiveDiff(diff []byte) ([]byte, []string, error) {
	if len(diff) == 0 {
		return diff, nil, nil
	}
	lines := bytes.SplitAfter(diff, []byte("\n"))
	firstHeader := -1
	for index, line := range lines {
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			firstHeader = index
			break
		}
	}
	if firstHeader < 0 {
		return diff, nil, nil
	}

	var filtered bytes.Buffer
	excluded := make([]string, 0, 2)
	excludedSet := make(map[string]struct{})
	for _, line := range lines[:firstHeader] {
		_, _ = filtered.Write(line)
	}
	sectionStart := firstHeader
	for index := firstHeader + 1; index <= len(lines); index++ {
		if index < len(lines) && !bytes.HasPrefix(lines[index], []byte("diff --git ")) {
			continue
		}
		section := bytes.Join(lines[sectionStart:index], nil)
		sensitive, sensitivePaths, err := diffSectionHasSensitivePath(section)
		if err != nil {
			return nil, nil, fmt.Errorf("[ROAST-BUNDLE-DIFF] cannot identify a changed path while excluding sensitive files: %w; rebuild the review target and retry", err)
		}
		if !sensitive {
			_, _ = filtered.Write(section)
		} else {
			if diffSectionIsDeletion(section) {
				sectionStart = index
				continue
			}
			for _, name := range sensitivePaths {
				if _, exists := excludedSet[name]; exists {
					continue
				}
				excludedSet[name] = struct{}{}
				excluded = append(excluded, name)
			}
		}
		sectionStart = index
	}
	return filtered.Bytes(), excluded, nil
}

func diffSectionIsDeletion(section []byte) bool {
	for _, line := range strings.Split(string(section), "\n") {
		if strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "GIT binary patch") {
			return false
		}
		if strings.HasPrefix(line, "deleted file mode") {
			return true
		}
		if strings.HasPrefix(line, "+++ ") && strings.TrimSpace(strings.TrimPrefix(line, "+++ ")) == "/dev/null" {
			return true
		}
	}
	return false
}

func diffSectionHasSensitivePath(section []byte) (bool, []string, error) {
	lines := strings.Split(string(section), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "diff --git ") {
		return false, nil, fmt.Errorf("diff section is missing its Git header")
	}
	headerPaths := make([]string, 0, 2)
	if parsedHeaderPaths, ok := bundleDiffHeaderPaths(strings.TrimPrefix(lines[0], "diff --git ")); ok {
		headerPaths = append(headerPaths, parsedHeaderPaths...)
	}
	authoritativePaths := make([]string, 0, 4)
	inFileHeader := true
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "GIT binary patch") {
			inFileHeader = false
		}
		if !inFileHeader {
			continue
		}
		for _, header := range []struct {
			marker string
			prefix string
		}{
			{marker: "--- ", prefix: "a/"},
			{marker: "+++ ", prefix: "b/"},
		} {
			if !strings.HasPrefix(line, header.marker) {
				continue
			}
			if name, ok := bundleDiffPath(strings.TrimPrefix(line, header.marker), header.prefix); ok {
				authoritativePaths = append(authoritativePaths, name)
			}
		}
		for _, marker := range []string{"rename from ", "rename to ", "copy from ", "copy to "} {
			if !strings.HasPrefix(line, marker) {
				continue
			}
			if name, ok := bundleExtendedDiffPath(strings.TrimPrefix(line, marker)); ok {
				authoritativePaths = append(authoritativePaths, name)
			}
		}
	}
	paths := headerPaths
	if len(authoritativePaths) > 0 {
		paths = authoritativePaths
	}
	if len(paths) == 0 {
		return false, nil, fmt.Errorf("diff section has no readable file path")
	}
	seen := make(map[string]struct{}, len(paths))
	uniquePaths := make([]string, 0, len(paths))
	for _, name := range paths {
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		uniquePaths = append(uniquePaths, name)
	}
	sensitiveNames := make([]string, 0, 1)
	safeName := ""
	for _, name := range uniquePaths {
		if secrets.IsSensitivePath(name) {
			sensitiveNames = append(sensitiveNames, name)
			continue
		}
		if safeName == "" {
			safeName = name
		}
	}
	if len(sensitiveNames) > 0 && safeName != "" {
		return false, nil, fmt.Errorf("[ROAST-BUNDLE-SENSITIVE-RENAME] sensitive path %q changes name to %q; remove or revoke the credential before retry so it cannot enter the review snapshot", sensitiveNames[0], safeName)
	}
	if len(sensitiveNames) > 0 {
		return true, sensitiveNames, nil
	}
	return false, nil, nil
}

func bundleDiffHeaderPaths(header string) ([]string, bool) {
	header = strings.TrimLeft(strings.TrimSuffix(header, "\r"), " \t")
	if strings.HasPrefix(header, `"`) {
		oldPath, remainder, ok := readBundleGitPath(header)
		if !ok {
			return nil, false
		}
		remainder = strings.TrimLeft(remainder, " \t")
		newPath := remainder
		if strings.HasPrefix(remainder, `"`) {
			newPath, remainder, ok = readBundleGitPath(remainder)
		} else {
			remainder = ""
		}
		if !ok || strings.TrimSpace(remainder) != "" {
			return nil, false
		}
		paths := make([]string, 0, 2)
		if name, valid := bundleDiffPath(oldPath, "a/"); valid {
			paths = append(paths, name)
		}
		if name, valid := bundleDiffPath(newPath, "b/"); valid {
			paths = append(paths, name)
		}
		return paths, len(paths) > 0
	}
	for start := 0; start < len(header); {
		relative := strings.Index(header[start:], ` "b/`)
		if relative < 0 {
			break
		}
		separator := start + relative
		newPath, remainder, ok := readBundleGitPath(header[separator+1:])
		if ok && strings.TrimSpace(remainder) == "" {
			oldName, oldValid := bundleDiffPath(header[:separator], "a/")
			newName, newValid := bundleDiffPath(newPath, "b/")
			if oldValid && newValid {
				return []string{oldName, newName}, true
			}
		}
		start = separator + 1
	}
	var firstPaths []string
	candidates := 0
	for start := 0; start < len(header); {
		relative := strings.Index(header[start:], " b/")
		if relative < 0 {
			break
		}
		separator := start + relative
		oldName, oldValid := bundleDiffPath(header[:separator], "a/")
		newName, newValid := bundleDiffPath(header[separator+1:], "b/")
		if oldValid && newValid {
			if oldName == newName {
				return []string{oldName, newName}, true
			}
			if candidates == 0 {
				firstPaths = []string{oldName, newName}
			}
			candidates++
		}
		start = separator + 1
	}
	if candidates == 1 {
		return firstPaths, true
	}
	return nil, false
}

func bundleDiffPath(value, prefix string) (string, bool) {
	value = strings.TrimLeft(value, " \t")
	value = strings.TrimSuffix(value, "\t")
	if strings.HasPrefix(value, `"`) {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", false
		}
		value = decoded
	}
	if value == "/dev/null" || !strings.HasPrefix(value, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(value, prefix)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

func bundleExtendedDiffPath(value string) (string, bool) {
	value = strings.TrimLeft(value, " \t")
	if strings.HasPrefix(value, `"`) {
		decoded, remainder, ok := readBundleGitPath(value)
		if !ok || strings.TrimSpace(remainder) != "" {
			return "", false
		}
		value = decoded
	}
	if value == "" || value == "." || value == ".." || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "../") {
		return "", false
	}
	return value, true
}

func readBundleGitPath(value string) (string, string, bool) {
	value = strings.TrimLeft(value, " \t")
	if !strings.HasPrefix(value, `"`) {
		separator := strings.IndexByte(value, ' ')
		if separator < 0 {
			return value, "", true
		}
		return value[:separator], value[separator+1:], true
	}
	for index := 1; index < len(value); index++ {
		if value[index] != '"' || escapedGitQuote(value, index) {
			continue
		}
		quoted := value[:index+1]
		decoded, err := strconv.Unquote(quoted)
		if err != nil {
			return "", "", false
		}
		return decoded, value[index+1:], true
	}
	return "", "", false
}

func escapedGitQuote(value string, index int) bool {
	backslashes := 0
	for cursor := index - 1; cursor >= 0 && value[cursor] == '\\'; cursor-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func (b Bundle) String() string {
	return strings.TrimSpace(fmt.Sprintf("%s: %d diff bytes, %d snapshot bytes", b.Target.Label(), len(b.Diff), len(b.Snapshot)))
}
