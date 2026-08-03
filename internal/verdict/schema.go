package verdict

import "fmt"

type Overall string

const (
	OverallWellDone Overall = "well_done"
	OverallRaw      Overall = "raw"
)

type Priority string

const (
	PriorityP0 Priority = "P0"
	PriorityP1 Priority = "P1"
	PriorityP2 Priority = "P2"
	PriorityP3 Priority = "P3"
)

type Finding struct {
	Priority   Priority `json:"priority"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Title      string   `json:"title"`
	Rationale  string   `json:"rationale"`
	Suggestion string   `json:"suggestion,omitempty"`
}

type Provenance struct {
	Target  string `json:"target"`
	Branch  string `json:"branch"`
	Tree    string `json:"tree"`
	Engine  string `json:"engine"`
	Context string `json:"context"`
}

type Verdict struct {
	Overall    Overall    `json:"overall"`
	Findings   []Finding  `json:"findings"`
	Provenance Provenance `json:"provenance"`
}

// Schema returns the JSON schema shown to the review engine.
func Schema() string {
	return `{
  "type": "object",
  "additionalProperties": false,
  "required": ["overall", "findings", "provenance"],
  "properties": {
    "overall": {"type": "string", "enum": ["well_done", "raw"]},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["priority", "file", "line", "title", "rationale"],
        "properties": {
          "priority": {"type": "string", "enum": ["P0", "P1", "P2", "P3"]},
          "file": {"type": "string", "minLength": 1},
          "line": {"type": "integer", "minimum": 1},
          "title": {"type": "string", "minLength": 1},
          "rationale": {"type": "string", "minLength": 1},
          "suggestion": {"type": "string"}
        }
      }
    },
    "provenance": {
      "type": "object",
      "additionalProperties": false,
      "required": ["target", "branch", "tree", "engine", "context"],
      "properties": {
        "target": {"type": "string", "minLength": 1},
        "branch": {"type": "string", "minLength": 1},
        "tree": {"type": "string", "minLength": 1},
        "engine": {"type": "string", "minLength": 1},
        "context": {"type": "string", "minLength": 1}
      }
    }
  }
}`
}

func ParsePriority(value string) (Priority, error) {
	priority := Priority(value)
	if !priority.Valid() {
		return "", fmt.Errorf("[ROAST-PRIORITY] %q is not a valid priority; expected P0, P1, P2, or P3", value)
	}
	return priority, nil
}

func (p Priority) Valid() bool {
	switch p {
	case PriorityP0, PriorityP1, PriorityP2, PriorityP3:
		return true
	default:
		return false
	}
}

func (p Priority) rank() int {
	switch p {
	case PriorityP0:
		return 0
	case PriorityP1:
		return 1
	case PriorityP2:
		return 2
	case PriorityP3:
		return 3
	default:
		return -1
	}
}

func (p Priority) Includes(other Priority) bool {
	return p.Valid() && other.Valid() && other.rank() <= p.rank()
}

func IncludedPriorities(max Priority) string {
	priorities := []Priority{PriorityP0, PriorityP1, PriorityP2, PriorityP3}
	values := make([]string, 0, len(priorities))
	for _, priority := range priorities {
		if max.Includes(priority) {
			values = append(values, string(priority))
		}
	}
	return joinComma(values)
}

func joinComma(values []string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ", "
		}
		result += value
	}
	return result
}
