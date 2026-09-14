package prompt

import (
	"strconv"
	"strings"
	"testing"
)

func chunkTestSections() []DiffSection {
	return []DiffSection{
		{Path: "a.go", Text: "diff --git a/a.go b/a.go\n" + strings.Repeat("+a\n", 100)},
		{Path: "b.go", Text: "diff --git a/b.go b/b.go\n" + strings.Repeat("+b\n", 100)},
		{Path: "c.go", Text: "diff --git a/c.go b/c.go\n" + strings.Repeat("+c\n", 100)},
	}
}

func joinSections(sections []DiffSection) string {
	var text strings.Builder
	for _, section := range sections {
		text.WriteString(section.Text)
	}
	return text.String()
}

func TestSplitDiffKeepsAFittingChangeInOneVerbatimChunk(t *testing.T) {
	sections := chunkTestSections()
	total := len(joinSections(sections))
	chunks, err := SplitDiff(sections, ChunkBudget{MaximumBytes: total + 100, WholeFrameBytes: 100, ChunkFrameBytes: 120})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Index != 1 || chunks[0].Count != 1 || chunks[0].ChangeFiles != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].PromptDiff() != joinSections(sections) {
		t.Fatalf("single chunk prompt diff is not the diff verbatim:\n%s", chunks[0].PromptDiff())
	}
	if summary := ChunkSummary(chunks); summary != "1 ["+strconv.Itoa(total)+" diff bytes]" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSplitDiffPacksWholeFilesInOrderUnderTheLimit(t *testing.T) {
	sections := chunkTestSections()
	sectionBytes := len(sections[0].Text)
	// Two sections fit in a chunk once the frame and the chunk note are paid for; three do not.
	maximum := 120 + chunkNoteAllowance(3) + 2*sectionBytes + 1
	chunks, err := SplitDiff(sections, ChunkBudget{MaximumBytes: maximum, WholeFrameBytes: 100, ChunkFrameBytes: 120})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2: %#v", len(chunks), chunks)
	}
	if got := chunks[0].Sections; len(got) != 2 || got[0].Path != "a.go" || got[1].Path != "b.go" {
		t.Fatalf("first chunk = %#v", got)
	}
	if got := chunks[1].Sections; len(got) != 1 || got[0].Path != "c.go" {
		t.Fatalf("second chunk = %#v", got)
	}
	for index, chunk := range chunks {
		if chunk.Index != index+1 || chunk.Count != 2 || chunk.ChangeFiles != 3 {
			t.Fatalf("chunk %d numbering = %#v", index, chunk)
		}
		if len(chunk.PromptDiff())+120 > maximum {
			t.Fatalf("chunk %d prompt diff is %d bytes, over the %d-byte budget", chunk.Index, len(chunk.PromptDiff()), maximum-120)
		}
		if !strings.HasPrefix(chunk.PromptDiff(), "Review chunk "+strconv.Itoa(chunk.Index)+" of 2: ") || !strings.HasSuffix(chunk.PromptDiff(), joinSections(chunk.Sections)) {
			t.Fatalf("chunk %d prompt diff = %q", chunk.Index, chunk.PromptDiff())
		}
	}
	if chunks[0].Label() != "chunk=1/2" || chunks[1].Label() != "chunk=2/2" {
		t.Fatalf("labels = %q, %q", chunks[0].Label(), chunks[1].Label())
	}
	if summary := ChunkSummary(chunks); summary != "2 ["+strconv.Itoa(2*sectionBytes)+", "+strconv.Itoa(sectionBytes)+" diff bytes]" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSplitDiffNamesTheFileThatCannotFitInAnyChunk(t *testing.T) {
	sections := chunkTestSections()
	sections[1].Text += strings.Repeat("+big\n", 200)
	sectionBytes := len(sections[0].Text)
	maximum := 120 + chunkNoteAllowance(3) + 2*sectionBytes + 1
	_, err := SplitDiff(sections, ChunkBudget{MaximumBytes: maximum, WholeFrameBytes: 100, ChunkFrameBytes: 120})
	if err == nil {
		t.Fatal("expected an error for the oversized file")
	}
	total := len(joinSections(sections))
	for _, expected := range []string{"ROAST-PROMPT-SIZE", "assembled review prompt is " + strconv.Itoa(100+total) + " bytes", `"b.go"`, strconv.Itoa(len(sections[1].Text)) + " diff bytes", strconv.Itoa(maximum) + "-byte limit", "own commit", "ROAST_MAX_PROMPT_BYTES", "no review engine was called"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error = %q, missing %q", err.Error(), expected)
		}
	}
}

func TestSplitDiffRejectsAnEmptyDiff(t *testing.T) {
	if _, err := SplitDiff(nil, ChunkBudget{MaximumBytes: 10}); err == nil || !strings.Contains(err.Error(), "ROAST-PROMPT-CHUNK") {
		t.Fatalf("error = %v", err)
	}
}

func TestChunkContributionsReportEachSection(t *testing.T) {
	chunk := DiffChunk{Index: 1, Count: 1, Sections: chunkTestSections()}
	contributions := chunk.Contributions()
	if len(contributions) != 3 || contributions[2].Path != "c.go" || contributions[2].Bytes != len(chunk.Sections[2].Text) {
		t.Fatalf("contributions = %#v", contributions)
	}
	if chunk.DiffBytes() != len(joinSections(chunk.Sections)) {
		t.Fatalf("diff bytes = %d", chunk.DiffBytes())
	}
}
