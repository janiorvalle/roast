package output

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/janiorvalle/roast/internal/verdict"
)

// Print writes the human-facing gate result. Plain mode keeps the same
// information while removing the chef voice for CI logs.
func Print(writer io.Writer, reviewVerdict verdict.Verdict, plain bool) error {
	if len(reviewVerdict.Findings) == 0 {
		if plain {
			_, err := fmt.Fprintln(writer, "clean: no findings")
			return err
		}
		_, err := fmt.Fprintln(writer, "well done - send it.")
		return err
	}

	if plain {
		if _, err := fmt.Fprintf(writer, "raw: %d finding(s)\n", len(reviewVerdict.Findings)); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(writer, "RAW. Fix %d finding(s) before serving this change:\n", len(reviewVerdict.Findings)); err != nil {
		return err
	}
	for _, finding := range reviewVerdict.Findings {
		if _, err := fmt.Fprintf(writer, "%s %s:%d: %s\n", finding.Priority, safePath(finding.File), finding.Line, safeTitle(finding.Title)); err != nil {
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
	}
	return nil
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
