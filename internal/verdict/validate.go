package verdict

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// Decode strictly parses one engine response. Unknown fields and trailing
// JSON are rejected so a model cannot quietly change the review contract, and
// each finding is read through the closed engine-side shape, so the
// review-side fields roast alone may set are refused even when zero-valued.
func Decode(raw []byte) (Verdict, error) {
	envelope, err := decodeEnvelope(raw, "engine response")
	if err != nil {
		return Verdict{}, err
	}
	findings := make([]Finding, 0, len(envelope.findings))
	for index, rawFinding := range envelope.findings {
		var finding engineFinding
		if err := decodeClosed(rawFinding, &finding); err != nil {
			return Verdict{}, fmt.Errorf("[ROAST-VERDICT-SCHEMA] finding %d has a field the engine may not return: %w; return only priority, file, line, title, rationale, and suggestion", index, err)
		}
		findings = append(findings, Finding{Priority: finding.Priority, File: finding.File, Line: finding.Line, Title: finding.Title, Rationale: finding.Rationale, Suggestion: finding.Suggestion})
	}
	verdict := Verdict{Overall: envelope.overall, Findings: findings, Provenance: envelope.provenance}
	if err := ValidateWithLocations(verdict, nil, nil, nil); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// engineFinding is the closed shape an engine returns: Finding without the
// review-side fields.
type engineFinding struct {
	Priority   Priority `json:"priority"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Title      string   `json:"title"`
	Rationale  string   `json:"rationale"`
	Suggestion string   `json:"suggestion,omitempty"`
}

type verdictEnvelope struct {
	overall    Overall
	findings   []json.RawMessage
	provenance Provenance
}

func decodeEnvelope(raw []byte, source string) (verdictEnvelope, error) {
	var envelope struct {
		Overall    *Overall           `json:"overall"`
		Findings   *[]json.RawMessage `json:"findings"`
		Provenance *Provenance        `json:"provenance"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return verdictEnvelope{}, fmt.Errorf("[ROAST-VERDICT-JSON] %s is not valid verdict JSON: %w; return one JSON object matching the schema", source, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return verdictEnvelope{}, fmt.Errorf("[ROAST-VERDICT-JSON] %s contains more than one JSON value; return exactly one verdict object", source)
		}
		return verdictEnvelope{}, fmt.Errorf("[ROAST-VERDICT-JSON] %s has trailing invalid JSON: %w; return exactly one verdict object", source, err)
	}
	if envelope.Overall == nil {
		return verdictEnvelope{}, fmt.Errorf("[ROAST-VERDICT-SCHEMA] missing required field overall; return overall as well_done or raw")
	}
	if envelope.Findings == nil {
		return verdictEnvelope{}, fmt.Errorf("[ROAST-VERDICT-SCHEMA] missing required field findings; return an array, even when it is empty")
	}
	if envelope.Provenance == nil {
		return verdictEnvelope{}, fmt.Errorf("[ROAST-VERDICT-SCHEMA] missing required field provenance; return target, branch, tree, engine, and context")
	}
	return verdictEnvelope{overall: *envelope.Overall, findings: *envelope.Findings, provenance: *envelope.Provenance}, nil
}

