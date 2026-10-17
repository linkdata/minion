package aicli

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestJSONLFramingAndFirstLimit(t *testing.T) {
	for _, size := range []int{1, 7, 1000} {
		var lines []string
		output := JSONL{}
		accept := func(p []byte) { lines = append(lines, string(p)) }
		fail := func(err error) { t.Fatal(err) }
		for data := " \n{}\n  last \r"; len(data) > 0; {
			n := min(size, len(data))
			if count, err := output.Write([]byte(data[:n]), accept, fail); count != n || err != nil {
				t.Fatalf("write=%d,%v", count, err)
			}
			data = data[n:]
		}
		output.Finish()
		if !slices.Equal(lines, []string{"{}", "last"}) || output.Events != 2 {
			t.Fatalf("chunk=%d events=%d lines=%q", size, output.Events, lines)
		}
	}
	for _, tc := range []struct {
		name string
		init JSONL
		data string
	}{
		{"bytes", JSONL{Bytes: MaxOutputBytes}, "x"},
		{"events", JSONL{Events: MaxEvents}, "{}\n"},
		{"line", JSONL{}, strings.Repeat("x", MaxEventBytes+1) + "\n{}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, failures := tc.init, 0
			fail := func(err error) {
				if !errors.Is(err, ErrOutputLimit) {
					t.Fatalf("limit identity lost: %v", err)
				}
				failures++
			}
			line := func([]byte) { t.Fatal("limited stream dispatched an event") }
			_, _ = output.Write([]byte(tc.data), line, fail)
			_, _ = output.Write([]byte("{}\n"), line, fail)
			output.Finish()
			if failures != 1 {
				t.Fatalf("limit fired %d times", failures)
			}
		})
	}
}
