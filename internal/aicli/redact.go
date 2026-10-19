package aicli

import (
	"encoding/json"
	"strconv"
	"strings"
)

// RedactText replaces literal, Go-quoted and JSON-encoded secret content.
func RedactText(text, secret string) string {
	if secret == "" {
		return text
	}
	quoted := strconv.Quote(secret)
	encoded, _ := json.Marshal(secret)
	// Encoded forms precede literals; replacement text is not rescanned.
	return strings.NewReplacer(
		string(encoded[1:len(encoded)-1]), "[redacted]",
		quoted[1:len(quoted)-1], "[redacted]",
		secret, "[redacted]",
	).Replace(text)
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
