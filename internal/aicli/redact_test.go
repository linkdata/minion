package aicli

import (
	"errors"
	"fmt"
	"testing"
)

func TestRedactError(t *testing.T) {
	cause := errors.New("dummy-secret")
	err := fmt.Errorf("CLI: %w", cause)
	redacted := RedactError(err, "dummy-secret")
	if redacted.Error() != "CLI: [redacted]" || !errors.Is(redacted, cause) {
		t.Fatalf("redaction lost message or cause: %v", redacted)
	}
	if RedactError(nil, "dummy-secret") != nil || RedactError(err, "") != err || RedactError(err, "absent") != err {
		t.Fatal("redaction changed an unrelated error")
	}
}
