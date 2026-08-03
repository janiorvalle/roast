package prompt

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

func TestAssembleSubstitutesEvidenceAndRejectsUnknownMarkers(t *testing.T) {
	assembled, err := Assemble(Data{
		Template:           "docs={{CONTEXT_DOCS}} schema={{VERDICT_SCHEMA}} priorities={{INCLUDED_PRIORITIES}} target={{TARGET}} branch={{BRANCH}} extra={{EXTRA_PROMPT}} diff={{DIFF}}",
		ContextDocuments:   []Document{{Path: "PROJECT.md", Content: "policy"}},
		VerdictSchema:      "schema",
		IncludedPriorities: "P0, P1, P2",
		Target:             "main...HEAD",
		Branch:             "feature",
		ExtraPrompt:        "look twice",
		Diff:               "diff --git a/a.go b/a.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"PROJECT.md", "policy", "schema", "P0, P1, P2", "main...HEAD", "feature", "look twice", "diff --git"} {
		if !strings.Contains(assembled, expected) {
			t.Fatalf("assembled prompt missing %q: %s", expected, assembled)
		}
	}
	if strings.Contains(assembled, "{{CONTEXT_DOCS}}") {
		t.Fatalf("placeholder remained: %s", assembled)
	}
	assembled, err = Assemble(Data{
		Template:           "{{CONTEXT_DOCS}} {{VERDICT_SCHEMA}} {{INCLUDED_PRIORITIES}} {{TARGET}} {{BRANCH}} {{EXTRA_PROMPT}} {{DIFF}}",
		ContextDocuments:   []Document{{Path: "PROJECT.md", Content: "literal {{UNKNOWN}}"}},
		VerdictSchema:      "schema",
		IncludedPriorities: "P0",
		Target:             "target",
		Branch:             "branch",
		ExtraPrompt:        "extra",
		Diff:               "diff {{UNKNOWN}}",
	})
	if err != nil || !strings.Contains(assembled, "literal {{UNKNOWN}}") || !strings.Contains(assembled, "diff {{UNKNOWN}}") {
		t.Fatalf("content marker was rejected or changed: %q, error = %v", assembled, err)
	}
	if _, err := Assemble(Data{Template: "{{CONTEXT_DOCS}} {{VERDICT_SCHEMA}} {{INCLUDED_PRIORITIES}} {{TARGET}} {{BRANCH}} {{EXTRA_PROMPT}} {{DIFF}} {{UNKNOWN}}"}); err == nil || !strings.Contains(err.Error(), "unresolved placeholder") {
		t.Fatalf("unknown marker error = %v", err)
	}
}

func TestContextDocumentsSelectsNamedAndGlobFiles(t *testing.T) {
	snapshot := makeSnapshot(t, map[string]string{
		"PROJECT.md":      "project\n",
		"notes.md":        "- [x] checked\n",
		"prompt.md":       "template\n",
		"src/code.go":     "package src\n",
		"docs/custom.txt": "custom\n",
	})
	selection, err := ContextDocuments(snapshot, "")
	if err != nil {
		t.Fatal(err)
	}
	documents := selection.Documents
	if len(documents) != 1 || documents[0].Path != "PROJECT.md" {
		t.Fatalf("documents = %#v", documents)
	}
	selection, err = ContextDocuments(snapshot, "docs/*")
	if err != nil {
		t.Fatal(err)
	}
	documents = selection.Documents
	if len(documents) != 2 || documents[0].Path != "PROJECT.md" || documents[1].Path != "docs/custom.txt" {
		t.Fatalf("glob documents = %#v", documents)
	}
	if _, err := ContextDocuments(snapshot, "docs/["); err == nil || !strings.Contains(err.Error(), "ROAST-PROMPT-CONTEXT") {
		t.Fatalf("invalid glob error = %v", err)
	}
}

func TestContextDocumentsCapsAndReportsExclusions(t *testing.T) {
	tooLarge := "too large\n" + strings.Repeat("x", maxContextDocumentBytes)
	selection, err := ContextDocuments(makeSnapshot(t, map[string]string{
		"AGENTS.md":         "keep\n",
		"docs/too-large.md": tooLarge,
	}), "docs/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Documents) != 1 || selection.Documents[0].Path != "AGENTS.md" {
		t.Fatalf("per-document selection = %#v", selection.Documents)
	}
	if len(selection.ExcludedDocuments) != 1 || selection.ExcludedDocuments[0].Path != "docs/too-large.md" || !strings.Contains(selection.ExcludedDocuments[0].Reason, "per-document limit") {
		t.Fatalf("per-document exclusions = %#v", selection.ExcludedDocuments)
	}
	if notice := selection.ExclusionNotice(); !strings.Contains(notice, "docs/too-large.md") || !strings.Contains(notice, "per-document limit") {
		t.Fatalf("per-document notice = %q", notice)
	}

	largeButAllowed := strings.Repeat("y", 60*1024)
	totalSelection, err := ContextDocuments(makeSnapshot(t, map[string]string{
		"AGENTS.md":       largeButAllowed,
		"CLAUDE.md":       largeButAllowed,
		"CONTRIBUTING.md": largeButAllowed,
		"PROJECT.md":      largeButAllowed,
		"README.md":       largeButAllowed,
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(totalSelection.Documents) != 4 || totalSelection.Documents[3].Path != "PROJECT.md" {
		t.Fatalf("total selection = %#v", totalSelection.Documents)
	}
	if renderedBytes := len(renderContextDocuments(totalSelection.Documents)); renderedBytes > maxContextTotalBytes {
		t.Fatalf("rendered context bytes = %d, want at most %d", renderedBytes, maxContextTotalBytes)
	}
	if len(totalSelection.ExcludedDocuments) != 1 || totalSelection.ExcludedDocuments[0].Path != "README.md" || !strings.Contains(totalSelection.ExcludedDocuments[0].Reason, "total limit") {
		t.Fatalf("total exclusions = %#v", totalSelection.ExcludedDocuments)
	}
	if notice := totalSelection.ExclusionNotice(); !strings.Contains(notice, "README.md") || !strings.Contains(notice, "total limit") {
		t.Fatalf("total notice = %q", notice)
	}
}

func TestContextExclusionNoticeQuotesRepositoryPaths(t *testing.T) {
	notice := ContextSelection{ExcludedDocuments: []ContextDocumentExclusion{{Path: "docs/bad\nname.md", Reason: "test reason"}}}.ExclusionNotice()
	if strings.Contains(notice, "docs/bad\nname.md") || !strings.Contains(notice, `"docs/bad\nname.md"`) {
		t.Fatalf("notice = %q", notice)
	}
}

func makeSnapshot(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	if err := archive.WriteHeader(&tar.Header{Name: "docs/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
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
