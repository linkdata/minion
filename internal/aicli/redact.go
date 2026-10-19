package aicli

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
)

// RedactText replaces literal, Go-quoted and JSON-encoded secret content.
func RedactText(text, secret string) string {
	return redactText(text, secret, false)
}

func redactText(text, secret string, truncated bool) string {
	if secret == "" {
		return text
	}
	quoted := strconv.Quote(secret)
	encoded, _ := json.Marshal(secret)
	jsonContent := string(encoded[1 : len(encoded)-1])
	forms := [4]string{jsonContent, javascriptJSONContent(jsonContent), quoted[1 : len(quoted)-1], secret}
	var replacements [8]string
	longest := 0
	for i, form := range forms {
		mask := "[redacted]"
		if truncated {
			mask = strings.Repeat("\x00", len(form))
		}
		replacements[2*i], replacements[2*i+1] = form, mask
		longest = max(longest, len(form))
	}
	// Encoded forms precede literals; replacement text is not rescanned.
	text = strings.NewReplacer(replacements[:]...).Replace(text)
	if truncated {
		// Equal-length masks keep complete keys hidden while the cut removes partial ones.
		discard := longest - 1
		if discard >= len(text) {
			return ""
		}
		return strings.ReplaceAll(text[discard:], "\x00", "[redacted]")
	}
	return text
}

// javascriptJSONContent removes escapes Go adds to otherwise valid JSON strings.
func javascriptJSONContent(encoded string) string {
	var result strings.Builder
	result.Grow(len(encoded))
	for i := 0; i < len(encoded); {
		if encoded[i] == '\\' && i+1 < len(encoded) {
			if encoded[i+1] == '\\' {
				result.WriteString(encoded[i : i+2])
				i += 2
				continue
			}
			if encoded[i+1] == 'u' && i+6 <= len(encoded) {
				switch encoded[i+2 : i+6] {
				case "003c":
					result.WriteByte('<')
				case "003e":
					result.WriteByte('>')
				case "0026":
					result.WriteByte('&')
				case "2028":
					result.WriteRune('\u2028')
				case "2029":
					result.WriteRune('\u2029')
				default:
					result.WriteString(encoded[i : i+6])
				}
				i += 6
				continue
			}
		}
		result.WriteByte(encoded[i])
		i++
	}
	return result.String()
}

// RedactTail keeps useful trailing diagnostics without exposing a key cut at the tail boundary.
func RedactTail(tail *bounded.Tail, secret string) string {
	text := strings.TrimSpace(redactText(tail.String(), secret, tail.Truncated))
	if len(text) > bounded.DiagnosticBytes {
		text = text[len(text)-bounded.DiagnosticBytes:]
	}
	return text
}

// RedactError redacts secret content in an error's displayed message.
//
// The wrapped cause remains unchanged for error matching. A nil error or empty
// secret leaves err unchanged.
func RedactError(err error, secret string) error {
	if err != nil && secret != "" {
		message := err.Error()
		if redacted := RedactText(message, secret); redacted != message {
			return &errRedacted{cause: err, message: redacted}
		}
	}
	return err
}

type errRedacted struct {
	cause   error
	message string
}

func (e *errRedacted) Error() string { return e.message }

func (e *errRedacted) Unwrap() error { return e.cause }
