package prompt

import (
	"fmt"
	"strconv"
	"strings"
)

// DiffSection is one file's part of the prompt diff, in diff order.
type DiffSection struct {
	Path string
	Text string
}

// DiffChunk is the part of one change that a single engine call reviews.
// Index is 1-based. A change that fits in one call is a single chunk.
type DiffChunk struct {
	Index       int
	Count       int
	ChangeFiles int
	Sections    []DiffSection
}

// ChunkBudget is what the prompt limit leaves for the diff. The frame is
// every prompt byte that is not diff: template, project documents, schema,
// provenance contract, and the validator retry reserve. A chunked call has a
// larger frame than a whole-diff call because its provenance names the chunk.
type ChunkBudget struct {
	MaximumBytes    int
	WholeFrameBytes int
	ChunkFrameBytes int
}

// SplitDiff packs diff sections into the fewest chunks that fit the budget,
// keeping diff order and never splitting a file. A change that fits in one
// call comes back as one chunk whose PromptDiff is the sections verbatim.
func SplitDiff(sections []DiffSection, budget ChunkBudget) ([]DiffChunk, error) {
	if len(sections) == 0 {
		return nil, fmt.Errorf("[ROAST-PROMPT-CHUNK] the review diff has no file sections; rebuild the review bundle and retry")
	}
	totalBytes := 0
	for _, section := range sections {
		totalBytes += len(section.Text)
	}
	if budget.WholeFrameBytes+totalBytes <= budget.MaximumBytes {
		return []DiffChunk{{Index: 1, Count: 1, ChangeFiles: len(sections), Sections: sections}}, nil
	}

	capacity := budget.MaximumBytes - budget.ChunkFrameBytes - chunkNoteAllowance(len(sections))
	for _, section := range sections {
		if len(section.Text) > capacity {
			return nil, oversizedSectionError(section, budget.WholeFrameBytes+totalBytes, capacity, budget.MaximumBytes)
		}
	}
	chunks := make([]DiffChunk, 0)
	var current []DiffSection
	currentBytes := 0
	for _, section := range sections {
		if currentBytes+len(section.Text) > capacity {
			chunks = append(chunks, DiffChunk{Sections: current})
			current = nil
			currentBytes = 0
		}
		current = append(current, section)
		currentBytes += len(section.Text)
	}
	chunks = append(chunks, DiffChunk{Sections: current})
	for index := range chunks {
		chunks[index].Index = index + 1
		chunks[index].Count = len(chunks)
		chunks[index].ChangeFiles = len(sections)
	}
	return chunks, nil
}

func oversizedSectionError(section DiffSection, assembledBytes, capacity, maximumBytes int) error {
	return fmt.Errorf("[ROAST-PROMPT-SIZE] assembled review prompt is %d bytes, above the %d-byte limit, and the change to %q alone is %d diff bytes, more than the %d bytes one chunk can hold after the prompt frame; split that file's change into its own commit and review the rest, or set ROAST_MAX_PROMPT_BYTES to a larger limit supported by the selected engine; no review engine was called", assembledBytes, maximumBytes, section.Path, len(section.Text), capacity)
}

// DiffBytes is the size of the diff text in this chunk, without the chunk note.
func (chunk DiffChunk) DiffBytes() int {
	total := 0
	for _, section := range chunk.Sections {
		total += len(section.Text)
	}
	return total
}

// Contributions lists the chunk's sections by path and size for size errors.
func (chunk DiffChunk) Contributions() []DiffContribution {
	contributions := make([]DiffContribution, 0, len(chunk.Sections))
	for _, section := range chunk.Sections {
		contributions = append(contributions, DiffContribution{Path: section.Path, Bytes: len(section.Text)})
	}
	return contributions
}

// PromptDiff is the text that fills the diff placeholder: the sections
// verbatim, preceded by a note only when the change was split.
func (chunk DiffChunk) PromptDiff() string {
	var text strings.Builder
	if chunk.Count > 1 {
		text.WriteString(chunkNote(chunk.Index, chunk.Count, len(chunk.Sections), chunk.ChangeFiles))
	}
	for _, section := range chunk.Sections {
		text.WriteString(section.Text)
	}
	return text.String()
}

// Label names this chunk in provenance, so a chunk verdict says which part
// of the change it reviewed.
func (chunk DiffChunk) Label() string {
	return "chunk=" + strconv.Itoa(chunk.Index) + "/" + strconv.Itoa(chunk.Count)
}

// ChunkSummary says how many chunks a review ran and how many diff bytes
// each carried, for the merged provenance and the run output.
func ChunkSummary(chunks []DiffChunk) string {
	sizes := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		sizes = append(sizes, strconv.Itoa(chunk.DiffBytes()))
	}
	return fmt.Sprintf("%d [%s diff bytes]", len(chunks), strings.Join(sizes, ", "))
}

func chunkNote(index, count, files, changeFiles int) string {
	return fmt.Sprintf("Review chunk %d of %d: this diff holds %d of the change's %d files. The other chunks hold the rest, and the snapshot already contains every file as it is after the whole change.\n\n", index, count, files, changeFiles)
}

// chunkNoteAllowance is the longest note a split of this many sections can
// produce, so packing never overflows the limit once the note is added.
func chunkNoteAllowance(sections int) int {
	return len(chunkNote(sections, sections, sections, sections))
}
