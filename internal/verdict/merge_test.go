package verdict

import "testing"

func TestMergeIsRawWhenAnyChunkIsRawAndOrdersFindingsBySeverity(t *testing.T) {
	provenance := Provenance{Target: "HEAD", Branch: "main", Tree: "tree", Engine: "engine", Context: "context; chunks=3 [10, 20, 30 diff bytes]"}
	chunks := []Verdict{
		{Overall: OverallWellDone, Findings: []Finding{}},
		{Overall: OverallRaw, Findings: []Finding{
			{Priority: PriorityP3, File: "b.go", Line: 2, Title: "polish", Rationale: "naming"},
			{Priority: PriorityP1, File: "b.go", Line: 9, Title: "logic", Rationale: "wrong branch"},
		}},
		{Overall: OverallRaw, Findings: []Finding{
			{Priority: PriorityP0, File: "c.go", Line: 1, Title: "crash", Rationale: "nil deref"},
			{Priority: PriorityP1, File: "c.go", Line: 5, Title: "race", Rationale: "unguarded map"},
		}},
	}
	merged := Merge(chunks, provenance)
	if merged.Overall != OverallRaw || merged.Provenance != provenance {
		t.Fatalf("merged = %#v", merged)
	}
	titles := make([]string, 0, len(merged.Findings))
	for _, finding := range merged.Findings {
		titles = append(titles, finding.Title)
	}
	want := []string{"crash", "logic", "race", "polish"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	for index := range want {
		if titles[index] != want[index] {
			t.Fatalf("titles = %v, want %v", titles, want)
		}
	}
	if err := ValidateWithLocations(merged, nil, nil, nil); err != nil {
		t.Fatalf("merged verdict fails validation: %v", err)
	}
}

func TestMergeOfCleanChunksIsWellDoneWithAnEmptyFindingsArray(t *testing.T) {
	provenance := Provenance{Target: "HEAD", Branch: "main", Tree: "tree", Engine: "engine", Context: "context"}
	merged := Merge([]Verdict{{Overall: OverallWellDone, Findings: []Finding{}}, {Overall: OverallWellDone, Findings: nil}}, provenance)
	if merged.Overall != OverallWellDone || merged.Findings == nil || len(merged.Findings) != 0 {
		t.Fatalf("merged = %#v", merged)
	}
	if err := ValidateWithLocations(merged, nil, nil, nil); err != nil {
		t.Fatalf("merged verdict fails validation: %v", err)
	}
}
