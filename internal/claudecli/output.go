package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/usage"
)

const (
	maxEventBytes      = 4 << 20
	maxOutputBytes     = 64 << 20
	maxEvents          = 100000
	maxDiagnosticBytes = 8 << 10
	maxResetTimestamp  = 253402300799 // 9999-12-31T23:59:59Z, the last RFC3339 year.
)

type eventUsage struct {
	Input   *int64 `json:"input_tokens"`
	Created *int64 `json:"cache_creation_input_tokens"`
	Cached  *int64 `json:"cache_read_input_tokens"`
	Output  *int64 `json:"output_tokens"`
}

func (u eventUsage) tokens() (tokens usage.Tokens, valid bool) {
	if u.Input == nil || u.Created == nil || u.Cached == nil || u.Output == nil {
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
	ResultIndex       *int64            `json:"result_index"`
	Tasks             json.RawMessage   `json:"tasks"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	Usage             json.RawMessage   `json:"usage"`
	ModelUsage        json.RawMessage   `json:"modelUsage"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           json.RawMessage   `json:"message"`
	ParentToolUseID   json.RawMessage   `json:"parent_tool_use_id"`
	Status            string            `json:"status"`
	Summary           string            `json:"summary"`
	APIError          string            `json:"api_error"`
	APIErrorStatus    json.RawMessage   `json:"api_error_status"`
	IsSynthetic       bool              `json:"isSynthetic"`
	RateLimitInfo     struct {
		Status         string          `json:"status"`
		ResetsAt       json.RawMessage `json:"resetsAt"`
		IsUsingOverage bool            `json:"isUsingOverage"`
		OverageInUse   bool            `json:"overageInUse"`
	} `json:"rate_limit_info"`
}

func (e event) tokens() (usage.Tokens, bool) {
	var models map[string]struct {
		Input   *int64 `json:"inputTokens"`
		Created *int64 `json:"cacheCreationInputTokens"`
		Cached  *int64 `json:"cacheReadInputTokens"`
		Output  *int64 `json:"outputTokens"`
	}
	if json.Unmarshal(e.ModelUsage, &models) == nil && len(models) != 0 {
		var total usage.Tokens
		valid := true
		for _, model := range models {
			tokens, ok := eventUsage(model).tokens()
			if !ok {
				valid = false
				break
			}
			if total, ok = sumTokens(total, tokens); !ok {
				valid = false
				break
			}
		}
		if valid {
			return total, true
		}
	}
	// Top-level usage excludes subagents, so it is only an incomplete fallback.
	var fallback eventUsage
	if json.Unmarshal(e.Usage, &fallback) == nil {
		if tokens, valid := fallback.tokens(); valid {
			return tokens, false
		}
	}
	return usage.Tokens{}, false
}

type messageUsage struct {
	tokens     usage.Tokens
	generation int64
	root       bool
}

type jsonlOutput struct {
	cancel              context.CancelFunc
	line                []byte
	bytes               int64
	events              int
	err                 error
	usageLimit          error
	taskFailure         error
	completed           bool
	structured          bool
	pending             int
	results             int64
	indexed             bool
	result              aicli.Result
	aggregateGeneration int64
	aggregateTokens     usage.Tokens
	assistantUsage      map[string]messageUsage
	assistantTokens     usage.Tokens
	rootTokens          usage.Tokens
	uncovered           bool
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
		o.completed, o.result.Answer = false, ""
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
	lower, valid := o.assistantLowerBound()
	if o.aggregateGeneration == 0 || o.uncovered || !valid || lower != o.result.Tokens {
		if valid {
			o.result.Tokens = lower
		}
		o.result.UsageComplete = false
	}
	if failure := errors.Join(o.usageLimit, o.taskFailure); o.err == nil && failure != nil {
		o.fail(failure)
	}
	if o.err == nil && o.pending != 0 {
		o.fail(errors.New("background tasks still running when Claude exited"))
	}
	if o.err == nil {
		answer := strings.TrimSpace(o.result.Answer)
		if !o.completed {
			o.fail(errors.New("missing successful final result from Claude"))
		} else if o.structured && (answer == "" || answer == "null") {
			o.fail(errors.New("missing Claude structured output"))
		} else if answer == "" {
			o.fail(errors.New("missing successful Claude result"))
		}
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
	if e.Type == "assistant" {
		o.recordAssistantUsage(e.Message, bytes.Equal(bytes.TrimSpace(e.ParentToolUseID), []byte("null")))
	}
	if e.Type == "system" && e.Subtype == "task_notification" && e.Status == "stopped" &&
		pressureStopSummary(e.Summary) {
		o.recordPressureStop()
	}
	if e.Type == "rate_limit_event" && e.RateLimitInfo.Status == "rejected" && !e.RateLimitInfo.IsUsingOverage && !e.RateLimitInfo.OverageInUse {
		o.usageLimit = errors.New("provider usage limit reached")
		var reset int64
		// Only a validated UTC timestamp may enter the service diagnostic.
		if json.Unmarshal(e.RateLimitInfo.ResetsAt, &reset) == nil && reset > 0 && reset <= maxResetTimestamp {
			o.usageLimit = fmt.Errorf("provider usage limit reached; resets at %s", time.Unix(reset, 0).UTC().Format(time.RFC3339))
		}
		o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
	}
	if e.Type == "system" && e.Subtype == "background_tasks_changed" {
		var tasks []struct {
			Ambient bool `json:"ambient"`
		}
		if len(e.Tasks) == 0 || e.Tasks[0] != '[' || json.Unmarshal(e.Tasks, &tasks) != nil {
			o.fail(errors.New("invalid Claude background task list"))
			return
		}
		o.pending = 0
		for _, task := range tasks {
			if !task.Ambient {
				o.pending++
			}
		}
		if o.pending != 0 {
			o.completed = false
			o.result.Answer = ""
		}
	}
	if e.Type == "user" && len(e.Message) != 0 {
		var message struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(e.Message, &message); err != nil {
			o.fail(fmt.Errorf("invalid Claude tool response: %w", err))
			return
		}
		content := bytes.TrimSpace(message.Content)
		if e.IsSynthetic && pressureStopNotice(content) {
			o.recordPressureStop()
		}
		if len(content) != 0 && content[0] == '[' {
			var blocks []struct {
				Type    string          `json:"type"`
				IsError bool            `json:"is_error"`
				Content json.RawMessage `json:"content"`
				Text    string          `json:"text"`
			}
			if err := json.Unmarshal(content, &blocks); err != nil {
				o.fail(fmt.Errorf("invalid Claude tool response: %w", err))
				return
			}
			for _, content := range blocks {
				if e.IsSynthetic && content.Type == "text" && pressureStopText(content.Text) {
					o.recordPressureStop()
				}
				if content.Type == "tool_result" && content.IsError {
					if toolSandboxFailure(content.Content) {
						o.fail(errors.New("sandbox could not start"))
						return
					}
				}
			}
		}
	}
	if e.Type != "result" {
		return
	}
	tokens, complete := e.tokens()
	if complete {
		// A cumulative snapshot must cover newly observed root calls before it
		// can replace their lower bound. Delegate/unknown messages can overlap
		// the prior snapshot, so only their component maxima are safe here.
		lower, valid := o.assistantLowerBound()
		complete = valid && tokens.Input >= lower.Input && tokens.CachedInput >= lower.CachedInput && tokens.Output >= lower.Output
	}
	// Native modelUsage is cumulative across result turns. A later partial
	// report must not erase previously recorded usage.
	if tokens.Input >= o.result.Tokens.Input && tokens.CachedInput >= o.result.Tokens.CachedInput && tokens.Output >= o.result.Tokens.Output {
		o.result.Tokens = tokens
	} else {
		if lower := maxTokens(o.result.Tokens, tokens); lower.Output <= math.MaxInt64-lower.Input {
			o.result.Tokens = lower
		}
		complete = false
	}
	if complete {
		o.aggregateTokens = tokens
		o.aggregateGeneration++
		o.rootTokens, o.uncovered = usage.Tokens{}, false
	}
	o.result.UsageComplete = complete && o.usageLimit == nil && o.taskFailure == nil
	if e.IsError || e.Subtype != "success" {
		if failure := errors.Join(o.usageLimit, o.taskFailure); failure != nil {
			o.fail(failure)
		} else {
			reason := fmt.Sprintf("result failed: %q", bounded.Text(e.Subtype, maxDiagnosticBytes))
			switch e.APIError {
			case "usage_limit_reached", "model_requires_usage_credits", "long_context_credits_required", "provider_credentials":
				reason += "; API error=" + e.APIError
			}
			var status int
			if json.Unmarshal(e.APIErrorStatus, &status) == nil && status >= 100 && status <= 599 {
				reason += fmt.Sprintf("; HTTP status=%d", status)
			}
			o.fail(errors.New(reason))
		}
		return
	}
	if len(e.PermissionDenials) != 0 {
		o.fail(errors.New("task encountered denied tool permissions"))
		return
	}
	if o.results != 0 && (!o.indexed || e.ResultIndex == nil) {
		o.fail(errors.New("multiple Claude results require native result_index"))
		return
	}
	if e.ResultIndex != nil && *e.ResultIndex != o.results {
		o.fail(fmt.Errorf("invalid Claude result_index %d, want %d", *e.ResultIndex, o.results))
		return
	}
	o.indexed = e.ResultIndex != nil
	o.results++
	if o.usageLimit != nil || o.taskFailure != nil {
		return
	}
	// Print mode can emit provisional turn results while background work runs.
	if o.pending != 0 {
		return
	}
	if o.structured {
		o.result.Answer = string(bytes.TrimSpace(e.StructuredOutput))
	} else {
		o.result.Answer = e.Result
	}
	o.completed = true
}

func (o *jsonlOutput) recordPressureStop() {
	// Keep parsing cumulative counters; later results cannot erase this stop.
	o.taskFailure = errors.New("background work stopped under provider memory pressure")
	o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
}

func pressureStopSummary(summary string) bool {
	return strings.HasPrefix(summary, `Background command "`) && strings.HasSuffix(summary, `" was stopped because the system is running low on memory`)
}

func pressureStopNotice(raw json.RawMessage) bool {
	var text string
	return json.Unmarshal(raw, &text) == nil && pressureStopText(text)
}

func pressureStopText(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<task-notification>") {
		return false
	}
	var notice struct {
		XMLName xml.Name `xml:"task-notification"`
		Status  string   `xml:"status"`
		Summary string   `xml:"summary"`
	}
	return xml.Unmarshal([]byte(text), &notice) == nil && (notice.Status == "killed" || notice.Status == "stopped") && pressureStopSummary(notice.Summary)
}

func (o *jsonlOutput) recordAssistantUsage(raw json.RawMessage, root bool) {
	var message struct {
		ID    string          `json:"id"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &message) != nil || message.ID == "" {
		return
	}
	old, seen := o.assistantUsage[message.ID]
	if !seen {
		if o.assistantUsage == nil {
			o.assistantUsage = make(map[string]messageUsage)
		}
		old = messageUsage{generation: o.aggregateGeneration, root: root}
		// Remember IDs even before usable counters arrive. A later block from
		// an already covered message must not become a new billable call.
		o.assistantUsage[message.ID] = old
		o.uncovered = o.aggregateGeneration != 0
		if root && o.results != 0 {
			// A new root response starts another turn even if no background-task
			// notification exposed it. The preceding answer is provisional.
			o.completed, o.result.Answer = false, ""
		}
	}
	var rawUsage eventUsage
	if json.Unmarshal(message.Usage, &rawUsage) != nil {
		return
	}
	tokens, valid := rawUsage.tokens()
	if !valid {
		return
	}
	tokens = maxTokens(old.tokens, tokens)
	if !tokens.Valid() || tokens.Output > math.MaxInt64-tokens.Input {
		return
	}
	total, valid := sumTokens(subtractTokens(o.assistantTokens, old.tokens), tokens)
	if !valid {
		return
	}
	if old.root && old.generation == o.aggregateGeneration {
		rootTotal, valid := sumTokens(subtractTokens(o.rootTokens, old.tokens), tokens)
		if !valid {
			return
		}
		o.rootTokens = rootTotal
	}
	o.assistantTokens = total
	old.tokens = tokens
	o.assistantUsage[message.ID] = old
}

func (o *jsonlOutput) assistantLowerBound() (usage.Tokens, bool) {
	tokens := maxTokens(o.result.Tokens, o.assistantTokens)
	if o.aggregateGeneration != 0 {
		// Root responses follow the preceding result in the native stream.
		// Unknown/delegate messages may arrive late and are never added to it.
		root, valid := sumTokens(o.aggregateTokens, o.rootTokens)
		if !valid {
			return usage.Tokens{}, false
		}
		tokens = maxTokens(tokens, root)
	}
	return tokens, tokens.Valid() && tokens.Output <= math.MaxInt64-tokens.Input
}

func maxTokens(a, b usage.Tokens) usage.Tokens {
	return usage.Tokens{Input: max(a.Input, b.Input), CachedInput: max(a.CachedInput, b.CachedInput), Output: max(a.Output, b.Output)}
}

func subtractTokens(a, b usage.Tokens) usage.Tokens {
	return usage.Tokens{Input: a.Input - b.Input, CachedInput: a.CachedInput - b.CachedInput, Output: a.Output - b.Output}
}

// sumTokens adds normalized vectors; raw cache normalization and component
// maxima have different validity rules and remain checked at their boundaries.
func sumTokens(a, b usage.Tokens) (usage.Tokens, bool) {
	if !a.Valid() || !b.Valid() || b.Input > math.MaxInt64-a.Input || b.Output > math.MaxInt64-a.Output {
		return usage.Tokens{}, false
	}
	a.Add(b)
	return a, a.Output <= math.MaxInt64-a.Input
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
		if bytes.HasPrefix(line, []byte("apply-seccomp:")) || bytes.HasPrefix(line, []byte("bwrap:")) || bytes.HasPrefix(line, []byte("Sandbox is required but failed to initialize:")) {
			return true
		}
	}
	return false
}
