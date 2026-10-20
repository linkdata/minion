package aicli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/bounded"
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

func TestRedactJavaScriptJSONForms(t *testing.T) {
	for _, tc := range []struct{ name, secret, encoded string }{
		{"HTML and control", "<\x1bZ", "<\\u001bZ"},
		{"line separator and control", "\u2028\x1bZ", "\u2028\\u001bZ"},
		{"paragraph separator and control", "\u2029\x1bZ", "\u2029\\u001bZ"},
		{"literal backslash-u and HTML", "\\u003c<\x1bZ", "\\\\u003c<\\u001bZ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "prefix " + tc.encoded + " suffix"
			if got := RedactText(text, tc.secret); got != "prefix [redacted] suffix" {
				t.Fatalf("JavaScript JSON key survived text redaction: %q", got)
			}
			var tail bounded.Tail
			if _, err := tail.Write([]byte(text)); err != nil || tail.Truncated {
				t.Fatalf("unexpected diagnostic tail state: %v, %t", err, tail.Truncated)
			}
			if got := RedactTail(&tail, tc.secret); got != "prefix [redacted] suffix" {
				t.Fatalf("JavaScript JSON key survived diagnostic redaction: %q", got)
			}
		})
	}
}

func TestRedactTailCutsPartialKnownEncodings(t *testing.T) {
	secret := "AB<\vCD"
	quoted := strconv.Quote(secret)
	encoded, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	forms := []struct{ name, text string }{
		{"literal", secret},
		{"Go quoted", quoted[1 : len(quoted)-1]},
		{"JSON encoded", string(encoded[1 : len(encoded)-1])},
		{"JavaScript JSON", "AB<\\u000bCD"},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			const note = "rate limit reached"
			fragment := form.text[1:]
			fill := strings.Repeat("x", bounded.DiagnosticBytes-len(fragment)-len(note))
			var tail bounded.Tail
			if _, err := tail.Write([]byte(form.text + fill + note)); err != nil || !tail.Truncated {
				t.Fatalf("expected truncated tail: %v, %t", err, tail.Truncated)
			}
			if got := RedactTail(&tail, secret); strings.TrimLeft(got, "x") != note {
				t.Fatalf("partial %s key survived: %q", form.name, got)
			}
			if got := RedactTail(&tail, ""); got != tail.String() {
				t.Fatalf("empty key altered tail: %q", got)
			}
		})
	}
	var whole bounded.Tail
	if _, err := whole.Write([]byte("prefix " + forms[2].text + " suffix")); err != nil || whole.Truncated {
		t.Fatalf("unexpected whole-tail truncation: %v, %t", err, whole.Truncated)
	}
	if got := RedactTail(&whole, secret); got != "prefix [redacted] suffix" {
		t.Fatalf("nontruncated key survived: %q", got)
	}
}

func TestRedactTailCutsEscapedBackslashSecret(t *testing.T) {
	const secret = `\\`
	const encoded = `\\\\`
	const note = "rate limit reached"
	fragment := encoded[1:]
	fill := strings.Repeat("x", bounded.DiagnosticBytes-len(fragment)-len(note))
	var tail bounded.Tail
	if _, err := tail.Write([]byte(encoded + fill + note)); err != nil || !tail.Truncated {
		t.Fatalf("expected truncated escaped key: %v, %t", err, tail.Truncated)
	}
	if got := RedactTail(&tail, secret); strings.TrimLeft(got, "x") != note {
		t.Fatalf("partial escaped key survived or later diagnostic lost: %q", got)
	}
}

func TestRedactTailMasksCompleteKeyAcrossCut(t *testing.T) {
	const secret = "ABC"
	const note = "rate limit reached"
	fill := strings.Repeat("x", bounded.DiagnosticBytes-len("x"+secret)-len(note))
	var tail bounded.Tail
	if _, err := tail.Write([]byte("zx" + secret + fill + note)); err != nil || !tail.Truncated {
		t.Fatalf("expected truncated tail: %v, %t", err, tail.Truncated)
	}
	got := RedactTail(&tail, secret)
	if want := fill + note; got != want {
		t.Fatalf("complete key crossed cut or diagnostic changed: prefix %q, length %d; want length %d", got[:min(20, len(got))], len(got), len(want))
	}
}

func TestRedactTailKeepsDiagnosticsBeforeAdjacentKeys(t *testing.T) {
	const secret = "long-key-secret"
	const note = "rate limit reached"
	var tail bounded.Tail
	if _, err := tail.Write([]byte(strings.Repeat("x", bounded.DiagnosticBytes) + note + strings.Repeat(secret, 100))); err != nil || !tail.Truncated {
		t.Fatalf("expected truncated tail: %v, %t", err, tail.Truncated)
	}
	got := RedactTail(&tail, secret)
	if !strings.HasSuffix(got, note+"[redacted]") || len(got) > bounded.DiagnosticBytes {
		t.Fatalf("diagnostic lost to mask expansion: %q", got)
	}
}

func TestRedactTailNormalizesLiteralNULBeforeMasking(t *testing.T) {
	const secret = "a\x00b"
	var tail bounded.Tail
	if _, err := tail.Write([]byte(strings.Repeat("x", bounded.DiagnosticBytes) + "diagnostic\x00" + secret + " kept")); err != nil || !tail.Truncated {
		t.Fatalf("expected truncated tail: %v, %t", err, tail.Truncated)
	}
	if got := RedactTail(&tail, secret); !strings.HasSuffix(got, "diagnostic?[redacted] kept") || strings.ContainsRune(got, '\x00') {
		t.Fatalf("literal NUL hid the key or was mistaken for a mask: %q", got)
	}
}
