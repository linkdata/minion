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
		output.Line, output.Fail = accept, fail
		for data := " \n{}\n  last \r"; len(data) > 0; {
			n := min(size, len(data))
			if count, err := output.Write([]byte(data[:n])); count != n || err != nil {
				t.Fatalf("write=%d,%v", count, err)
			}
			data = data[n:]
		}
		output.Finish()
		if !slices.Equal(lines, []string{"{}", "last"}) || output.events != 2 {
			t.Fatalf("chunk=%d events=%d lines=%q", size, output.events, lines)
		}
	}
	for _, tc := range []struct {
		name string
		init JSONL
		data string
	}{
		{"bytes", JSONL{total: maxJSONLOutputBytes}, "x"},
		{"events", JSONL{events: maxEvents}, "{}\n"},
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
			output.Line, output.Fail = line, fail
			_, _ = output.Write([]byte(tc.data))
			_, _ = output.Write([]byte("{}\n"))
			output.Finish()
			if failures != 1 {
				t.Fatalf("limit fired %d times", failures)
			}
		})
	}
}

func TestJSONLAcceptsExactTotalLimits(t *testing.T) {
	for _, kind := range []string{"bytes", "events"} {
		t.Run(kind, func(t *testing.T) {
			dispatched, failures := 0, 0
			output := JSONL{Line: func([]byte) { dispatched++ }, Fail: func(err error) {
				if !errors.Is(err, ErrOutputLimit) {
					t.Fatalf("wrong failure: %v", err)
				}
				failures++
			}}
			if kind == "bytes" {
				output.total = maxJSONLOutputBytes - 1
				_, _ = output.Write([]byte("\n"))
				if output.total != maxJSONLOutputBytes || dispatched != 0 {
					t.Fatalf("bytes=%d dispatched=%d", output.total, dispatched)
				}
			} else {
				output.events = maxEvents - 1
				_, _ = output.Write([]byte("{}\n"))
				if output.events != maxEvents || dispatched != 1 {
					t.Fatalf("events=%d dispatched=%d", output.events, dispatched)
				}
			}
			if failures != 0 {
				t.Fatal("exact limit was rejected")
			}
			extra := "{}\n"
			if kind == "bytes" {
				extra = "x"
			}
			_, _ = output.Write([]byte(extra))
			_, _ = output.Write([]byte("{}\n"))
			output.Finish()
			if failures != 1 {
				t.Fatalf("overflow failed %d times", failures)
			}
		})
	}
}

func TestJSONLCRLFAtExactEventByteLimit(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(map[int]string{0: "exact", 1: "one byte over"}[extra], func(t *testing.T) {
			var lines []string
			var failures []error
			output := JSONL{
				Line: func(p []byte) { lines = append(lines, string(p)) },
				Fail: func(err error) { failures = append(failures, err) },
			}
			event := strings.Repeat("x", MaxEventBytes-1+extra) + "\r\n"
			if count, err := output.Write([]byte(event)); count != len(event) || err != nil {
				t.Fatalf("write=%d,%v", count, err)
			}
			output.Finish()
			if extra == 0 {
				if len(failures) != 0 || len(lines) != 1 || lines[0] != strings.TrimSpace(event) {
					t.Fatalf("exact CRLF event: lines=%d failures=%v", len(lines), failures)
				}
			} else if len(lines) != 0 || len(failures) != 1 || !errors.Is(failures[0], ErrOutputLimit) {
				t.Fatalf("oversized CRLF event: lines=%d failures=%v", len(lines), failures)
			}
		})
	}
}
