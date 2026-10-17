package bounded

import (
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
		if output.String() != text[len(text)-DiagnosticBytes:] {
			t.Fatalf("chunk=%d tail=%q", size, output.String())
		}
	}

}

func TestTextPreservesUTF8AtTheDiagnosticBound(t *testing.T) {
	for _, scenario := range []struct{ value, want string }{
		{strings.Repeat("a", DiagnosticBytes-1) + "€b", strings.Repeat("a", DiagnosticBytes-1) + "..."},
		{strings.Repeat("a", DiagnosticBytes-3) + "€b", strings.Repeat("a", DiagnosticBytes-3) + "€..."},
		{strings.Repeat("a", DiagnosticBytes-4) + "€b", strings.Repeat("a", DiagnosticBytes-4) + "€b"},
		{strings.Repeat("a", DiagnosticBytes-1) + "é", strings.Repeat("a", DiagnosticBytes-1) + "..."},
		{strings.Repeat("a", DiagnosticBytes-3) + "😀", strings.Repeat("a", DiagnosticBytes-3) + "..."},
		{strings.Repeat("a", DiagnosticBytes-4) + "😀b", strings.Repeat("a", DiagnosticBytes-4) + "😀..."},
		{"a\xffb", "a\xffb"},
		{"a\xff" + strings.Repeat("b", DiagnosticBytes), "a\xff" + strings.Repeat("b", DiagnosticBytes-2) + "..."},
		{strings.Repeat("a", DiagnosticBytes) + "\x80b", strings.Repeat("a", DiagnosticBytes) + "..."},
		{strings.Repeat("a", DiagnosticBytes-1) + "\x80b", strings.Repeat("a", DiagnosticBytes-1) + "\x80..."},
	} {
		if got := Text(scenario.value); got != scenario.want {
			t.Fatalf("Text(%q)=%q want%q", scenario.value, got, scenario.want)
		}
	}
}
