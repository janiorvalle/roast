package roast

import _ "embed"

//go:embed skills/roast/SKILL.md
var skillInstructions []byte

// SkillInstructions returns the skill shipped with the roast binary.
func SkillInstructions() []byte {
	return append([]byte(nil), skillInstructions...)
}
