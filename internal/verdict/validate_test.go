package verdict

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeRejectsUnknownAndTrailingFields(t *testing.T) {
	valid := `{"overall":"well_done","findings":[],"provenance":{"target":"HEAD","branch":"main","tree":"abc","engine":"fake/canned","context":"snapshot"}}`
	if _, err := Decode([]byte(strings.Replace(valid, `"findings":[]`, `"unexpected":true,"findings":[]`, 1))); err == nil || !strings.Contains(err.Error(), "ROAST-VERDICT-JSON") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := Decode([]byte(valid + " {}\n")); err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Fatalf("trailing value error = %v", err)
	}
}

func TestValidateRequiresSnapshotFile(t *testing.T) {
	reviewVerdict := validRawVerdict("internal/example.go")
	if err := ValidateWithLocations(reviewVerdict, map[string]struct{}{"README.md": {}}, nil, nil); err == nil || !strings.Contains(err.Error(), "ROAST-VERDICT-FILE") {
		t.Fatalf("missing file error = %v", err)
	}
	if err := ValidateWithLocations(reviewVerdict, map[string]struct{}{"internal/example.go": {}}, nil, nil); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
}

func TestValidateRejectsLineOutsideSnapshot(t *testing.T) {
	reviewVerdict := validRawVerdict("internal/example.go")
	reviewVerdict.Findings[0].Line = 2
	if err := ValidateWithLocations(reviewVerdict, map[string]struct{}{"internal/example.go": {}}, map[string]int{"internal/example.go": 1}, nil); err == nil || !strings.Contains(err.Error(), "ROAST-VERDICT-LINE") {
		t.Fatalf("line error = %v", err)
	}
}

func TestValidateAllowsOldSideLineForChangedFile(t *testing.T) {
	reviewVerdict := validRawVerdict("internal/example.go")
	reviewVerdict.Findings[0].Line = 200
	ranges := map[string][]LineRange{"internal/example.go": {{Start: 200, End: 200}}}
	if err := ValidateWithLocations(reviewVerdict, map[string]struct{}{"internal/example.go": {}}, map[string]int{"internal/example.go": 20}, ranges); err != nil {
		t.Fatalf("old-side line rejected: %v", err)
	}
}

func TestValidateUsesDiffHunkRangesForOldSideLines(t *testing.T) {
	reviewVerdict := validRawVerdict("internal/example.go")
	reviewVerdict.Findings[0].Line = 200
	diff := []byte("diff --git a/internal/example.go b/internal/example.go\n--- a/internal/example.go\n+++ b/internal/example.go\n@@ -200,2 +1,1 @@\n")
	ranges := DiffLineRanges(diff)
	if err := ValidateWithLocations(reviewVerdict, map[string]struct{}{"internal/example.go": {}}, map[string]int{"internal/example.go": 20}, ranges); err != nil {
		t.Fatalf("diff old-side line rejected: %v", err)
	}
	reviewVerdict.Findings[0].Line = 1000
	if err := ValidateWithLocations(reviewVerdict, map[string]struct{}{"internal/example.go": {}}, map[string]int{"internal/example.go": 20}, ranges); err == nil || !strings.Contains(err.Error(), "ROAST-VERDICT-LINE") {
		t.Fatalf("out-of-range diff line error = %v", err)
	}
}

func TestFilterRecalculatesOutcome(t *testing.T) {
	reviewVerdict := Verdict{
		Overall: OverallRaw,
		Findings: []Finding{
			{Priority: PriorityP2, File: "a.go", Line: 1, Title: "P2", Rationale: "reason"},
			{Priority: PriorityP3, File: "b.go", Line: 2, Title: "P3", Rationale: "reason"},
		},
		Provenance: validProvenance(),
	}
	filtered, err := Filter(reviewVerdict, PriorityP2)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Overall != OverallRaw || len(filtered.Findings) != 1 || filtered.Findings[0].Priority != PriorityP2 {
		t.Fatalf("filtered verdict = %#v", filtered)
	}
	filtered, err = Filter(reviewVerdict, PriorityP1)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Overall != OverallWellDone || len(filtered.Findings) != 0 {
		t.Fatalf("P1 filtered verdict = %#v", filtered)
	}
}

