package claudecli

import (
	"bytes"

	"github.com/linkdata/minion/internal/bounded"
)

// waitCeiling remembers the native stop diagnostic across the whole stderr
// stream. Overlong lines are discarded; Write always drains its input.
type waitCeiling struct {
	matched bool
	lines   bounded.Lines
}

func (m *waitCeiling) Write(p []byte) (int, error) {
	if m.lines.Line == nil {
		m.lines = bounded.Lines{Limit: maxEventBytes, Line: m.match}
	}
	return m.lines.Write(p)
}

func (m *waitCeiling) finish() { m.lines.Finish() }

func (m *waitCeiling) match(line []byte) {
	// Managed CLI settings can override its launch environment. Match this
	// diagnostic independently of the separately retained stderr tail.
	line = bytes.TrimRight(line, " \t\r")
	if bytes.HasPrefix(line, []byte("Background tasks still running ")) && bytes.HasSuffix(line, []byte("; stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely.")) {
		m.matched = true
	}
}
