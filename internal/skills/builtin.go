package skills

import (
	"bytes"
	_ "embed"
)

// BuiltinAntiPromptInjection names Minion's embedded prompt-injection skill.
const BuiltinAntiPromptInjection = "builtin://anti-prompt-injection"

//go:embed anti-prompt-injection/SKILL.md
var antiPromptInjection []byte

// BuiltinContent returns a copy of an embedded skill's SKILL.md.
func BuiltinContent(rawURL string) (content []byte, ok bool) {
	if rawURL == BuiltinAntiPromptInjection {
		return bytes.Clone(antiPromptInjection), true
	}
	return nil, false
}
