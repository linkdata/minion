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

func TestRedactMarkerCannotContainSecret(t *testing.T) {
	if got := RedactText("#", "#"); got != "[redacted]" {
		t.Fatalf("alternate marker key survived: %q", got)
	}
	for _, tc := range []struct{ name, secret string }{
		{"word", "redacted"},
		{"whole marker", "[redacted]"},
		{"letter", "a"},
		{"bracket", "["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactText("PFX "+tc.secret+" SFX", tc.secret); got != "PFX # SFX" || strings.Contains(got, tc.secret) {
				t.Fatalf("literal key survived: %q", got)
			}
			encoded, err := json.Marshal(tc.secret)
			if err != nil {
				t.Fatal(err)
			}
			if got := RedactText("PFX "+string(encoded)+" SFX", tc.secret); got != `PFX "#" SFX` || strings.Contains(got, tc.secret) {
				t.Fatalf("JSON key survived: %q", got)
			}
			cause := errors.New("CLI: " + tc.secret)
			if got := RedactError(cause, tc.secret); got.Error() != "CLI: #" || !errors.Is(got, cause) {
				t.Fatalf("error key survived or cause changed: %v", got)
			}
			var tail bounded.Tail
			if _, err := tail.Write([]byte(strings.Repeat("X", bounded.DiagnosticBytes) + "PFX " + tc.secret + " SFX")); err != nil || !tail.Truncated {
				t.Fatalf("unexpected diagnostic tail state: %v, %t", err, tail.Truncated)
			}
			got := RedactTail(&tail, tc.secret)
			if !strings.HasSuffix(got, "PFX # SFX") || strings.Contains(got, tc.secret) || len(got) > bounded.DiagnosticBytes {
				t.Fatalf("truncated diagnostic key survived or exceeded bound: %q", got)
			}
		})
	}
}

func TestRedactMarkerCannotReformSecretAtBoundary(t *testing.T) {
	for _, tc := range []struct{ secret, text, forbidden string }{
		{"]x", "]]xx", "]x"},
		{"x[", "xx[[", "x["},
		{"]#", "]]##", "]#"},
		{"]\n", `]]\n\n`, `]\n`},
	} {
		if got := RedactText(tc.text, tc.secret); strings.Contains(got, tc.forbidden) {
			t.Errorf("text still contains %q after redaction: %q", tc.forbidden, got)
		}
		cause := errors.New(tc.text)
		if got := RedactError(cause, tc.secret); strings.Contains(got.Error(), tc.forbidden) || !errors.Is(got, cause) {
			t.Errorf("error exposed %q or lost its cause: %q", tc.forbidden, got.Error())
		}
		var tail bounded.Tail
		if _, err := tail.Write([]byte(strings.Repeat("z", bounded.DiagnosticBytes) + tc.text + " diagnostic")); err != nil || !tail.Truncated {
			t.Fatalf("unexpected diagnostic tail state: %v, %t", err, tail.Truncated)
		}
		if got := RedactTail(&tail, tc.secret); strings.Contains(got, tc.forbidden) || !strings.HasSuffix(got, "diagnostic") {
			t.Errorf("tail exposed %q or lost the diagnostic: length=%d, suffix=%q", tc.forbidden, len(got), got[max(0, len(got)-64):])
		}
	}
	var printable strings.Builder
	for marker := byte('!'); marker <= '~'; marker++ {
		printable.WriteByte(marker)
	}
	secret := "]" + printable.String()
	if got := RedactText("]"+secret+printable.String(), secret); got != "" {
		t.Fatalf("diagnostic with no safe separator was not discarded: %q", got)
	}
}

