package aicli

import (
	"errors"
	"fmt"
	"strconv"
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

func TestRedactErrorKnownSecretEncodings(t *testing.T) {
	for _, tc := range []struct{ name, secret, encoded string }{
		{"literal quote", `fixture"secret`, `fixture"secret`},
		{"quoted quote", `fixture"secret`, `fixture\"secret`},
		{"literal backslash", `fixture\secret`, `fixture\secret`},
		{"quoted backslash", `fixture\secret`, `fixture\\secret`},
		{"Go control escape", "fixture\x1bsecret", `fixture\x1bsecret`},
		{"JSON control escape", "fixture\x1bsecret", `fixture\u001bsecret`},
		{"JSON HTML escape", "fixture<secret", `fixture\u003csecret`},
		{"replacement not rescanned", "redacted", "redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := errors.New("provider: " + tc.encoded)
			redacted := RedactError(cause, tc.secret)
			if redacted.Error() != "provider: [redacted]" || !errors.Is(redacted, cause) {
				t.Fatalf("redaction changed the message or cause: %v", redacted)
			}
		})
	}
	cause := &strconv.NumError{Func: "ParseInt", Num: `fixture"secret`, Err: strconv.ErrSyntax}
	redacted := RedactError(cause, cause.Num)
	var typed *strconv.NumError
	if redacted.Error() != `strconv.ParseInt: parsing "[redacted]": invalid syntax` || !errors.As(redacted, &typed) || typed != cause || !errors.Is(redacted, strconv.ErrSyntax) {
		t.Fatalf("quoted redaction lost the typed cause: %v", redacted)
	}
}
