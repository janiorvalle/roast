package verdict

import (
	"strings"
	"testing"
)

func TestDecodeRejectsReviewSideFieldsFromTheEngineEvenWhenZeroValued(t *testing.T) {
	for _, field := range []string{`"pre_existing": true`, `"pre_existing": false`, `"resolution": {"kind": "wrong", "reason": "no"}`, `"resolution": {}`, `"resolution": null`} {
		raw := `{"overall":"raw","findings":[{"priority":"P1","file":"a.go","line":1,"title":"t","rationale":"r",` + field + `}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
		_, err := Decode([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "ROAST-VERDICT-SCHEMA") || !strings.Contains(err.Error(), "return only priority, file, line, title, rationale, and suggestion") {
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
	provenance := Provenance{Target: "t", Branch: "b", Tree: "x", Engine: "e", Context: "c"}
	current := Verdict{Overall: OverallRaw, Findings: []Finding{{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r"}}, Provenance: provenance}
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

func TestPreExistingFindingIsAnObservationUnlessItIsP0(t *testing.T) {
	observation := Finding{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true}
	if observation.Open() || !observation.Observation() {
		t.Fatalf("a pre-existing P1 finding is an observation, never raw: %#v", observation)
	}
	severe := Finding{Priority: PriorityP0, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true}
	if !severe.Open() || severe.Observation() {
		t.Fatalf("a pre-existing P0 finding (data loss, crash, broken build, security) stays raw: %#v", severe)
	}
	resolved := Finding{Priority: PriorityP0, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true, Resolution: Resolution{Kind: ResolutionWrong, Reason: "evidence"}}
	if resolved.Open() || resolved.Observation() {
		t.Fatalf("a resolved finding is neither open nor an observation: %#v", resolved)
	}
}

func TestSettleIsWellDoneWhenOnlyObservationsRemain(t *testing.T) {
	settled := Settle(Verdict{Overall: OverallRaw, Findings: []Finding{{Priority: PriorityP1, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true}}})
	if settled.Overall != OverallWellDone {
		t.Fatalf("settled = %#v", settled)
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

func TestDiffDeletedLinesNamesOldSideLineNumbers(t *testing.T) {
	diff := "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,5 +1,4 @@\n a\n-b\n c\n-d\n+e\n f\n" +
		"diff --git a/g.go b/g.go\ndeleted file mode 100644\n--- a/g.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-x\n-y\n"
	deleted := DiffDeletedLines([]byte(diff))
	if _, ok := deleted["f.go"][2]; !ok {
		t.Fatalf("deleted = %v", deleted)
	}
	if _, ok := deleted["f.go"][4]; !ok {
		t.Fatalf("deleted = %v", deleted)
	}
	if len(deleted["f.go"]) != 2 || len(deleted["g.go"]) != 2 {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestDecodePriorRefusesToSetAsideAP0ExceptAsWrong(t *testing.T) {
	for _, kind := range []string{"pre-existing", "out-of-fence"} {
		raw := `{"overall":"well_done","findings":[{"priority":"P0","file":"a.go","line":1,"title":"t","rationale":"r","pre_existing":true,"resolution":{"kind":"` + kind + `","reason":"was there before"}}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
		_, err := DecodePrior([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "ROAST-PRIOR-VERDICT") || !strings.Contains(err.Error(), "fixed or shown wrong") {
			t.Fatalf("%s: err = %v", kind, err)
		}
	}
	raw := `{"overall":"well_done","findings":[{"priority":"P0","file":"a.go","line":1,"title":"t","rationale":"r","pre_existing":true,"resolution":{"kind":"wrong","reason":"the path is unreachable"}}],"provenance":{"target":"t","branch":"b","tree":"x","engine":"e","context":"c"}}`
	if _, err := DecodePrior([]byte(raw)); err != nil {
		t.Fatalf("wrong must still clear a P0: %v", err)
	}
}

func TestCarryResolutionsAppliesOnlyWrongToAP0(t *testing.T) {
	prior := Verdict{Findings: []Finding{{Priority: PriorityP1, File: "a.go", Title: "t", Resolution: Resolution{Kind: ResolutionOutOfFence, Reason: "task 2"}}}}
	current := Verdict{Overall: OverallRaw, Findings: []Finding{{Priority: PriorityP0, File: "a.go", Line: 1, Title: "t", Rationale: "r", PreExisting: true}}}
	if carried := CarryResolutions(current, prior); carried.Findings[0].Resolved() || carried.Overall != OverallRaw {
		t.Fatalf("a prior out-of-fence on the same title must not clear a P0: %#v", carried)
	}
}
