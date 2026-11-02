package output

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/janiorvalle/roast/internal/verdict"
)

// Print writes the human-facing gate result. Plain mode keeps the same
// information while removing the chef voice for CI logs. Open findings come
// first and set the outcome; findings resolved in a prior verdict follow in
// their own block so the agent sees the resolution was honored.
func Print(writer io.Writer, reviewVerdict verdict.Verdict, plain bool) error {
	open, resolved := splitFindings(reviewVerdict.Findings)
	if err := printOpen(writer, open, plain); err != nil {
		return err
	}
	return printResolved(writer, resolved, plain)
}

func splitFindings(findings []verdict.Finding) ([]verdict.Finding, []verdict.Finding) {
	open := make([]verdict.Finding, 0, len(findings))
	resolved := make([]verdict.Finding, 0)
	for _, finding := range findings {
		if finding.Open() {
			open = append(open, finding)
		} else {
			resolved = append(resolved, finding)
		}
	}
	return open, resolved
}

func printOpen(writer io.Writer, open []verdict.Finding, plain bool) error {
	if len(open) == 0 {
		if plain {
			_, err := fmt.Fprintln(writer, "clean: no findings")
			return err
		}
		_, err := fmt.Fprintln(writer, "well done - send it.")
		return err
	}
	if plain {
		if _, err := fmt.Fprintf(writer, "raw: %d finding(s)\n", len(open)); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(writer, "RAW. Fix %d finding(s) before serving this change:\n", len(open)); err != nil {
		return err
	}
	for _, finding := range open {
		if err := printFinding(writer, finding); err != nil {
			return err
		}
	}
	return nil
}

func printResolved(writer io.Writer, resolved []verdict.Finding, plain bool) error {
	if len(resolved) == 0 {
		return nil
	}
	if plain {
		if _, err := fmt.Fprintf(writer, "resolved: %d finding(s) from a prior verdict, not counted\n", len(resolved)); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(writer, "Resolved in a prior verdict, not counted (%d):\n", len(resolved)); err != nil {
		return err
	}
	for _, finding := range resolved {
		if err := printHeading(writer, finding); err != nil {
			return err
		}
		if err := printPreExisting(writer, finding); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "  resolved: %s: %s\n", finding.Resolution.Kind, safeDetail(finding.Resolution.Reason)); err != nil {
			return err
		}
	}
	return nil
}

func printFinding(writer io.Writer, finding verdict.Finding) error {
	if err := printHeading(writer, finding); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "  rationale: %s\n", safeDetail(finding.Rationale)); err != nil {
		return err
	}
	if suggestion := safeDetail(finding.Suggestion); suggestion != "" {
		if _, err := fmt.Fprintf(writer, "  suggestion: %s\n", suggestion); err != nil {
			return err
		}
	}
	return printPreExisting(writer, finding)
}

func printHeading(writer io.Writer, finding verdict.Finding) error {
	_, err := fmt.Fprintf(writer, "%s %s:%d: %s\n", finding.Priority, safePath(finding.File), finding.Line, safeTitle(finding.Title))
	return err
}

func printPreExisting(writer io.Writer, finding verdict.Finding) error {
	if !finding.PreExisting {
		return nil
	}
	_, err := fmt.Fprintf(writer, "  pre-existing: line %d of %s was last changed before the reviewed change\n", finding.Line, safePath(finding.File))
	return err
}

func safePath(value string) string {
	var sanitized strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) {
			fmt.Fprintf(&sanitized, "\\u%04x", character)
			continue
		}
		sanitized.WriteRune(character)
	}
	return sanitized.String()
}

func safeTitle(value string) string {
	return strings.Join(strings.Fields(safePath(value)), " ")
}

func safeDetail(value string) string {
	return strings.TrimSpace(safePath(value))
}