func TestSnapshotFilesReadsRegularEntries(t *testing.T) {
	snapshot := tarBytes(t, map[string]string{"README.md": "read me\n", "internal/example.go": "package example\n"})
	files, err := SnapshotFiles(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %#v", files)
	}
	lineCounts, err := SnapshotLineCounts(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if lineCounts["README.md"] != 1 || lineCounts["internal/example.go"] != 1 {
		t.Fatalf("line counts = %#v", lineCounts)
	}
}

func TestDiffFilesIncludesDeletedPaths(t *testing.T) {
	diff := []byte("diff --git a/deleted.go b/deleted.go\n--- a/deleted.go\n+++ /dev/null\n")
	files := DiffFiles(diff)
	if _, ok := files["deleted.go"]; !ok {
		t.Fatalf("files = %#v", files)
	}
}

func TestDiffLineRangesParsesOldAndNewHunks(t *testing.T) {
	diff := []byte("diff --git a/example.go b/example.go\n--- a/example.go\n+++ b/example.go\n@@ -10,3 +12,4 @@\n")
	ranges := DiffLineRanges(diff)
	if !lineInRanges(10, ranges["example.go"]) || !lineInRanges(15, ranges["example.go"]) {
		t.Fatalf("ranges = %#v", ranges)
	}
}

func TestDiffLineRangesParsesMultipleHunks(t *testing.T) {
	diff := []byte("diff --git a/example.go b/example.go\n--- a/example.go\n+++ b/example.go\n@@ -10,1 +10,1 @@\n@@ -200,2 +200,1 @@\n")
	ranges := DiffLineRanges(diff)
	if !lineInRanges(200, ranges["example.go"]) {
		t.Fatalf("ranges = %#v", ranges)
	}
}

func TestDiffFilesDecodesGitQuotedPaths(t *testing.T) {
	diff := []byte("diff --git \"a/name\\told.go\" \"b/name\\told.go\"\n--- \"a/name\\told.go\"\n+++ /dev/null\n")
	files := DiffFiles(diff)
	if _, ok := files["name\told.go"]; !ok {
		t.Fatalf("files = %#v", files)
	}
}

func TestDiffFilesDecodesQuotedPathEndingInBackslash(t *testing.T) {
	diff := []byte(`diff --git "a/deleted\\" "b/deleted\\"` + "\n")
	files := DiffFiles(diff)
	if _, ok := files[`deleted\`]; !ok {
		t.Fatalf("files = %#v", files)
	}
}

func TestDiffFilesDecodesIndependentlyQuotedHeaderPaths(t *testing.T) {
	diff := []byte("diff --git a/old.go \"b/new name.go\"\n")
	files := DiffFiles(diff)
	if _, ok := files["old.go"]; !ok {
		t.Fatalf("old files = %#v", files)
	}
	if _, ok := files["new name.go"]; !ok {
		t.Fatalf("new files = %#v", files)
	}
}

func TestDiffFilesDecodesUnquotedPathsWithSpaces(t *testing.T) {
	diff := []byte("diff --git a/old name.go b/old name.go\n")
	files := DiffFiles(diff)
	if _, ok := files["old name.go"]; !ok {
		t.Fatalf("files = %#v", files)
	}
}

func TestDiffFilesUsesFileHeadersForAmbiguousUnquotedPaths(t *testing.T) {
	diff := []byte("diff --git a/foo b/bar.go b/foo b/bar.go\nindex 1..2 100644\n--- a/foo b/bar.go\t\n+++ b/foo b/bar.go\t\n@@ -1 +1 @@\n")
	files := DiffFiles(diff)
	if _, ok := files["foo b/bar.go"]; !ok {
		t.Fatalf("files = %#v", files)
	}
	if _, ok := files["foo"]; ok {
		t.Fatalf("ambiguous partial path was accepted: %#v", files)
	}
}

func TestDiffFilesIgnoresHunkContentThatLooksLikeHeaders(t *testing.T) {
	diff := []byte("diff --git a/main.go b/main.go\nindex 1..2 100644\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n--- a/ghost.go\n+++ b/ghost.go\n")
	files := DiffFiles(diff)
	if _, ok := files["ghost.go"]; ok {
		t.Fatalf("hunk marker was accepted: %#v", files)
	}
}

func validRawVerdict(file string) Verdict {
	return Verdict{
		Overall: OverallRaw,
		Findings: []Finding{{
			Priority:  PriorityP2,
			File:      file,
			Line:      1,
			Title:     "A real defect",
			Rationale: "input causes the wrong result",
		}},
		Provenance: validProvenance(),
	}
}

func validProvenance() Provenance {
	return Provenance{Target: "HEAD", Branch: "main", Tree: "abc", Engine: "fake/canned", Context: "snapshot"}
}

func tarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	for name, content := range files {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestEncodeIsReadableJSON(t *testing.T) {
	encoded, err := Encode(Verdict{Overall: OverallWellDone, Findings: nil, Provenance: validProvenance()})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Verdict
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("encoded verdict is invalid JSON: %v", err)
	}
	if decoded.Findings == nil {
		t.Fatal("encoded empty findings as null")
	}
}