func decodeClosed(raw json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// ValidateWithLocations validates snapshot lines and the old/new ranges named
// by the reviewed diff.
func ValidateWithLocations(reviewVerdict Verdict, snapshotFiles map[string]struct{}, lineCounts map[string]int, diffRanges map[string][]LineRange) error {
	if reviewVerdict.Overall != OverallWellDone && reviewVerdict.Overall != OverallRaw {
		return fmt.Errorf("[ROAST-VERDICT-SCHEMA] overall %q is invalid; expected well_done or raw", reviewVerdict.Overall)
	}
	if reviewVerdict.Findings == nil {
		return fmt.Errorf("[ROAST-VERDICT-SCHEMA] findings must be an array, not null; return [] when there are no findings")
	}
	if strings.TrimSpace(reviewVerdict.Provenance.Target) == "" || strings.TrimSpace(reviewVerdict.Provenance.Branch) == "" || strings.TrimSpace(reviewVerdict.Provenance.Tree) == "" || strings.TrimSpace(reviewVerdict.Provenance.Engine) == "" || strings.TrimSpace(reviewVerdict.Provenance.Context) == "" {
		return fmt.Errorf("[ROAST-VERDICT-SCHEMA] provenance is incomplete; return non-empty target, branch, tree, engine, and context fields")
	}
	for index, finding := range reviewVerdict.Findings {
		if !finding.Priority.Valid() {
			return fmt.Errorf("[ROAST-VERDICT-SCHEMA] finding %d has invalid priority %q; expected P0, P1, P2, or P3", index, finding.Priority)
		}
		if err := validateFindingPath(finding.File); err != nil {
			return fmt.Errorf("[ROAST-VERDICT-SCHEMA] finding %d: %w", index, err)
		}
		if finding.Line < 1 {
			return fmt.Errorf("[ROAST-VERDICT-SCHEMA] finding %d has line %d; use the first affected line or another positive line number", index, finding.Line)
		}
		if strings.TrimSpace(finding.Title) == "" {
			return fmt.Errorf("[ROAST-VERDICT-SCHEMA] finding %d has an empty title; state the defect in one sentence", index)
		}
		if strings.TrimSpace(finding.Rationale) == "" {
			return fmt.Errorf("[ROAST-VERDICT-SCHEMA] finding %d has an empty rationale; include the concrete failure scenario", index)
		}
		if snapshotFiles != nil {
			if _, exists := snapshotFiles[finding.File]; !exists {
				return fmt.Errorf("[ROAST-VERDICT-FILE] finding %d references %q, which is not in the reviewed snapshot or target diff; choose a tracked snapshot file or a path shown in the reviewed diff", index, finding.File)
			}
		}
		if lineCounts != nil {
			maximumLine, hasSnapshotLines := lineCounts[finding.File]
			if hasSnapshotLines && finding.Line > maximumLine {
				if diffRanges == nil || !lineInRanges(finding.Line, diffRanges[finding.File]) {
					return invalidLineError(index, finding, maximumLine)
				}
			} else if !hasSnapshotLines && diffRanges != nil && !lineInRanges(finding.Line, diffRanges[finding.File]) {
				return fmt.Errorf("[ROAST-VERDICT-LINE] finding %d points to line %d in diff-only file %q, but that line is outside the reviewed hunk ranges; use a line shown in the diff", index, finding.Line, finding.File)
			}
		}
	}
	open := openFindings(reviewVerdict.Findings)
	if reviewVerdict.Overall == OverallWellDone && open > 0 {
		return fmt.Errorf("[ROAST-VERDICT-SCHEMA] overall is well_done but %d finding(s) are open; use raw when any open finding remains", open)
	}
	if reviewVerdict.Overall == OverallRaw && open == 0 {
		return fmt.Errorf("[ROAST-VERDICT-SCHEMA] overall is raw but no finding is open; use well_done when no open finding remains")
	}
	return nil
}

func invalidLineError(index int, finding Finding, maximumLine int) error {
	return fmt.Errorf("[ROAST-VERDICT-LINE] finding %d points to line %d in %q, but the file has only %d line(s); use a line present in the reviewed file or diff", index, finding.Line, finding.File, maximumLine)
}

func validateFindingPath(name string) error {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("finding file %q is not a clean relative path; use a path such as internal/example.go", name)
	}
	return nil
}

// DiffFiles returns paths named by Git's diff headers, including deleted paths
// that cannot appear in the snapshot at the reviewed head.
func DiffFiles(diff []byte) map[string]struct{} {
	files := make(map[string]struct{})
	fileHeader := false
	for _, line := range strings.Split(string(diff), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			parseDiffHeader(line[len("diff --git "):], files)
			fileHeader = true
			continue
		}
		if !fileHeader {
			continue
		}
		if strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "GIT binary patch") {
			fileHeader = false
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			addDiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "--- "), "\t"), "a/", files)
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			addDiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t"), "b/", files)
			fileHeader = false
		}
	}
	return files
}

// DiffDeletedLines returns, per path, the old-side line numbers a unified Git
// diff removes. A finding's line has no side, so a finding on one of these
// numbers may mean the deleted line, not the post-change line that now holds
// the same number.
func DiffDeletedLines(diff []byte) map[string]map[int]struct{} {
	deleted := make(map[string]map[int]struct{})
	oldPath := ""
	oldLine := 0
	inHunk := false
	for _, line := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, _, _ = diffHeaderPaths(line[len("diff --git "):])
			inHunk = false
		case !inHunk && strings.HasPrefix(line, "--- "):
			oldPath, _ = cleanDiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "--- "), "\t"), "a/")
		case strings.HasPrefix(line, "@@ "):
			oldStart, _, _, _, ok := parseHunkHeader(line)
			inHunk = ok
			oldLine = oldStart
		case inHunk && strings.HasPrefix(line, "-"):
			if oldPath != "" {
				if deleted[oldPath] == nil {
					deleted[oldPath] = make(map[int]struct{})
				}
				deleted[oldPath][oldLine] = struct{}{}
			}
			oldLine++
		case inHunk && (strings.HasPrefix(line, " ") || line == ""):
			oldLine++
		case inHunk && strings.HasPrefix(line, "\\"):
		case inHunk && strings.HasPrefix(line, "+"):
		default:
			inHunk = false
		}
	}
	return deleted
}

