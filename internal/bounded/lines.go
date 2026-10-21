package bounded

import "bytes"

// Lines drains a stream into bounded lines without their newline bytes.
// Limit must be positive. Line must be set; its slice is valid only during the callback.
// Overflow fires immediately once per oversized line; that whole line is discarded
// through newline or EOF.
type Lines struct {
	Limit    int
	Line     func([]byte)
	Overflow func()
	line     []byte
	dropping bool
}

func (w *Lines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) != 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if !w.dropping {
			if len(part) > w.Limit-len(w.line) {
				w.line, w.dropping = w.line[:0], true
				if w.Overflow != nil {
					w.Overflow()
				}
			} else {
				w.line = append(w.line, part...)
			}
		}
		if end < 0 {
			break
		}
		if !w.dropping {
			w.Line(w.line)
		}
		w.line, w.dropping = w.line[:0], false
		p = p[end+1:]
	}
	return n, nil
}

// Finish delivers a nonempty final line and discards an oversized remainder.
func (w *Lines) Finish() {
	if !w.dropping && len(w.line) != 0 {
		w.Line(w.line)
	}
	w.line, w.dropping = w.line[:0], false
}
