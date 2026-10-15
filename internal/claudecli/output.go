package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/usage"
)

const (
	maxEventBytes      = 4 << 20
	maxOutputBytes     = 64 << 20
	maxEvents          = 10000
	maxDiagnosticBytes = 8 << 10
)

type eventUsage struct {
	Input   *int64 `json:"input_tokens"`
	Created *int64 `json:"cache_creation_input_tokens"`
	Cached  *int64 `json:"cache_read_input_tokens"`
	Output  *int64 `json:"output_tokens"`
}

func (u *eventUsage) tokens() (tokens usage.Tokens, valid bool) {
	if u == nil || u.Input == nil || u.Created == nil || u.Cached == nil || u.Output == nil {
		return
	}
	for _, value := range []*int64{u.Input, u.Created, u.Cached, u.Output} {
		if *value < 0 {
			return
		}
	}
	if *u.Created > math.MaxInt64-*u.Input || *u.Cached > math.MaxInt64-*u.Input-*u.Created {
		return
	}
	tokens = usage.Tokens{Input: *u.Input + *u.Created + *u.Cached, CachedInput: *u.Cached, Output: *u.Output}
	valid = tokens.Output <= math.MaxInt64-tokens.Input
	return
}

type event struct {
	Type              string            `json:"type"`
	Subtype           string            `json:"subtype"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	Errors            []string          `json:"errors"`
	Usage             *eventUsage       `json:"usage"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           json.RawMessage   `json:"message"`
}

type jsonlOutput struct {
	cancel     context.CancelFunc
	line       []byte
	bytes      int64
	events     int
	err        error
	completed  bool
	structured bool
	result     aicli.Result
}

func (o *jsonlOutput) Write(p []byte) (int, error) {
	n := len(p)
	if o.err != nil {
		return n, nil
	}
	if int64(n) > maxOutputBytes-o.bytes {
		o.fail(fmt.Errorf("%w: JSONL bytes", aicli.ErrOutputLimit))
		return n, nil
	}
	o.bytes += int64(n)
	for len(p) > 0 && o.err == nil {
		end := bytes.IndexByte(p, '\n')
		fragment := p
		if end >= 0 {
			fragment = p[:end]
		}
		if len(fragment) > maxEventBytes-len(o.line) {
			o.fail(fmt.Errorf("%w: JSONL event bytes", aicli.ErrOutputLimit))
			break
		}
		o.line = append(o.line, fragment...)
		if end < 0 {
			break
		}
		o.finishLine()
		p = p[end+1:]
	}
	return n, nil
}

func (o *jsonlOutput) fail(err error) {
	if o.err == nil {
		o.err = err
		o.result.UsageComplete = false
		if o.cancel != nil {
			o.cancel()
		}
	}
}

func (o *jsonlOutput) finish() {
	if o.err == nil && len(o.line) != 0 {
		o.finishLine()
	}
}

func (o *jsonlOutput) finishLine() {
	line := bytes.TrimSpace(o.line)
	if len(line) == 0 {
		o.line = o.line[:0]
		return
	}
	o.events++
	if o.events > maxEvents {
		o.fail(fmt.Errorf("%w: JSONL events", aicli.ErrOutputLimit))
		return
	}
	var e event
	if err := json.Unmarshal(line, &e); err != nil {
		o.fail(fmt.Errorf("invalid Claude JSONL event: %w", err))
		return
	}
	o.line = o.line[:0]
	if e.Type == "user" && len(e.Message) != 0 {
		var message struct {
			Content []struct {
				Type    string          `json:"type"`
				IsError bool            `json:"is_error"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		}
		if err := json.Unmarshal(e.Message, &message); err != nil {
			o.fail(fmt.Errorf("invalid Claude tool response: %w", err))
			return
		}
		for _, content := range message.Content {
			if content.Type == "tool_result" && content.IsError {
				if toolSandboxFailure(content.Content) {
					o.fail(errors.New("sandbox could not start"))
					return
				}
			}
		}
	}
	if e.Type != "result" {
		return
	}
	if o.completed {
		o.fail(errors.New("duplicate Claude result"))
		return
	}
	o.completed = true
	o.result.Tokens, o.result.UsageComplete = e.Usage.tokens()
	if e.IsError || e.Subtype != "success" {
		o.fail(fmt.Errorf("result failed: %s", limitText(e.Subtype)))
		return
	}
	if len(e.PermissionDenials) != 0 {
		o.fail(errors.New("task encountered denied tool permissions"))
		return
	}
	if o.structured {
		data := bytes.TrimSpace(e.StructuredOutput)
		if len(data) == 0 || bytes.Equal(data, []byte("null")) {
			o.fail(errors.New("missing Claude structured output"))
			return
		}
		o.result.Answer = string(data)
	} else {
		o.result.Answer = e.Result
	}
}

func toolSandboxFailure(content json.RawMessage) bool {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return sandboxFailure(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, block := range blocks {
			if block.Type == "text" && sandboxFailure(block.Text) {
				return true
			}
		}
	}
	return false
}

func sandboxFailure(text string) bool {
	for _, line := range bytes.Split([]byte(text), []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("apply-seccomp:")) || bytes.HasPrefix(line, []byte("bwrap:")) {
			return true
		}
	}
	return false
}

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= maxDiagnosticBytes {
		b.data = append(b.data[:0], p[n-maxDiagnosticBytes:]...)
		return n, nil
	}
	if excess := len(b.data) + n - maxDiagnosticBytes; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.data) }

func limitText(s string) string {
	if len(s) > maxDiagnosticBytes {
		return s[:maxDiagnosticBytes] + "..."
	}
	return s
}
