package secrets

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
)

const verifiedExitCode = 183

// Scan runs TruffleHog against the complete preimage and postimage content for
// changed files. A missing
// scanner, scanner failure, or verified result blocks the review.
func Scan(ctx context.Context, reviewTarget target.Target, diff, scanSnapshot []byte, commands runner.Runner) error {
	binary, err := FindBinary()
	if err != nil {
		return err
	}
	return scanWithTarget(ctx, reviewTarget, diff, scanSnapshot, binary, commands)
}

// FindBinary locates the platform-specific TruffleHog executable. An explicit
// path is useful for hermetic CI and keeps the remediation in the error clear.
func FindBinary() (string, error) {
	for _, variable := range []string{"ROAST_TRUFFLEHOG", "TRUFFLEHOG"} {
		if configured := strings.TrimSpace(os.Getenv(variable)); configured != "" {
			binary, err := exec.LookPath(configured)
			if err == nil {
				return binary, nil
			}
		}
	}
	for _, name := range binaryNames(runtime.GOOS) {
		binary, err := exec.LookPath(name)
		if err == nil {
			return binary, nil
		}
	}
	return "", fmt.Errorf("[ROAST-SECRET-BINARY] TruffleHog was not found on PATH; install it and retry (`brew install trufflehog` on macOS, or download the %q release binary for Linux/Windows), or set ROAST_TRUFFLEHOG=/absolute/path/to/trufflehog; no review engine was called", binaryNames(runtime.GOOS)[0])
}

func binaryNames(goos string) []string {
	if goos == "windows" {
		return []string{"trufflehog.exe", "trufflehog"}
	}
	return []string{"trufflehog"}
}

func scanWithBinary(ctx context.Context, repoDir string, diff []byte, binary string, commands runner.Runner) error {
	return scanWithBinaryPostimages(ctx, repoDir, diff, binary, nil, commands)
}

func scanWithTarget(ctx context.Context, reviewTarget target.Target, diff, scanSnapshot []byte, binary string, commands runner.Runner) error {
	if commands == nil {
		commands = runner.ExecRunner{}
	}
	temporaryDirectory, err := os.MkdirTemp("", "roast-secret-scan-")
	if err != nil {
		return fmt.Errorf("[ROAST-SECRET-SCAN] cannot create the temporary changed-content scan directory: %w; fix the local filesystem and retry; no review engine was called", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	postimages, err := scanSnapshotFiles(scanSnapshot)
	if err != nil {
		return fmt.Errorf("[ROAST-SECRET-SCAN] cannot read captured scan snapshot: %w; rebuild the review target and retry; no review engine was called", err)
	}
	preimageRef, err := resolvePreimageRef(ctx, reviewTarget, commands)
	if err != nil {
		return err
	}
	aliases, err := writeTargetScanFiles(temporaryDirectory, ctx, reviewTarget, diff, preimageRef, postimages, commands)
	if err != nil {
		return err
	}
	return runScanner(ctx, reviewTarget.RepoDir, binary, temporaryDirectory, aliases, commands)
}

func scanWithBinaryPostimages(ctx context.Context, repoDir string, diff []byte, binary string, postimages map[string][]byte, commands runner.Runner) error {
	if commands == nil {
		commands = runner.ExecRunner{}
	}
	temporaryDirectory, err := os.MkdirTemp("", "roast-secret-scan-")
	if err != nil {
		return fmt.Errorf("[ROAST-SECRET-SCAN] cannot create the temporary changed-content scan directory: %w; fix the local filesystem and retry; no review engine was called", err)
	}
	defer os.RemoveAll(temporaryDirectory)

	aliases, err := writeChangedFiles(temporaryDirectory, diff, postimages)
	if err != nil {
		return err
	}
	return runScanner(ctx, repoDir, binary, temporaryDirectory, aliases, commands)
}

func runScanner(ctx context.Context, repoDir, binary, temporaryDirectory string, aliases map[string]string, commands runner.Runner) error {
	args := []string{
		"filesystem",
		"--json",
		"--no-update",
		"--no-color",
		"--results=verified",
		"--fail",
		"--fail-on-scan-errors",
		temporaryDirectory,
	}
	result, err := commands.Run(ctx, repoDir, binary, args...)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		return fmt.Errorf("[ROAST-SECRET-SCAN] could not run TruffleHog: %w; verify the executable works, then retry; no review engine was called", err)
	}
	locations := verifiedLocations(result.Stdout, aliases)
	if result.ExitCode == verifiedExitCode || len(locations) > 0 {
		if len(locations) > 0 {
			return fmt.Errorf("[ROAST-SECRET-VERIFIED] TruffleHog found a verified secret in changed content (%s); revoke or remove it, then rerun roast; no review engine was called", strings.Join(locations, ", "))
		}
		return fmt.Errorf("[ROAST-SECRET-VERIFIED] TruffleHog found a verified secret in changed content; revoke or remove it, then rerun roast; no review engine was called")
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("[ROAST-SECRET-SCAN] TruffleHog could not complete the changed-content scan: %s; fix the scanner error and retry; no review engine was called", runner.Failure(binary, args, result))
	}
	return nil
}

type trufflehogResult struct {
	Verified       bool   `json:"Verified"`
	DetectorName   string `json:"DetectorName"`
	SourceMetadata struct {
		Data struct {
			Filesystem struct {
				File string `json:"file"`
			} `json:"Filesystem"`
		} `json:"Data"`
	} `json:"SourceMetadata"`
}

func verifiedLocations(output []byte, aliases map[string]string) []string {
	locations := make([]string, 0, 1)
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		var result trufflehogResult
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil || !result.Verified {
			continue
		}
		location := result.SourceMetadata.Data.Filesystem.File
		if location == "" {
			location = result.DetectorName
		}
		if location == "" {
			continue
		}
		cleanLocation := filepath.Clean(location)
		if source, ok := aliases[cleanLocation]; ok {
			location = source
		} else {
			location = filepath.Base(cleanLocation)
		}
		if _, exists := seen[location]; exists {
			continue
		}
		seen[location] = struct{}{}
		locations = append(locations, strconv.Quote(location))
	}
	return locations
}

