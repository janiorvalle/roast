package roast

import _ "embed"

//go:embed prompt.md
var promptTemplate []byte

// DefaultPromptTemplate returns the review prompt shipped with roast.
func DefaultPromptTemplate() string {
	return string(promptTemplate)
}