func TestRedactTextKeepsJSONValidAfterReformedSecret(t *testing.T) {
	const secret = "]!"
	got := RedactText(`{"answer":"]]!!","refutations":""}`, secret)
	if !json.Valid([]byte(got)) || strings.Contains(got, secret) {
		t.Fatalf("redaction broke JSON or exposed the key: %q", got)
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

func TestRedactEquivalentJSONEscapes(t *testing.T) {
	for _, tc := range []struct{ name, secret, encoded string }{
		{"escaped slash", "a/b", `a\/b`},
		{"letter in marker", "a", `\u0061`},
		{"escaped ASCII", "a/b", `\u0061/b`},
		{"mixed escapes", "a/b", `\u0061\/\u0062`},
		{"Unicode slash", "a/b", `\u0061\u002F\u0062`},
		{"Unicode backslash", `a\b`, `a\u005Cb`},
		{"mixed hex case", "sk-key", `s\u006B-\u006bey`},
		{"escaped quote", `a"b`, `a\u0022b`},
		{"surrogate pair", "😀/x", `\uD83d\uDE00\/x`},
		{"unpaired surrogate", "�/x", `\uD800\/x`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := redactionMarker(tc.secret)
			text := "prefix " + tc.encoded + " suffix"
			if got := RedactText(text, tc.secret); got != "prefix "+marker+" suffix" {
				t.Fatalf("equivalent JSON encoding survived: %q", got)
			}
			cause := errors.New(text)
			if got := RedactError(cause, tc.secret); got.Error() != "prefix "+marker+" suffix" || !errors.Is(got, cause) {
				t.Fatalf("error kept encoded key or lost cause: %v", got)
			}
			var tail bounded.Tail
			if _, err := tail.Write([]byte(text)); err != nil || tail.Truncated {
				t.Fatalf("unexpected diagnostic tail: %v, %t", err, tail.Truncated)
			}
			if got := RedactTail(&tail, tc.secret); got != "prefix "+marker+" suffix" {
				t.Fatalf("diagnostic kept encoded key: %q", got)
			}
			document := `{"error":"` + tc.encoded + `","hint":"limit hit"}`
			got := RedactText(document, tc.secret)
			if !json.Valid([]byte(got)) || got != `{"error":"`+marker+`","hint":"limit hit"}` {
				t.Fatalf("redaction broke JSON or unrelated diagnostic: %q", got)
			}
		})
	}
	if got := RedactText(`a\u0061a\u0062`, "aab"); got != "a[redacted]" {
		t.Fatalf("overlapping decoded prefixes hid an encoded key: %q", got)
	}
	if got := RedactText(`prefix \u0061"b suffix`, `a"b`); got != "prefix [redacted] suffix" {
		t.Fatalf("freeform diagnostic kept a mixed escaped key: %q", got)
	}
	const adjacentDelimiter = `{"x":"\u0061"}`
	if got := RedactText(adjacentDelimiter, `a"`); got != adjacentDelimiter || !json.Valid([]byte(got)) {
		t.Fatalf("matched through a structural JSON quote: %q", got)
	}
	const escapedQuote = `{"x":"\u0061\""}`
	if got := RedactText(escapedQuote, `a"`); got != `{"x":"[redacted]"}` || !json.Valid([]byte(got)) {
		t.Fatalf("missed an escaped quote inside JSON content: %q", got)
	}
}

func TestRedactTailCutsPartialEquivalentJSONEscapes(t *testing.T) {
	const note = "rate limit reached"
	for _, tc := range []struct{ name, secret, encoded string }{
		{"ASCII", "a/b", `\u0061\/\u0062`},
		{"surrogate pair", "😀/x", `\uD83D\uDE00\/x`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for cut := 1; cut < len(tc.encoded); cut++ {
				fill := strings.Repeat("x", bounded.DiagnosticBytes-len(tc.encoded[cut:])-len(note))
				var tail bounded.Tail
				if _, err := tail.Write([]byte(tc.encoded + fill + note)); err != nil || !tail.Truncated {
					t.Fatalf("cut %d: unexpected diagnostic tail: %v, %t", cut, err, tail.Truncated)
				}
				if got := RedactTail(&tail, tc.secret); strings.TrimLeft(got, "x") != note {
					t.Fatalf("cut %d: partial encoded key survived or diagnostic lost: %q", cut, got)
				}
			}
		})
	}
}

func TestRedactEquivalentJSONEscapesInLargeAnswer(t *testing.T) {
	const secret = "a/b"
	t.Run("freeform", func(t *testing.T) {
		prefix := strings.Repeat(`\q`, (64<<20)/2)
		got := RedactText(prefix+`a\/b`, secret)
		if len(got) != len(prefix)+len("[redacted]") || !strings.HasSuffix(got, "[redacted]") {
			t.Fatalf("large escaped answer was not redacted: length=%d, suffix=%q", len(got), got[len(got)-min(len(got), 20):])
		}
	})
	t.Run("JSON", func(t *testing.T) {
		prefix := strings.Repeat(`\\`, (64<<20)/2)
		document := `{"answer":"` + prefix + `a\/b"}`
		got := RedactText(document, secret)
		if len(got) != len(document)-len(`a\/b`)+len("[redacted]") || !strings.HasSuffix(got, `[redacted]"}`) || !json.Valid([]byte(got)) {
			t.Fatalf("large JSON answer was not safely redacted: length=%d, suffix=%q", len(got), got[len(got)-min(len(got), 20):])
		}
	})
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
	if want := fill[6*len(secret)-1-len("x"+secret):] + note; got != want {
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

func TestRedactTailDoesNotCutThroughExpandedMarker(t *testing.T) {
	var tail bounded.Tail
	if _, err := tail.Write([]byte(strings.Repeat("x", bounded.DiagnosticBytes))); err != nil || tail.Truncated {
		t.Fatalf("unexpected tail state: %v, %t", err, tail.Truncated)
	}
	got := RedactTail(&tail, "x")
	want := strings.Repeat("[redacted]", bounded.DiagnosticBytes/len("[redacted]"))
	if got != want {
		t.Fatalf("expanded redaction marker was cut: prefix=%q length=%d, want length=%d", got[:min(20, len(got))], len(got), len(want))
	}
}
