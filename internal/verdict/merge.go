package verdict

import "sort"

// Merge folds the verdicts of a chunked review into one verdict for the whole
// change: raw when any chunk is raw, every chunk's findings kept and ordered
// most severe first, and the provenance the caller gives for the whole change.
func Merge(chunks []Verdict, provenance Provenance) Verdict {
	merged := Verdict{Overall: OverallWellDone, Findings: []Finding{}, Provenance: provenance}
	for _, chunk := range chunks {
		merged.Findings = append(merged.Findings, chunk.Findings...)
		if chunk.Overall == OverallRaw {
			merged.Overall = OverallRaw
		}
	}
	sort.SliceStable(merged.Findings, func(i, j int) bool {
		return merged.Findings[i].Priority.rank() < merged.Findings[j].Priority.rank()
	})
	return merged
}
