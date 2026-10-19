// Package bounded retains bounded command output.
package bounded

import (
	"bytes"
	"context"
)

// DiagnosticBytes bounds captured host-command diagnostics, not transport fields.
const DiagnosticBytes = 8 << 10

// Buffer discards output beyond Limit and optionally cancels its producer.
// Write reports the full input length so subprocess output continues to drain.
type Buffer struct {
	Limit    int
	Cancel   context.CancelFunc
	Exceeded bool
	buffer   bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := max(0, b.Limit-b.buffer.Len())
	if len(p) > remaining {
		if !b.Exceeded && b.Cancel != nil {
			b.Cancel()
		}
		b.Exceeded = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *Buffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *Buffer) String() string { return b.buffer.String() }

// Tail drains command diagnostics while retaining their last DiagnosticBytes.
type Tail struct {
	data      []byte
	Truncated bool
}

func (b *Tail) Write(p []byte) (int, error) {
	n, limit := len(p), DiagnosticBytes
	if n >= limit {
		b.Truncated = b.Truncated || n > limit || len(b.data) > 0
		b.data = append(b.data[:0], p[n-limit:]...)
		return n, nil
	}
	if excess := len(b.data) + n - limit; excess > 0 {
		b.Truncated = true
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *Tail) String() string { return string(b.data) }

// Text bounds diagnostic text and marks a discarded suffix.
func Text(s string) string {
	if len(s) > DiagnosticBytes {
		return s[:DiagnosticBytes] + "..."
	}
	return s
}
