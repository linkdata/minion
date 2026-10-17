package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/usage"
)

const (
	maxEventBytes     = aicli.MaxEventBytes
	maxResetTimestamp = 253402300799 // 9999-12-31T23:59:59Z, the last RFC3339 year.
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
	valid = tokens.Valid()
	return
}

type event struct {
	Type              string            `json:"type"`
	Subtype           string            `json:"subtype"`
	ResultIndex       *int64            `json:"result_index"`
	TerminalReason    json.RawMessage   `json:"terminal_reason"`
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
	Ambient           bool              `json:"ambient"`
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
			if total, ok = total.AddChecked(tokens); !ok {
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
	stream              aicli.JSONL
	fatal               func()
	stopped             bool
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
	if o.err != nil {
		return len(p), nil
	}
	return o.stream.Write(p, o.finishLine, o.fail)
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
	o.stream.Finish()
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

func (o *jsonlOutput) finishLine(line []byte) {
	if o.err != nil {
		return
	}
	var e event
	if err := json.Unmarshal(line, &e); err != nil {
		o.fail(fmt.Errorf("invalid Claude JSONL event: %w", err))
		return
	}
	if e.Type == "assistant" {
		o.recordAssistantUsage(e.Message, bytes.Equal(bytes.TrimSpace(e.ParentToolUseID), []byte("null")))
	}
	if e.Type == "system" && e.Subtype == "task_notification" && (e.Status == "stopped" || e.Status == "killed") && !e.Ambient {
		o.recordTaskStop(e.Summary)
	}
	if e.Type == "rate_limit_event" && e.RateLimitInfo.Status == "rejected" && !e.RateLimitInfo.IsUsingOverage && !e.RateLimitInfo.OverageInUse {
		o.usageLimit = errors.New("provider usage limit reached")
		var reset int64
		// Only a validated UTC timestamp may enter the service diagnostic.
		if json.Unmarshal(e.RateLimitInfo.ResetsAt, &reset) == nil && reset > 0 && reset <= maxResetTimestamp {
			o.usageLimit = fmt.Errorf("provider usage limit reached; resets at %s", time.Unix(reset, 0).UTC().Format(time.RFC3339))
		}
		o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
		o.startFailureGrace()
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
		var text string
		if e.IsSynthetic && !e.Ambient && json.Unmarshal(content, &text) == nil {
			o.recordTaskNotice(text)
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
				if e.IsSynthetic && !e.Ambient && content.Type == "text" {
					o.recordTaskNotice(content.Text)
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
		complete = valid && covers(tokens, lower)
	}
	// Native modelUsage is cumulative across result turns. A later partial
	// report must not erase previously recorded usage.
	if covers(tokens, o.result.Tokens) {
		o.result.Tokens = tokens
	} else {
		if lower := o.result.Tokens.Max(tokens); lower.Valid() {
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
	if failure := errors.Join(o.usageLimit, o.taskFailure); failure != nil {
		o.fail(failure)
		return
	}
	if e.IsError || e.Subtype != "success" {
		reason := fmt.Sprintf("result failed: %q", bounded.Text(e.Subtype))
		switch e.APIError {
		case "usage_limit_reached", "model_requires_usage_credits", "long_context_credits_required", "provider_credentials":
			reason += "; API error=" + e.APIError
		}
		var status int
		if json.Unmarshal(e.APIErrorStatus, &status) == nil && status >= 100 && status <= 599 {
			reason += fmt.Sprintf("; HTTP status=%d", status)
		}
		o.fail(errors.New(reason))
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
	if len(e.TerminalReason) != 0 {
		var reason string
		if json.Unmarshal(e.TerminalReason, &reason) != nil || reason != "completed" {
			o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
			return
		}
	}
	// Print mode can emit provisional turn results while background work runs.
	if o.pending != 0 || o.stopped {
		return
	}
	if o.structured {
		o.result.Answer = string(bytes.TrimSpace(e.StructuredOutput))
	} else {
		o.result.Answer = e.Result
	}
	o.completed = true
}

func (o *jsonlOutput) recordTaskStop(summary string) {
	// A new root response must assess an interrupted task before a later
	// successful result can certify completion. Ambient cleanup is ignored.
	o.stopped = true
	o.completed, o.result.Answer = false, ""
	if pressureStopSummary(summary) {
		// Keep parsing cumulative counters; later results cannot erase this stop.
		o.taskFailure = errors.New("background work stopped under provider memory pressure")
		o.result.UsageComplete = false
		o.startFailureGrace()
	}
}

func (o *jsonlOutput) startFailureGrace() {
	if o.fatal != nil {
		o.fatal()
		o.fatal = nil
	}
}

func pressureStopSummary(summary string) bool {
	return strings.HasPrefix(summary, `Background command "`) && strings.HasSuffix(summary, `" was stopped because the system is running low on memory`)
}

func (o *jsonlOutput) recordTaskNotice(text string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<task-notification>") {
		return
	}
	_, status, found := strings.Cut(text, "<status>")
	if !found {
		return
	}
	status, _, found = strings.Cut(status, "</status>")
	status = strings.TrimSpace(html.UnescapeString(status))
	if !found || status != "killed" && status != "stopped" {
		return
	}
	_, summary, _ := strings.Cut(text, "<summary>")
	if end := strings.LastIndex(summary, "</summary>"); end >= 0 {
		summary = summary[:end]
	}
	o.recordTaskStop(html.UnescapeString(summary))
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
		if root {
			o.stopped = false
		}
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
	tokens = old.tokens.Max(tokens)
	if !tokens.Valid() {
		return
	}
	total, valid := o.assistantTokens.Sub(old.tokens).AddChecked(tokens)
	if !valid {
		return
	}
	if old.root && old.generation == o.aggregateGeneration {
		rootTotal, valid := o.rootTokens.Sub(old.tokens).AddChecked(tokens)
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
	tokens := o.result.Tokens.Max(o.assistantTokens)
	if o.aggregateGeneration != 0 {
		// Root responses follow the preceding result in the native stream.
		// Unknown/delegate messages may arrive late and are never added to it.
		root, valid := o.aggregateTokens.AddChecked(o.rootTokens)
		if !valid {
			return usage.Tokens{}, false
		}
		tokens = tokens.Max(root)
	}
	return tokens, tokens.Valid()
}

func covers(a, b usage.Tokens) bool { return a.Max(b) == a }

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

func isWaitCeilingStop(line []byte) bool {
	// Managed CLI settings can override its launch environment. Match this
	// diagnostic independently of the separately retained stderr tail.
	line = bytes.TrimRight(line, " \t\r")
	return bytes.HasPrefix(line, []byte("Background tasks still running ")) && bytes.HasSuffix(line, []byte("; stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."))
}
