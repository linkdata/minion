package claudecli

import "bytes"

// waitCeiling remembers the native stop diagnostic across the whole stderr
// stream. Overlong lines are discarded; Write always drains its input.
type waitCeiling struct {
	matched  bool
	line     []byte
	dropping bool
}

func (m *waitCeiling) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) != 0 && !m.matched {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if len(part) > maxEventBytes-len(m.line) {
			m.line, m.dropping = nil, true
		}
		if !m.dropping {
			m.line = append(m.line, part...)
		}
		if end < 0 {
			break
		}
		m.finish()
		m.line, m.dropping = m.line[:0], false
		p = p[end+1:]
	}
	return n, nil
}

func (m *waitCeiling) finish() {
	// Managed CLI settings can override its launch environment. Match this
	// diagnostic independently of the separately retained stderr tail.
	line := bytes.TrimRight(m.line, " \t\r")
	if !m.dropping && bytes.HasPrefix(line, []byte("Background tasks still running ")) && bytes.HasSuffix(line, []byte("; stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely.")) {
		m.matched = true
	}
}
