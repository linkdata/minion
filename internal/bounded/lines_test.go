package bounded

import (
	"reflect"
	"strings"
	"testing"
)

func TestLinesChunksBoundsAndEOF(t *testing.T) {
	for _, chunk := range []int{1, 2, 100} {
		var lines []string
		overflows := 0
		w := Lines{Limit: 3, Line: func(line []byte) { lines = append(lines, string(line)) }, Overflow: func() { overflows++ }}
		data := "abc\n\nabcdmore\nx\nyz"
		for len(data) != 0 {
			n := min(chunk, len(data))
			if wrote, err := w.Write([]byte(data[:n])); wrote != n || err != nil {
				t.Fatalf("Write=%d,%v", wrote, err)
			}
			if len(w.line) > w.Limit {
				t.Fatalf("unbounded retained line: %d", len(w.line))
			}
			data = data[n:]
		}
		if !reflect.DeepEqual(lines, []string{"abc", "", "x"}) || overflows != 1 {
			t.Fatalf("chunk%d lines=%q overflows=%d", chunk, lines, overflows)
		}
		w.Finish()
		w.Finish()
		if !reflect.DeepEqual(lines, []string{"abc", "", "x", "yz"}) || overflows != 1 {
			t.Fatalf("chunk%d EOF lines=%q overflows=%d", chunk, lines, overflows)
		}
	}
}

func TestLinesOverflowIsImmediateAndDrainsRemainder(t *testing.T) {
	calls := 0
	w := Lines{Limit: 3, Overflow: func() { calls++ }, Line: func([]byte) { t.Fatal("oversized line delivered") }}
	_, _ = w.Write([]byte("abc"))
	if calls != 0 {
		t.Fatal("exact limit rejected")
	}
	_, _ = w.Write([]byte("d"))
	if calls != 1 {
		t.Fatal("overflow delayed until newline")
	}
	_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	w.Finish()
	if calls != 1 || len(w.line) != 0 || w.dropping {
		t.Fatalf("EOF failed to drain oversized remainder: calls=%d writer=%+v", calls, w)
	}
}

func TestLinesZeroLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		empty, overflow := 0, 0
		w := Lines{Limit: limit, Line: func(p []byte) {
			if len(p) != 0 {
				t.Fatal("nonempty zero-limit line")
			}
			empty++
		}, Overflow: func() { overflow++ }}
		_, _ = w.Write([]byte("\na\n"))
		w.Finish()
		if empty != 1 || overflow != 1 {
			t.Fatalf("limit%d empty=%d overflow=%d", limit, empty, overflow)
		}
	}
}
