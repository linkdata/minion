package aicli

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
)

// RedactText replaces literal, Go-quoted and JSON-encoded secret content.
func RedactText(text, secret string) string {
	redacted, _ := redactText(text, secret)
	return redacted
}

func redactText(text, secret string) (string, int) {
	if secret == "" {
		return text, 0
	}
	quoted := strconv.Quote(secret)
	encoded, _ := json.Marshal(secret)
	forms := [3]string{string(encoded[1 : len(encoded)-1]), quoted[1 : len(quoted)-1], secret}
	var replacements [6]string
	longest := 0
	for i, form := range forms {
		replacements[2*i], replacements[2*i+1] = form, "[redacted]"
		longest = max(longest, len(form))
	}
	// Encoded forms precede literals; replacement text is not rescanned.
	return strings.NewReplacer(replacements[:]...).Replace(text), longest
}

// RedactTail keeps useful trailing diagnostics without exposing a key cut at the tail boundary.
func RedactTail(tail *bounded.Tail, secret string) string {
	text, longest := redactText(strings.TrimSpace(tail.String()), secret)
	if secret == "" || !tail.Truncated {
		return text
	}
	// Complete matches are gone; discard any encoded-key suffix at the cut.
	discard := longest - 1
	if discard >= len(text) {
		return ""
	}
	return strings.TrimSpace(text[discard:])
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