func writeChangedFiles(directory string, diff []byte, postimages map[string][]byte) (map[string]string, error) {
	files, err := changedFiles(diff, postimages)
	if err != nil {
		return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot prepare changed content for TruffleHog: %w; rebuild the review target and retry; no review engine was called", err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	independentPaths := needsIndependentScanPaths(names)
	aliases := make(map[string]string, len(names))
	for index, name := range names {
		scanName := name
		if independentPaths {
			scanName = boundedScanName("entry", index, name)
		}
		scanPath, err := safeScanPath(directory, scanName)
		if err != nil {
			return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot prepare changed file %q: %w; rebuild the review target and retry; no review engine was called", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(scanPath), 0o700); err != nil {
			return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot create changed-content directory for %q: %w; fix the local filesystem and retry; no review engine was called", name, err)
		}
		if err := os.WriteFile(scanPath, files[name], 0o600); err != nil {
			return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot write changed content for %q: %w; fix the local filesystem and retry; no review engine was called", name, err)
		}
		aliases[filepath.Clean(scanPath)] = name
	}
	return aliases, nil
}

func boundedScanName(side string, index int, source string) string {
	digest := sha256.Sum256([]byte(side + "\x00" + source))
	return fmt.Sprintf("%s-%06d-%s", side, index, hex.EncodeToString(digest[:8]))
}

type reviewDiffFile struct {
	oldPath      string
	newPath      string
	oldGitlink   bool
	newGitlink   bool
	oldModeKnown bool
	newModeKnown bool
}

func writeTargetScanFiles(directory string, ctx context.Context, reviewTarget target.Target, diff []byte, preimageRef string, postimages map[string][]byte, commands runner.Runner) (map[string]string, error) {
	diffFiles, err := reviewDiffFiles(diff)
	if err != nil {
		return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot identify changed files for TruffleHog: %w; rebuild the review target and retry; no review engine was called", err)
	}
	aliases := make(map[string]string, len(diffFiles)*2)
	imageIndex := 0
	for _, diffFile := range diffFiles {
		for _, image := range []struct {
			path    string
			side    string
			post    bool
			gitlink bool
		}{
			{path: diffFile.oldPath, side: "base", post: false, gitlink: diffFile.oldGitlink},
			{path: diffFile.newPath, side: "head", post: true, gitlink: diffFile.newGitlink},
		} {
			if image.path == "" || image.gitlink {
				continue
			}
			content, present, err := readTargetImage(ctx, reviewTarget, image.path, image.post, preimageRef, postimages, commands)
			if err != nil {
				return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot read %s image for %q: %w; rebuild the review target and retry; no review engine was called", image.side, image.path, err)
			}
			if !present {
				continue
			}
			name := boundedScanName(image.side, imageIndex, image.path)
			imageIndex++
			scanPath, err := safeScanPath(directory, name)
			if err != nil {
				return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot prepare %s image for %q: %w; rebuild the review target and retry; no review engine was called", image.side, image.path, err)
			}
			if err := os.WriteFile(scanPath, content, 0o600); err != nil {
				return nil, fmt.Errorf("[ROAST-SECRET-SCAN] cannot write %s image for %q: %w; fix the local filesystem and retry; no review engine was called", image.side, image.path, err)
			}
			aliases[filepath.Clean(scanPath)] = image.path
		}
	}
	return aliases, nil
}

func resolvePreimageRef(ctx context.Context, reviewTarget target.Target, commands runner.Runner) (string, error) {
	if !reviewTarget.ThreeDotDiff {
		return reviewTarget.BaseSHA, nil
	}
	args := []string{"merge-base", reviewTarget.BaseSHA, reviewTarget.HeadSHA}
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return "", fmt.Errorf("[ROAST-SECRET-MERGE-BASE] cannot inspect the merge base: %w; rebuild the review target and retry; no review engine was called", err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(string(result.Stdout)) == "" {
		return "", fmt.Errorf("[ROAST-SECRET-MERGE-BASE] Git cannot find a merge base for %s and %s; fetch the reviewed history and retry; no review engine was called", reviewTarget.BaseSHA, reviewTarget.HeadSHA)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

func scanSnapshotFiles(data []byte) (map[string][]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	files := make(map[string][]byte)
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if _, err := safeScanPath("scan-snapshot", name); err != nil {
			return nil, fmt.Errorf("invalid captured path %q: %w", header.Name, err)
		}
		if _, exists := files[name]; exists {
			return nil, fmt.Errorf("captured snapshot contains duplicate path %q", name)
		}
		var content []byte
		if header.Typeflag == tar.TypeSymlink {
			content = []byte(header.Linkname)
		} else if header.Typeflag == tar.TypeReg || header.Typeflag == 0 {
			content, err = io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
		} else {
			continue
		}
		files[name] = content
	}
}

func readTargetImage(ctx context.Context, reviewTarget target.Target, name string, post bool, preimageRef string, postimages map[string][]byte, commands runner.Runner) ([]byte, bool, error) {
	if post && postimages != nil {
		content, exists := postimages[name]
		if !exists {
			return nil, false, fmt.Errorf("captured postimage does not contain %q", name)
		}
		return content, true, nil
	}
	if reviewTarget.Kind == target.KindDirty && post {
		path, err := safeScanPath(reviewTarget.RepoDir, name)
		if err != nil {
			return nil, false, err
		}
		content, err := readDirtyImage(path)
		if err != nil {
			return nil, false, err
		}
		return content, true, nil
	}
	ref := preimageRef
	if post {
		ref = reviewTarget.SnapshotRef
	}
	if ref == "" {
		return nil, false, nil
	}
	args := []string{"cat-file", "blob", ref + ":" + name}
	result, err := commands.Run(ctx, reviewTarget.RepoDir, "git", args...)
	if err != nil {
		return nil, false, err
	}
	if result.ExitCode != 0 {
		return nil, false, runner.Failure("git", args, result)
	}
	return result.Stdout, true, nil
}

func readDirtyImage(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		return []byte(linkTarget), nil
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	// Secret scanning retains the captured preimage/postimage contract; this
	// read intentionally uses the plain trusted-worktree file primitive.
	return os.ReadFile(path)
}

func reviewDiffFiles(diff []byte) ([]reviewDiffFile, error) {
	files := make([]reviewDiffFile, 0, 2)
	current := reviewDiffFile{}
	hasSection := false
	inHunk := false
	finish := func() error {
		if !hasSection {
			return nil
		}
		if current.oldPath == "" && current.newPath == "" {
			return fmt.Errorf("diff section has no readable file path")
		}
		files = append(files, current)
		return nil
	}

	for _, rawLine := range strings.Split(string(diff), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if err := finish(); err != nil {
				return nil, err
			}
			oldPath, newPath, _ := diffHeaderPaths(strings.TrimPrefix(line, "diff --git "))
			current = reviewDiffFile{oldPath: oldPath, newPath: newPath}
			hasSection = true
			inHunk = false
		case strings.HasPrefix(line, "deleted file mode"):
			current.newPath = ""
			current.oldModeKnown = true
			current.oldGitlink = strings.HasSuffix(strings.TrimSpace(line), "160000")
		case strings.HasPrefix(line, "new file mode"):
			current.oldPath = ""
			current.newModeKnown = true
			current.newGitlink = strings.HasSuffix(strings.TrimSpace(line), "160000")
		case strings.HasPrefix(line, "old mode "):
			current.oldModeKnown = true
			current.oldGitlink = strings.TrimSpace(strings.TrimPrefix(line, "old mode ")) == "160000"
		case strings.HasPrefix(line, "new mode "):
			current.newModeKnown = true
			current.newGitlink = strings.TrimSpace(strings.TrimPrefix(line, "new mode ")) == "160000"
		case strings.HasPrefix(line, "index "):
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[len(fields)-1] == "160000" && !current.oldModeKnown && !current.newModeKnown {
				current.oldGitlink = true
				current.newGitlink = true
			}
		case strings.HasPrefix(line, "--- ") && !inHunk:
			current.oldPath = patchPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ ") && !inHunk:
			current.newPath = patchPath(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "rename from "):
			current.oldPath, _ = extendedDiffPath(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			current.newPath, _ = extendedDiffPath(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "copy from "):
			current.oldPath, _ = extendedDiffPath(strings.TrimPrefix(line, "copy from "))
		case strings.HasPrefix(line, "copy to "):
			current.newPath, _ = extendedDiffPath(strings.TrimPrefix(line, "copy to "))
		case strings.HasPrefix(line, "@@ "):
			inHunk = true
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return files, nil
}

func extendedDiffPath(value string) (string, bool) {
	value = strings.TrimSuffix(value, "\r")
	if strings.HasPrefix(value, `"`) {
		decoded, remainder, ok := readGitQuotedPath(value)
		if !ok || strings.TrimSpace(remainder) != "" {
			return "", false
		}
		value = decoded
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

func needsIndependentScanPaths(names []string) bool {
	canonical := make([]string, len(names))
	seen := make(map[string]struct{}, len(names))
	for index, name := range names {
		value := strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))))
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
		canonical[index] = value
	}
	for index, current := range canonical {
		for previous := 0; previous < index; previous++ {
			if strings.HasPrefix(current, canonical[previous]+"/") || strings.HasPrefix(canonical[previous], current+"/") {
				return true
			}
		}
	}
	return false
}

func safeScanPath(directory, name string) (string, error) {
	name = filepath.FromSlash(name)
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("path is outside the temporary scan directory")
	}
	return filepath.Join(directory, clean), nil
}

func changedFiles(diff []byte, postimages map[string][]byte) (map[string][]byte, error) {
	files := make(map[string][]byte)
	currentPath := ""
	oldPath := ""
	newPath := ""
	inHunk := false
	inBinaryPatch := false
	for _, rawLine := range strings.Split(string(diff), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, newPath, _ = diffHeaderPaths(strings.TrimPrefix(line, "diff --git "))
			currentPath = newPath
			if currentPath == "" {
				currentPath = oldPath
			}
			inHunk = false
			inBinaryPatch = false
		case strings.HasPrefix(line, "--- ") && !inHunk:
			oldPath = patchPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ ") && !inHunk:
			newPath = patchPath(strings.TrimPrefix(line, "+++ "), "b/")
			currentPath = newPath
			if currentPath == "" {
				currentPath = oldPath
			}
			inHunk = false
		case strings.HasPrefix(line, "@@ "):
			inHunk = currentPath != ""
		case strings.HasPrefix(line, "GIT binary patch"):
			inBinaryPatch = true
			inHunk = false
			if content, ok := postimages[currentPath]; ok {
				files[currentPath] = append([]byte(nil), content...)
			}
		case inBinaryPatch:
			continue
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ")):
			files[currentPath] = append(files[currentPath], []byte(line[1:]+"\n")...)
		}
	}
	return files, nil
}

func diffHeaderPaths(value string) (string, string, bool) {
	value = strings.TrimLeft(strings.TrimSuffix(value, "\r"), " \t")
	if strings.HasPrefix(value, `"`) {
		oldRaw, remainder, ok := readGitQuotedPath(value)
		if !ok {
			return "", "", false
		}
		remainder = strings.TrimLeft(remainder, " \t")
		newRaw := remainder
		if strings.HasPrefix(remainder, `"`) {
			newRaw, remainder, ok = readGitQuotedPath(remainder)
			if !ok || strings.TrimSpace(remainder) != "" {
				return "", "", false
			}
		}
		return patchPath(oldRaw, "a/"), patchPath(newRaw, "b/"), true
	}
	for start := 0; start < len(value); {
		relative := strings.Index(value[start:], ` "b/`)
		if relative < 0 {
			break
		}
		separator := start + relative
		newRaw, remainder, ok := readGitQuotedPath(value[separator+1:])
		if ok && strings.TrimSpace(remainder) == "" {
			oldPath := patchPath(value[:separator], "a/")
			newPath := patchPath(newRaw, "b/")
			if oldPath != "" && newPath != "" {
				return oldPath, newPath, true
			}
		}
		start = separator + 1
	}
	firstOld, firstNew := "", ""
	candidates := 0
	for start := 0; start < len(value); {
		relative := strings.Index(value[start:], " b/")
		if relative < 0 {
			break
		}
		separator := start + relative
		oldPath := patchPath(value[:separator], "a/")
		newPath := patchPath(value[separator+1:], "b/")
		if oldPath != "" && newPath != "" {
			if oldPath == newPath {
				return oldPath, newPath, true
			}
			if candidates == 0 {
				firstOld, firstNew = oldPath, newPath
			}
			candidates++
		}
		start = separator + 1
	}
	if candidates == 1 {
		return firstOld, firstNew, true
	}
	return "", "", false
}

func readGitQuotedPath(value string) (string, string, bool) {
	value = strings.TrimLeft(value, " \t")
	if !strings.HasPrefix(value, `"`) {
		return "", "", false
	}
	for index := 1; index < len(value); index++ {
		if value[index] != '"' {
			continue
		}
		backslashes := 0
		for previous := index - 1; previous >= 0 && value[previous] == '\\'; previous-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continue
		}
		decoded, err := strconv.Unquote(value[:index+1])
		if err != nil {
			return "", "", false
		}
		return decoded, value[index+1:], true
	}
	return "", "", false
}

func patchPath(value, prefix string) string {
	value = strings.TrimSuffix(value, "\t")
	value = strings.TrimSuffix(value, "\r")
	if value == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(value, `"`) {
		decoded, err := strconvUnquote(value)
		if err != nil {
			return ""
		}
		value = decoded
	}
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	name := strings.TrimPrefix(value, prefix)
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return ""
	}
	return clean
}

func strconvUnquote(value string) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("invalid quoted Git path")
	}
	var decoded string
	var err error
	decoded, err = strconv.Unquote(value)
	if err != nil {
		return "", err
	}
	return decoded, nil
}
