package bounded

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestBufferBoundsCopiedOutputAndCancels(t *testing.T) {
	canceled := 0
	output := Buffer{Limit: 3, Cancel: func() { canceled++ }}
	n, err := io.Copy(&output, io.LimitReader(strings.NewReader("long output"), 100))
	if err != nil || n != 11 || !output.Exceeded || output.String() != "lon" || canceled != 1 {
		t.Fatalf("copy=%d,%v output=%q exceeded=%v cancellations=%d", n, err, output.String(), output.Exceeded, canceled)
	}
	if n, err := output.Write([]byte("more")); n != 4 || err != nil || canceled != 1 || len(output.Bytes()) != 3 {
		t.Fatalf("subsequent write=%d,%v len=%d cancellations=%d", n, err, len(output.Bytes()), canceled)
	}
}

func TestBufferExactAndZeroLimit(t *testing.T) {
	for _, limit := range []int{0, 3} {
		output := Buffer{Limit: limit}
		_, _ = output.Write([]byte("abc")[:limit])
		if output.Exceeded || len(output.Bytes()) != limit {
			t.Fatalf("exact limit=%d output=%q exceeded=%v", limit, output.Bytes(), output.Exceeded)
		}
		_, _ = output.Write([]byte("x"))
		if !output.Exceeded || len(output.Bytes()) != limit {
			t.Fatalf("overflow limit=%d output=%q exceeded=%v", limit, output.Bytes(), output.Exceeded)
		}
	}
}

func TestTailDrainsAndKeepsSuffix(t *testing.T) {
	text := strings.Repeat("x", DiagnosticBytes) + "suffix"
	for _, size := range []int{1, 3, DiagnosticBytes + 10} {
		output := Tail{}
		for remaining := text; len(remaining) > 0; {
			next := min(size, len(remaining))
			if n, err := output.Write([]byte(remaining[:next])); err != nil || n != next {
				t.Fatalf("write=%d,%v", n, err)
			}
			remaining = remaining[next:]
		}
		if output.String() != text[len(text)-DiagnosticBytes:] || !output.Truncated {
			t.Fatalf("chunk=%d tail=%q truncated=%t", size, output.String(), output.Truncated)
		}
	}

	var exact Tail
	if n, err := exact.Write([]byte(strings.Repeat("x", DiagnosticBytes))); n != DiagnosticBytes || err != nil || exact.Truncated {
		t.Fatalf("exact tail write=%d error=%v truncated=%t", n, err, exact.Truncated)
	}
	if n, err := exact.Write([]byte(strings.Repeat("y", DiagnosticBytes))); n != DiagnosticBytes || err != nil || !exact.Truncated || exact.String() != strings.Repeat("y", DiagnosticBytes) {
		t.Fatalf("replacement tail write=%d error=%v truncated=%t", n, err, exact.Truncated)
	}
}

func TestTextBoundsBytesForQuotedDiagnostics(t *testing.T) {
	value := strings.Repeat("a", DiagnosticBytes-1) + "€b"
	if got := Text(value); got != value[:DiagnosticBytes]+"..." || !strings.HasSuffix(fmt.Sprintf("%q", got), `\xe2..."`) {
		t.Fatalf("byte-bound diagnostic=%q", got)
	}
	for _, value := range []string{"short\xff", strings.Repeat("x", DiagnosticBytes)} {
		if got := Text(value); got != value {
			t.Fatalf("changed untruncated diagnostic: %q", got)
		}
	}
}
