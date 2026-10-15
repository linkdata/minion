package aicli

import "strings"

// RedactError replaces literal secret occurrences in an error's displayed message.
//
// The wrapped cause remains unchanged for error matching. A nil error or empty
// secret leaves err unchanged.
func RedactError(err error, secret string) error {
	if err != nil && secret != "" {
		message := err.Error()
		if redacted := strings.ReplaceAll(message, secret, "[redacted]"); redacted != message {
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