// DiffAddedFiles returns the paths a unified Git diff creates, so a finding on
// a new file is never blamed against history it does not have.
func DiffAddedFiles(diff []byte) map[string]struct{} {
	added := make(map[string]struct{})
	fromNull := false
	for _, line := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			fromNull = false
		case strings.HasPrefix(line, "--- "):
			fromNull = strings.TrimSuffix(strings.TrimPrefix(line, "--- "), "\t") == "/dev/null"
		case fromNull && strings.HasPrefix(line, "+++ "):
			addDiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t"), "b/", added)
			fromNull = false
		}
	}
	return added
}

type LineRange struct {
	Start int
	End   int
}

// DiffLineRanges returns old- and new-side hunk ranges for every path named
// by a unified Git diff.
func DiffLineRanges(diff []byte) map[string][]LineRange {
	ranges := make(map[string][]LineRange)
	oldPath, newPath := "", ""
	fileActive := false
	fileHeaderActive := false
	for _, line := range strings.Split(string(diff), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			oldPath, newPath, _ = diffHeaderPaths(line[len("diff --git "):])
			fileActive = true
			fileHeaderActive = true
			continue
		}
		if !fileActive {
			continue
		}
		if fileHeaderActive && strings.HasPrefix(line, "--- ") {
			oldPath, _ = cleanDiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "--- "), "\t"), "a/")
			continue
		}
		if fileHeaderActive && strings.HasPrefix(line, "+++ ") {
			newPath, _ = cleanDiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t"), "b/")
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			oldStart, oldCount, newStart, newCount, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			if oldPath != "" && oldCount > 0 {
				ranges[oldPath] = append(ranges[oldPath], LineRange{Start: oldStart, End: oldStart + oldCount - 1})
			}
			if newPath != "" && newCount > 0 {
				ranges[newPath] = append(ranges[newPath], LineRange{Start: newStart, End: newStart + newCount - 1})
			}
			fileHeaderActive = false
		}
	}
	return ranges
}

func parseHunkHeader(line string) (int, int, int, int, bool) {
	value := strings.TrimPrefix(line, "@@ ")
	if end := strings.Index(value, " @@"); end >= 0 {
		value = value[:end]
	}
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return 0, 0, 0, 0, false
	}
	oldStart, oldCount, ok := parseRangeSpec(fields[0], '-')
	if !ok {
		return 0, 0, 0, 0, false
	}
	newStart, newCount, ok := parseRangeSpec(fields[1], '+')
	if !ok {
		return 0, 0, 0, 0, false
	}
	return oldStart, oldCount, newStart, newCount, true
}

func parseRangeSpec(spec string, prefix byte) (int, int, bool) {
	if len(spec) < 2 || spec[0] != prefix {
		return 0, 0, false
	}
	parts := strings.SplitN(spec[1:], ",", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 0 {
		return 0, 0, false
	}
	count := 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			return 0, 0, false
		}
	}
	return start, count, true
}

func lineInRanges(line int, ranges []LineRange) bool {
	for _, lineRange := range ranges {
		if line >= lineRange.Start && line <= lineRange.End {
			return true
		}
	}
	return false
}

func parseDiffHeader(header string, files map[string]struct{}) {
	oldPath, newPath, ok := diffHeaderPaths(header)
	if !ok {
		return
	}
	if oldPath != "" {
		files[oldPath] = struct{}{}
	}
	if newPath != "" {
		files[newPath] = struct{}{}
	}
}

func diffHeaderPaths(header string) (string, string, bool) {
	header = strings.TrimLeft(header, " \t")
	if strings.HasPrefix(header, `"`) {
		oldPath, remainder, ok := readGitPath(header)
		if !ok {
			return "", "", false
		}
		oldName, oldOK := cleanDiffPath(oldPath, "a/")
		newName, newOK := cleanDiffPath(remainder, "b/")
		return oldName, newName, oldOK || newOK
	}
	separator := strings.Index(header, ` "b/`)
	if separator < 0 {
		separator = strings.Index(header, " b/")
	}
	if strings.Count(header, " b/") > 1 {
		// An unquoted pathname containing " b/" is ambiguous in the diff
		// header. The ---/+++ file headers below carry the exact path.
		return "", "", false
	}
	if separator < 0 {
		return "", "", false
	}
	oldName, oldOK := cleanDiffPath(header[:separator], "a/")
	newName, newOK := cleanDiffPath(header[separator+1:], "b/")
	return oldName, newName, oldOK || newOK
}

