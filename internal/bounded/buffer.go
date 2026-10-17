// Package bounded retains bounded command output.
package bounded

import (
	"bytes"
	"context"
	"strings"
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

func (b *Buffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *Buffer) String() string { return b.buffer.String() }

// Tail drains command diagnostics while retaining only their last Limit bytes.
type Tail struct {
	Limit int
	data  []byte
}

func (b *Tail) Write(p []byte) (int, error) {
	n, limit := len(p), max(0, b.Limit)
	if n >= limit {
		b.data = append(b.data[:0], p[n-limit:]...)
		return n, nil
	}
	if excess := len(b.data) + n - limit; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *Tail) String() string { return string(b.data) }

// LineMatch remembers a matching diagnostic line while bounding partial lines.
// Overlong lines are discarded through their next newline; Write always drains.
type LineMatch struct {
	Limit    int
	Match    func([]byte) bool
	Matched  bool
	line     []byte
	dropping bool
}

func (m *LineMatch) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) != 0 && !m.Matched {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if len(part) > max(0, m.Limit)-len(m.line) {
			m.line, m.dropping = nil, true
		}
		if !m.dropping {
			m.line = append(m.line, part...)
		}
		if end < 0 {
			break
		}
		if !m.dropping {
			m.Finish()
		}
		m.line, m.dropping = m.line[:0], false
		p = p[end+1:]
	}
	return n, nil
}

func (m *LineMatch) Finish() {
	if !m.dropping && m.Match != nil && m.Match(m.line) {
		m.Matched = true
	}
}

// Text bounds diagnostic text and marks a discarded suffix.
func Text(s string, limit int) string {
	limit = max(0, limit)
	if len(s) > limit {
		return strings.ToValidUTF8(s[:limit], "") + "..."
	}
	return s
}
