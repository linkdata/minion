package aicli

import (
	"bytes"
	"fmt"

	"github.com/linkdata/minion/internal/bounded"
)

const (
	MaxEventBytes  = 4 << 20
	MaxOutputBytes = 64 << 20
	MaxEvents      = 100000
)

// JSONL frames bounded provider output; adapters retain their own event policy.
type JSONL struct {
	Bytes   int64
	Events  int
	lines   bounded.Lines
	limited bool
}

// Write drains p, dispatching nonblank lines and reporting the first limit.
// Callbacks must remain the same for the lifetime of the stream.
func (o *JSONL) Write(p []byte, line func([]byte), fail func(error)) (int, error) {
	n := len(p)
	stop := func(reason string) {
		if !o.limited {
			o.limited = true
			fail(fmt.Errorf("%w: JSONL %s", ErrOutputLimit, reason))
		}
	}
	if o.limited {
		return n, nil
	}
	if int64(n) > MaxOutputBytes-o.Bytes {
		stop("output exceeded the size limit")
		return n, nil
	}
	o.Bytes += int64(n)
	if o.lines.Line == nil {
		o.lines = bounded.Lines{Limit: MaxEventBytes, Line: func(p []byte) {
			p = bytes.TrimSpace(p)
			if o.limited || len(p) == 0 {
				return
			}
			o.Events++
			if o.Events > MaxEvents {
				stop("event count exceeded the limit")
				return
			}
			line(p)
		}, Overflow: func() { stop("event exceeded the output size limit") }}
	}
	_, _ = o.lines.Write(p)
	return n, nil
}

// Finish dispatches a final unterminated event.
func (o *JSONL) Finish() { o.lines.Finish() }
