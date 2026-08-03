package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/janiorvalle/roast/internal/verdict"
)

func TestPrintChefAndPlainModes(t *testing.T) {
	reviewVerdict := verdict.Verdict{Overall: verdict.OverallRaw, Findings: []verdict.Finding{{Priority: verdict.PriorityP1, File: "main.go", Line: 7, Title: "bad\nformatting", Rationale: "reason", Suggestion: "fix it"}}}
	var chef bytes.Buffer
	if err := Print(&chef, reviewVerdict, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chef.String(), "RAW.") || !strings.Contains(chef.String(), `P1 main.go:7: bad\u000aformatting`) || !strings.Contains(chef.String(), "  rationale: reason\n  suggestion: fix it") {
		t.Fatalf("chef output = %q", chef.String())
	}
	var plain bytes.Buffer
	if err := Print(&plain, reviewVerdict, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "RAW.") || !strings.Contains(plain.String(), "raw: 1 finding(s)") || !strings.Contains(plain.String(), "  rationale: reason\n  suggestion: fix it") {
		t.Fatalf("plain output = %q", plain.String())
	}
}

func TestPrintClean(t *testing.T) {
	var output bytes.Buffer
	if err := Print(&output, verdict.Verdict{Overall: verdict.OverallWellDone, Findings: []verdict.Finding{}}, false); err != nil {
		t.Fatal(err)
	}
	if output.String() != "well done - send it.\n" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestPrintEscapesControlCharacters(t *testing.T) {
	reviewVerdict := verdict.Verdict{Overall: verdict.OverallRaw, Findings: []verdict.Finding{{Priority: verdict.PriorityP1, File: "bad\x1bname.go", Line: 1, Title: "bad\abell", Rationale: "reason\nwith control", Suggestion: "fix\tthis"}}}
	var output bytes.Buffer
	if err := Print(&output, reviewVerdict, true); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(output.String(), "\x1b\a") || !strings.Contains(output.String(), `\u001b`) || !strings.Contains(output.String(), `\u0007`) || !strings.Contains(output.String(), `rationale: reason\u000awith control`) || !strings.Contains(output.String(), `suggestion: fix\u0009this`) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestPrintOmitsEmptySuggestion(t *testing.T) {
	var output bytes.Buffer
	if err := Print(&output, verdict.Verdict{Overall: verdict.OverallRaw, Findings: []verdict.Finding{{Priority: verdict.PriorityP1, File: "main.go", Line: 1, Title: "title", Rationale: "reason"}}}, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "suggestion:") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestPrintPreservesPathWhitespace(t *testing.T) {
	reviewVerdict := verdict.Verdict{Overall: verdict.OverallRaw, Findings: []verdict.Finding{{Priority: verdict.PriorityP1, File: "dir/a  b.go ", Line: 1, Title: "title", Rationale: "reason"}}}
	var output bytes.Buffer
	if err := Print(&output, reviewVerdict, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "dir/a  b.go :1") {
		t.Fatalf("output = %q", output.String())
	}
}
