package verdict

import (
	"strings"
	"testing"
)

func TestDecodeRejectsReviewSideFieldsFromTheEngine(t *testing.T) {
	for _, field := range []string{`"pre_existing": true`, `"resolution": {"kind": "wrong", "reason": "no"}`} {
		raw := `{"overall":"raw","findings":[{"priority":"P1","file":"a.go","line":1,"title":"t","rationale":"r",` + field + `}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
		_, err := Decode([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "ROAST-VERDICT-SCHEMA") || !strings.Contains(err.Error(), "review-side") {
			t.Fatalf("field %s: err = %v", field, err)
		}
	}
}

func TestDecodePriorAcceptsResolvedFindingsAndAWellDoneOverall(t *testing.T) {
	raw := `{"overall":"well_done","findings":[{"priority":"P1","file":"a.go","line":1,"title":"t","rationale":"r","pre_existing":true,"resolution":{"kind":"pre-existing","reason":"line 1 predates the task"}}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
	prior, err := DecodePrior([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Findings) != 1 || !prior.Findings[0].Resolved() || prior.Findings[0].Resolution.Kind != ResolutionPreExisting || !prior.Findings[0].PreExisting {
		t.Fatalf("prior = %#v", prior)
	}
}

func TestDecodePriorRejectsAnUnknownKindAndAnEmptyReason(t *testing.T) {
	cases := map[string]string{
		"kind":   `{"kind":"later","reason":"some day"}`,
		"reason": `{"kind":"wrong","reason":"  "}`,
	}
	for name, resolution := range cases {
		raw := `{"overall":"well_done","findings":[{"priority":"P1","file":"a.go","line":1,"title":"t","rationale":"r","resolution":` + resolution + `}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
		_, err := DecodePrior([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "ROAST-PRIOR-VERDICT") || !strings.Contains(err.Error(), "pre-existing, out-of-fence, or wrong") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestDecodePriorRejectsAWellDoneVerdictWithAnOpenFinding(t *testing.T) {
	raw := `{"overall":"well_done","findings":[{"priority":"P1","file":"a.go","line":1,"title":"t","rationale":"r"}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
	if _, err := DecodePrior([]byte(raw)); err == nil || !strings.Contains(err.Error(), "ROAST-PRIOR-VERDICT") {
		t.Fatalf("err = %v", err)
	}
}

func TestCarryResolutionsMatchesOnFileAndNormalizedTitle(t *testing.T) {
	prior := Verdict{Overall: OverallWellDone, Findings: []Finding{
		{Priority: PriorityP1, File: "a.go", Line: 3, Title: "Every user is granted admin.", Rationale: "r", Resolution: Resolution{Kind: ResolutionWrong, Reason: "IsAdmin is only reached for staff"}},
		{Priority: PriorityP1, File: "b.go", Line: 9, Title: "unresolved one", Rationale: "r"},
	}}
	current := Verdict{Overall: OverallRaw, Findings: []Finding{
		{Priority: PriorityP1, File: "a.go", Line: 7, Title: "  every USER is granted  admin", Rationale: "r2"},
		{Priority: PriorityP1, File: "b.go", Line: 9, Title: "unresolved one", Rationale: "r"},
		{Priority: PriorityP1, File: "c.go", Line: 1, Title: "Every user is granted admin.", Rationale: "same title, other file"},
	}}
	carried := CarryResolutions(current, prior)
	if !carried.Findings[0].Resolved() || carried.Findings[0].Resolution.Reason != "IsAdmin is only reached for staff" {
		t.Fatalf("matched finding = %#v", carried.Findings[0])
	}
	if carried.Findings[1].Resolved() {
		t.Fatalf("a prior finding without a resolution must not resolve anything: %#v", carried.Findings[1])
	}
	if carried.Findings[2].Resolved() {
		t.Fatalf("a different file must not match: %#v", carried.Findings[2])
	}
	if carried.Overall != OverallRaw {
		t.Fatalf("overall = %s, want raw while open findings remain", carried.Overall)
	}
}

func TestCarryResolutionsSettlesWellDoneWhenEveryFindingIsResolved(t *testing.T) {
	prior := Verdict{Findings: []Finding{{File: "a.go", Title: "t", Resolution: Resolution{Kind: ResolutionOutOfFence, Reason: "belongs to task 2"}}}}
	current := Verdict{Overall: OverallRaw, Findings: []Finding{{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r"}}}
	carried := CarryResolutions(current, prior)
	if carried.Overall != OverallWellDone || !carried.Findings[0].Resolved() {
		t.Fatalf("carried = %#v", carried)
	}
	if err := ValidateWithLocations(carried, nil, nil, nil); err != nil {
		t.Fatalf("a well done verdict with only resolved findings must validate: %v", err)
	}
}

func TestCarryResolutionsTakesTheFirstMatchAcrossSeveralPriorVerdicts(t *testing.T) {
	first := Verdict{Findings: []Finding{{File: "a.go", Title: "t", Resolution: Resolution{Kind: ResolutionWrong, Reason: "first round"}}}}
	second := Verdict{Findings: []Finding{{File: "a.go", Title: "t", Resolution: Resolution{Kind: ResolutionWrong, Reason: "second round"}}}}
	current := Verdict{Overall: OverallRaw, Findings: []Finding{{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r"}}}
	if carried := CarryResolutions(current, first, second); carried.Findings[0].Resolution.Reason != "first round" {
		t.Fatalf("carried = %#v", carried.Findings[0])
	}
}

func TestFilterKeepsResolvedFindingsOutOfTheOutcome(t *testing.T) {
	reviewVerdict := Verdict{Overall: OverallRaw, Findings: []Finding{
		{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r", Resolution: Resolution{Kind: ResolutionWrong, Reason: "evidence"}},
		{Priority: PriorityP3, File: "a.go", Line: 2, Title: "polish", Rationale: "r"},
	}}
	filtered, err := Filter(reviewVerdict, PriorityP1)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Overall != OverallWellDone || len(filtered.Findings) != 1 {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func TestPreExistingFindingStaysOpenUntilTheHumanRules(t *testing.T) {
	finding := Finding{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true}
	if !finding.Open() {
		t.Fatal("a pre-existing finding is tagged, still raw")
	}
}

func TestEncodeOmitsReviewSideFieldsUntilTheyAreSet(t *testing.T) {
	plain, err := Encode(Verdict{Overall: OverallRaw, Findings: []Finding{{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "pre_existing") || strings.Contains(string(plain), "resolution") {
		t.Fatalf("plain finding carries review-side fields: %s", plain)
	}
	tagged, err := Encode(Verdict{Overall: OverallWellDone, Findings: []Finding{{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true, Resolution: Resolution{Kind: ResolutionPreExisting, Reason: "older than the diff"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tagged), `"pre_existing": true`) || !strings.Contains(string(tagged), `"kind": "pre-existing"`) {
		t.Fatalf("tagged finding lost its fields: %s", tagged)
	}
}

func TestDiffAddedFilesNamesOnlyNewPaths(t *testing.T) {
	diff := "diff --git a/old.go b/old.go\n--- a/old.go\n+++ b/old.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+x\n"
	added := DiffAddedFiles([]byte(diff))
	if _, ok := added["new.go"]; !ok || len(added) != 1 {
		t.Fatalf("added = %v", added)
	}
}
