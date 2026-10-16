// Package bounded retains a bounded prefix of command output.
package bounded

import (
	"bytes"
	"context"
	"io"
)

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

func (b *Buffer) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{b}, r)
}

func (b *Buffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *Buffer) String() string { return b.buffer.String() }
func (b *Buffer) Len() int       { return b.buffer.Len() }
