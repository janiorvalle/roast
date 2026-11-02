package verdict

import (
	"fmt"
	"strings"
)

// ResolutionKind is why the agent set a finding aside: the line predates the
// change, the finding belongs to another task, or the finding is wrong.
type ResolutionKind string

const (
	ResolutionPreExisting ResolutionKind = "pre-existing"
	ResolutionOutOfFence  ResolutionKind = "out-of-fence"
	ResolutionWrong       ResolutionKind = "wrong"
)

func (kind ResolutionKind) Valid() bool {
	switch kind {
	case ResolutionPreExisting, ResolutionOutOfFence, ResolutionWrong:
		return true
	default:
		return false
	}
}

// Resolution is the agent's annotation on a finding in a verdict passed back
// with --prior-verdict. A zero Resolution means the finding is open.
type Resolution struct {
	Kind   ResolutionKind `json:"kind"`
	Reason string         `json:"reason"`
}

func (resolution Resolution) IsZero() bool {
	return resolution == Resolution{}
}

// Resolved reports whether a prior verdict's annotation was honored for this
// finding.
func (finding Finding) Resolved() bool {
	return !finding.Resolution.IsZero()
}

// Open reports whether a finding still gates the change. By the human's
// ruling a pre-existing finding is an observation and never raw, except data
// loss, a crash, a broken build or install, or a security exposure, which stay
// raw. P0 is the prompt's class for those four plus a change that does not do
// what it claims, which is the change's own defect wherever blame puts it, so
// P0 gates whatever the tag says.
func (finding Finding) Open() bool {
	return !finding.Resolved() && (!finding.PreExisting || finding.Priority == PriorityP0)
}

// Observation reports whether a finding is reported only for the agent's
// information: pre-existing, below P0, and not resolved.
func (finding Finding) Observation() bool {
	return !finding.Resolved() && !finding.Open()
}

// DecodePrior parses a verdict roast wrote earlier, annotated by the agent
// with a resolution per finding it set aside. Review-side fields are allowed
// here and nowhere else.
func DecodePrior(raw []byte) (Verdict, error) {
	envelope, err := decodeEnvelope(raw, "prior verdict")
	if err != nil {
		return Verdict{}, fmt.Errorf("[ROAST-PRIOR-VERDICT] %w; pass the JSON roast wrote with --json-output, with a resolution per finding you set aside", err)
	}
	prior := Verdict{Overall: envelope.overall, Findings: make([]Finding, 0, len(envelope.findings)), Provenance: envelope.provenance}
	for index, rawFinding := range envelope.findings {
		var finding Finding
		if err := decodeClosed(rawFinding, &finding); err != nil {
			return Verdict{}, fmt.Errorf("[ROAST-PRIOR-VERDICT] finding %d is not a verdict finding: %w; keep the fields roast wrote and add only resolution with kind and reason", index, err)
		}
		prior.Findings = append(prior.Findings, finding)
	}
	for index, finding := range prior.Findings {
		if finding.Resolution.IsZero() {
			continue
		}
		if !finding.Resolution.Kind.Valid() {
			return Verdict{}, fmt.Errorf("[ROAST-PRIOR-VERDICT] finding %d has resolution kind %q; use pre-existing, out-of-fence, or wrong", index, finding.Resolution.Kind)
		}
		if strings.TrimSpace(finding.Resolution.Reason) == "" {
			return Verdict{}, fmt.Errorf("[ROAST-PRIOR-VERDICT] finding %d has a resolution with an empty reason; write one sentence saying why it is pre-existing, out-of-fence, or wrong", index)
		}
	}
	if err := ValidateWithLocations(prior, nil, nil, nil); err != nil {
		return Verdict{}, fmt.Errorf("[ROAST-PRIOR-VERDICT] %w", err)
	}
	return prior, nil
}

// CarryResolutions copies a resolution from the first prior verdict holding a
// resolved finding with the same file and title onto each current finding,
// then settles the outcome from the findings still open. Matching ignores
// case, whitespace, and trailing punctuation in the title, and ignores the
// line, since fixes move lines between rounds. A reworded title does not
// match and comes back open, so the agent resolves it once more.
func CarryResolutions(current Verdict, prior ...Verdict) Verdict {
	findings := make([]Finding, 0, len(current.Findings))
	for _, finding := range current.Findings {
		if resolution, ok := priorResolution(finding, prior); ok {
			finding.Resolution = resolution
		}
		findings = append(findings, finding)
	}
	current.Findings = findings
	return Settle(current)
}

func priorResolution(finding Finding, prior []Verdict) (Resolution, bool) {
	for _, priorVerdict := range prior {
		for _, candidate := range priorVerdict.Findings {
			if candidate.Resolved() && candidate.File == finding.File && titleKey(candidate.Title) == titleKey(finding.Title) {
				return candidate.Resolution, true
			}
		}
	}
	return Resolution{}, false
}

func titleKey(title string) string {
	collapsed := strings.Join(strings.Fields(strings.ToLower(title)), " ")
	return strings.TrimRight(collapsed, ".!")
}

// Settle sets the outcome from the findings that are still open: raw when
// any remains, well done otherwise.
func Settle(reviewVerdict Verdict) Verdict {
	reviewVerdict.Overall = OverallWellDone
	if openFindings(reviewVerdict.Findings) > 0 {
		reviewVerdict.Overall = OverallRaw
	}
	return reviewVerdict
}

func openFindings(findings []Finding) int {
	count := 0
	for _, finding := range findings {
		if finding.Open() {
			count++
		}
	}
	return count
}