func addDiffPath(value, prefix string, files map[string]struct{}) {
	name, ok := cleanDiffPath(value, prefix)
	if ok {
		files[name] = struct{}{}
	}
}

func cleanDiffPath(value, prefix string) (string, bool) {
	value = strings.TrimLeft(value, " \t")
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
	if err := validateFindingPath(name); err == nil {
		return name, true
	}
	return "", false
}

func readGitPath(value string) (string, string, bool) {
	value = strings.TrimLeft(value, " \t")
	if !strings.HasPrefix(value, `"`) {
		separator := strings.IndexByte(value, ' ')
		if separator < 0 {
			return value, "", true
		}
		return value[:separator], value[separator+1:], true
	}
	escaped := false
	for index := 1; index < len(value); index++ {
		character := value[index]
		if character == '\\' && !escaped {
			escaped = true
			continue
		}
		if character == '"' && !escaped {
			quoted := value[:index+1]
			decoded, err := strconv.Unquote(quoted)
			if err != nil {
				return "", "", false
			}
			return decoded, value[index+1:], true
		}
		escaped = false
	}
	return "", "", false
}

// SnapshotFiles returns the regular file names in a bundle snapshot.
func SnapshotFiles(snapshot []byte) (map[string]struct{}, error) {
	archive := tar.NewReader(bytes.NewReader(snapshot))
	files := make(map[string]struct{})
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] cannot read snapshot tar: %w; rebuild the review bundle and retry", err)
		}
		if _, err := io.Copy(io.Discard, archive); err != nil {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] cannot read snapshot entry %q: %w; rebuild the review bundle and retry", header.Name, err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if err := validateFindingPath(header.Name); err != nil {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] invalid snapshot path %q: %w", header.Name, err)
		}
		if _, exists := files[header.Name]; exists {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] snapshot contains duplicate file %q; rebuild the review bundle and retry", header.Name)
		}
		files[header.Name] = struct{}{}
	}
}

// SnapshotLineCounts returns the number of addressable lines in each regular
// snapshot file without retaining the file contents after each entry.
func SnapshotLineCounts(snapshot []byte) (map[string]int, error) {
	archive := tar.NewReader(bytes.NewReader(snapshot))
	lineCounts := make(map[string]int)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return lineCounts, nil
		}
		if err != nil {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] cannot read snapshot tar: %w; rebuild the review bundle and retry", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if err := validateFindingPath(header.Name); err != nil {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] invalid snapshot path %q: %w", header.Name, err)
		}
		lines, err := countLinesReader(archive)
		if err != nil {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] cannot count lines in snapshot entry %q: %w; rebuild the review bundle and retry", header.Name, err)
		}
		if _, exists := lineCounts[header.Name]; exists {
			return nil, fmt.Errorf("[ROAST-VERDICT-SNAPSHOT] snapshot contains duplicate file %q; rebuild the review bundle and retry", header.Name)
		}
		lineCounts[header.Name] = lines
	}
}

func countLinesReader(reader io.Reader) (int, error) {
	buffer := make([]byte, 32*1024)
	lines := 0
	readBytes := 0
	var lastByte byte
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			lines += bytes.Count(buffer[:count], []byte{'\n'})
			lastByte = buffer[count-1]
			readBytes += count
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if readBytes > 0 && lastByte != '\n' {
		lines++
	}
	return lines, nil
}

// Filter removes findings below the configured threshold and settles the
// gate outcome from the open findings that remain.
func Filter(reviewVerdict Verdict, maximum Priority) (Verdict, error) {
	if !maximum.Valid() {
		return Verdict{}, fmt.Errorf("[ROAST-PRIORITY] maximum priority %q is invalid; expected P0, P1, P2, or P3", maximum)
	}
	filtered := make([]Finding, 0, len(reviewVerdict.Findings))
	for _, finding := range reviewVerdict.Findings {
		if maximum.Includes(finding.Priority) {
			filtered = append(filtered, finding)
		}
	}
	reviewVerdict.Findings = filtered
	return Settle(reviewVerdict), nil
}

// Encode writes the stable JSON representation used by --json-output.
func Encode(reviewVerdict Verdict) ([]byte, error) {
	if reviewVerdict.Findings == nil {
		reviewVerdict.Findings = []Finding{}
	}
	encoded, err := json.MarshalIndent(reviewVerdict, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("[ROAST-VERDICT-JSON] cannot encode verdict: %w", err)
	}
	return append(encoded, '\n'), nil
}
