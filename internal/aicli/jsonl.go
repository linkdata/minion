package aicli

import (
	"bytes"
	"fmt"

	"github.com/linkdata/minion/internal/bounded"
)

const (
	MaxEventBytes       = 4 << 20
	maxJSONLOutputBytes = 64 << 20
	maxEvents           = 100000
)

// JSONL frames bounded provider output; adapters retain their own event policy.
type JSONL struct {
	Line    func([]byte)
	Fail    func(error)
	total   int64
	events  int
	lines   bounded.Lines
	limited bool
}

// Write drains p, dispatching nonblank lines and reporting the first limit.
// Line and Fail must be configured before the first write.
func (o *JSONL) Write(p []byte) (int, error) {
	n := len(p)
	if o.limited {
		return n, nil
	}
	if int64(n) > maxJSONLOutputBytes-o.total {
		o.stop("output exceeded the size limit")
		return n, nil
	}
	o.total += int64(n)
	if o.lines.Line == nil {
		o.lines = bounded.Lines{Limit: MaxEventBytes, Line: func(p []byte) {
			p = bytes.TrimSpace(p)
			if o.limited || len(p) == 0 {
				return
			}
			o.events++
			if o.events > maxEvents {
				o.stop("event count exceeded the limit")
				return
			}
			o.Line(p)
		}, Overflow: func() { o.stop("event exceeded the size limit") }}
	}
	_, _ = o.lines.Write(p)
	return n, nil
}

func (o *JSONL) stop(reason string) {
	if !o.limited {
		o.limited = true
		o.Fail(fmt.Errorf("%w: JSONL %s", ErrOutputLimit, reason))
	}
}

// Finish dispatches a final unterminated event.
func (o *JSONL) Finish() { o.lines.Finish() }
