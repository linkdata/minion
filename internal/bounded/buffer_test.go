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
	for _, limit := range []int{0, 3} {
		for _, chunks := range [][]string{{"abcdef"}, {"a", "bc", "de", "f"}, {"abc", "def"}} {
			output := Tail{Limit: limit}
			for _, chunk := range chunks {
				if n, err := output.Write([]byte(chunk)); err != nil || n != len(chunk) {
					t.Fatalf("write=%d,%v", n, err)
				}
			}
			if output.String() != "abcdef"[6-limit:] {
				t.Fatalf("limit=%d tail=%q", limit, output.String())
			}
		}
	}
	if Text("abc", 3) != "abc" || Text("abcd", 3) != "abc..." || Text("a", 0) != "..." {
		t.Fatal("diagnostic text bounds changed")
	}
}

func TestTextPreservesUTF8AtTheDiagnosticBound(t *testing.T) {
	for _, scenario := range []struct {
		value string
		limit int
		want  string
	}{
		{"a€b", 2, "a..."}, {"a€b", 4, "a€..."}, {"a€b", 5, "a€b"}, {"éx", 1, "..."},
	} {
		if got := Text(scenario.value, scenario.limit); got != scenario.want {
			t.Fatalf("Text(%q,%d)=%q want%q", scenario.value, scenario.limit, got, scenario.want)
		}
	}
}
