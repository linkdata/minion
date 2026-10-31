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

const maxResetTimestamp = 253402300799 // 9999-12-31T23:59:59Z, the last RFC3339 year.

const (
	maxProgressHistoryBytes   = 16 << 20
	maxProgressHistoryEntries = 100000
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
	SessionID         string            `json:"session_id"`
	UUID              string            `json:"uuid"`
	AgentID           string            `json:"agent_id"`
	Subtype           string            `json:"subtype"`
	ThinkingDelta     float64           `json:"estimated_tokens_delta"`
	ResultIndex       *int64            `json:"result_index"`
	TerminalReason    json.RawMessage   `json:"terminal_reason"`
	Tasks             json.RawMessage   `json:"tasks"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	Errors            json.RawMessage   `json:"errors"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	Usage             json.RawMessage   `json:"usage"`
	ModelUsage        json.RawMessage   `json:"modelUsage"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           json.RawMessage   `json:"message"`
	StreamEvent       json.RawMessage   `json:"event"`
	ParentToolUseID   json.RawMessage   `json:"parent_tool_use_id"`
	TaskID            string            `json:"task_id"`
	RunID             string            `json:"run_id"`
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

type progressStream struct {
	messageID string
	replay    bool
	tools     map[int]uint8 // 1 started, 2 stopped.
}

type taskProgressState struct {
	totalTokens int64
	toolUses    int64
}

type openTaskRuns struct {
	only     string
	many     map[string]struct{}
	finished bool
}

type jsonlOutput struct {
	sessionID           string
	apiKey              string
	onEvent             func(aicli.StreamEvent)
	onProgress          func()
	progressSeen        map[string]bool
	progressStreams     map[string]*progressStream
	streamSeen          map[string]bool
	streamSeenBytes     int
	progressTasks       map[string]bool
	runlessListRuns     map[string]string
	ambiguousOpen       map[string]bool
	taskProgress        map[string]taskProgressState
	openTaskRuns        map[string]openTaskRuns
	progressBytes       int64
	progressEntries     int
	progressByteLimit   int64 // Tests may lower the cumulative history limit.
	progressEntryLimit  int   // Tests may lower the cumulative history limit.
	identitySeen        bool
	baseline            usage.Tokens
	baselineUnknown     bool
	cumulative          usage.Tokens
	hasCumulative       bool
	cancel              context.CancelFunc
	stream              aicli.JSONL
	fatal               func()
	fatalGap            bool
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
	if o.stream.Line == nil {
		o.stream.Line, o.stream.Fail = o.finishLine, o.fail
	}
	return o.stream.Write(p)
}

func (o *jsonlOutput) fail(err error) {
	if o.err == nil {
		o.err = err
		o.completed, o.result.Answer = false, ""
		o.result.UsageComplete = false
		o.markFatalGap("output_stop")
		if o.cancel != nil {
			o.cancel()
		}
	}
}

func (o *jsonlOutput) markFatalGap(kind string) {
	if o.onEvent != nil && !o.fatalGap {
		o.fatalGap = true
		o.onEvent(aicli.StreamEvent{Kind: kind})
	}
}

func (o *jsonlOutput) observe(code, message string) {
	safe := o.safeDiagnosticText(message)
	if safe == "" {
		o.result.Diagnostics = aicli.AppendNativeDiagnostic(o.result.Diagnostics, "diagnostics_truncated", "")
		safe = "native diagnostic could not be safely displayed"
	}
	o.result.Diagnostics = aicli.AppendNativeDiagnostic(o.result.Diagnostics, code, safe)
}

func (o *jsonlOutput) safeResultDetail(detail string) string {
	if detail == "" {
		return ""
	}
	safe := o.safeDiagnosticText(detail)
	if safe == "" {
		o.result.Diagnostics = aicli.AppendNativeDiagnostic(o.result.Diagnostics, "diagnostics_truncated", "")
		return "native result detail could not be safely displayed"
	}
	return safe
}

func (o *jsonlOutput) observeResultDetail(code, detail string) {
	if detail != "" {
		o.result.Diagnostics = aicli.AppendNativeDiagnostic(o.result.Diagnostics, code, o.safeResultDetail(detail))
	}
}

func (o *jsonlOutput) safeDiagnosticText(detail string) string {
	redacted := aicli.RedactText(detail, o.apiKey)
	safe := aicli.RedactText(aicli.DiagnosticText(redacted), o.apiKey)
	if len(safe) > bounded.DiagnosticBytes {
		return ""
	}
	return safe
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
	for _, failure := range []error{o.usageLimit, o.taskFailure} {
		if failure != nil && !errors.Is(o.err, failure) {
			if o.err == nil {
				o.fail(failure)
			} else {
				o.err = errors.Join(failure, o.err)
			}
		}
	}
	if o.err == nil && o.pending != 0 {
		o.fail(errors.New("background tasks still running when Claude exited"))
	}
	if o.err == nil && o.sessionID != "" && !o.identitySeen {
		o.fail(errors.New("missing Claude conversation identity"))
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

func (o *jsonlOutput) acceptsSession(id string) bool {
	return id == "" || id == o.sessionID
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
	if e.Type == "result" || e.Type == "system" && e.Subtype == "init" {
		parent := bytes.TrimSpace(e.ParentToolUseID)
		if len(parent) != 0 && !bytes.Equal(parent, []byte("null")) {
			// A tagged helper is not this invocation's root conversation/result.
			return
		}
		if o.sessionID != "" && e.SessionID != "" {
			if e.SessionID != o.sessionID {
				o.fail(errors.New("claude conversation identity changed within one call"))
				return
			}
			o.identitySeen = true
		}
	}
	if e.Type == "assistant" {
		o.recordAssistantUsage(e.Message, e.ParentToolUseID)
	}
	o.recordProgress(e)
	if e.Type == "system" && e.Subtype == "task_notification" && (e.Status == "stopped" || e.Status == "killed") && !e.Ambient && o.acceptsSession(e.SessionID) {
		o.recordTaskStop(e.Status, e.Summary)
	}
	if e.Type == "rate_limit_event" && e.RateLimitInfo.Status == "rejected" && !e.RateLimitInfo.IsUsingOverage && !e.RateLimitInfo.OverageInUse {
		o.usageLimit = errors.New("provider usage limit reached")
		var reset int64
		// Only a validated UTC timestamp may enter the service diagnostic.
		if json.Unmarshal(e.RateLimitInfo.ResetsAt, &reset) == nil && reset > 0 && reset <= maxResetTimestamp {
			o.usageLimit = fmt.Errorf("provider usage limit reached; resets at %s", time.Unix(reset, 0).UTC().Format(time.RFC3339))
		}
		o.observe("rate_limit_rejected", o.usageLimit.Error())
		o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
		o.startFailureGrace()
	}
	if e.Type == "system" && e.Subtype == "background_tasks_changed" && !e.Ambient && o.acceptsSession(e.SessionID) {
		var tasks []struct {
			TaskID  string `json:"task_id"`
			RunID   string `json:"run_id"`
			Ambient bool   `json:"ambient"`
		}
		if json.Unmarshal(e.Tasks, &tasks) != nil || tasks == nil {
			o.fail(errors.New("invalid Claude background task list"))
			return
		}
		o.pending = 0
		active := make(map[string]bool)
		seenRunless := make(map[string]bool)
		staleFinishedIDs := make(map[string]bool)
		for _, task := range tasks {
			if !task.Ambient {
				o.pending++
				if task.TaskID != "" {
					runID := task.RunID
					if runID != "" && o.progressSeen["task_finish:"+taskRunKey(task.TaskID, runID)] {
						staleFinishedIDs[task.TaskID] = true
						o.pending-- // A replayed snapshot cannot reopen a finished native run.
						continue
					}
					if runID != "" && o.onProgress != nil {
						hadNative := o.progressSeen["task_native:"+task.TaskID]
						if !o.bindRunLessTaskStart(task.TaskID, runID) {
							o.bindRunLessTaskCompletion(task.TaskID, runID, hadNative)
						}
						if o.err != nil {
							return
						}
					}
					if runID == "" && o.onProgress != nil {
						seenRunless[task.TaskID] = true
						if o.progressSeen["task_ambiguous:"+task.TaskID] {
							continue
						}
						o.openTaskRun(task.TaskID)
						if o.openTaskRuns[task.TaskID].finished {
							o.markAmbiguousRunless(task.TaskID)
							continue
						}
						if alias := o.runlessListRuns[task.TaskID]; alias != "" {
							open := o.openTaskRun(task.TaskID)
							if (open != "" && open != alias) || o.openTaskRuns[task.TaskID].many != nil {
								o.markAmbiguousRunless(task.TaskID)
								continue
							}
							runID = alias
						} else if !o.progressTasks[task.TaskID] {
							runID = o.openTaskRun(task.TaskID)
							if runID == "" && o.openTaskRuns[task.TaskID].many != nil {
								o.markAmbiguousRunless(task.TaskID)
								continue
							}
							if runID != "" {
								o.rememberProgress("task_start:" + task.TaskID)
								if o.err != nil {
									return
								}
								if o.runlessListRuns == nil {
									o.runlessListRuns = make(map[string]string)
								}
								o.runlessListRuns[task.TaskID] = runID
							}
						}
					}
					active[taskRunKey(task.TaskID, runID)] = true
					if task.RunID != "" && o.onProgress != nil {
						o.rememberProgress("task_native:" + task.TaskID)
					}
				}
			}
		}
		for prior := range o.progressTasks {
			id, _, _ := strings.Cut(prior, "\x00")
			if o.progressSeen["task_ambiguous:"+id] && !o.progressSeen["task_finish:"+prior] && !active[prior] {
				active[prior] = true // An ambiguous run-less list cannot finish an identified run.
				continue
			}
			if staleFinishedIDs[id] && (prior == id || !o.progressSeen["task_finish:"+prior]) && !active[prior] {
				active[prior] = true // A stale entry cannot prove another run with this task ID finished.
				o.pending++
			}
		}
		if o.err != nil {
			return
		}
		o.pending = max(o.pending, len(active))
		o.recordTaskListProgress(active)
		if o.err != nil {
			return
		}
		for id := range o.runlessListRuns {
			if !seenRunless[id] && !active[taskRunKey(id, o.runlessListRuns[id])] {
				o.rememberProgress("task_finish:" + id)
				if o.err != nil {
					return
				}
				delete(o.runlessListRuns, id)
			}
		}
		if o.pending == 0 && o.hasAmbiguousOpenRun() {
			o.pending = 1 // A stale run-less snapshot cannot clear an unidentified native run.
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
		if e.IsSynthetic && !e.Ambient && o.acceptsSession(e.SessionID) && json.Unmarshal(content, &text) == nil {
			o.recordTaskNotice(text)
		}
		if len(content) != 0 && content[0] == '[' {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(content, &blocks); err != nil {
				o.fail(fmt.Errorf("invalid Claude tool response: %w", err))
				return
			}
			for _, content := range blocks {
				if e.IsSynthetic && !e.Ambient && o.acceptsSession(e.SessionID) && content.Type == "text" {
					o.recordTaskNotice(content.Text)
				}
			}
		}
	}
	o.captureMessage(e)
	if e.Type != "result" {
		return
	}
	tokens, complete := e.tokens()
	o.hasCumulative = false
	if complete {
		cumulative := tokens
		tokens = tokens.Sub(o.baseline)
		var rawRoot eventUsage
		root, rootValid := usage.Tokens{}, json.Unmarshal(e.Usage, &rawRoot) == nil
		if rootValid {
			root, rootValid = rawRoot.tokens()
		}
		// An exact snapshot must cover the prior cumulative floor plus all
		// attributable current counters before it can become the next baseline.
		lower, valid := o.assistantLowerBound()
		complete = tokens.Valid() && valid && covers(tokens, lower) && (!rootValid || covers(tokens, root))
		if complete {
			o.cumulative, o.hasCumulative = cumulative, true
		}
		if o.baselineUnknown || !tokens.Valid() {
			// An uncertain prior baseline cannot identify this invocation's delta.
			tokens, complete = usage.Tokens{}, false
		}
		if rootValid && !covers(tokens, root) {
			if merged := tokens.Max(root); merged.Valid() {
				tokens = merged
			} else {
				tokens = root
			}
			complete = false
		}
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
	if len(e.PermissionDenials) != 0 {
		message := fmt.Sprintf("%d native permission denials", len(e.PermissionDenials))
		var names []string
		for _, raw := range e.PermissionDenials {
			var denial struct {
				ToolName string `json:"tool_name"`
			}
			if json.Unmarshal(raw, &denial) == nil && denial.ToolName != "" {
				names = append(names, denial.ToolName)
			}
		}
		if len(names) != 0 {
			message += ": " + strings.Join(names, ", ")
		}
		o.observe("permission_denied", message)
	}
	var terminalReason string
	terminalValid := true
	if len(e.TerminalReason) != 0 {
		terminalValid = json.Unmarshal(e.TerminalReason, &terminalReason) == nil
		if !terminalValid {
			terminalReason = "invalid"
		} else if terminalReason == "" {
			terminalReason = string(bytes.TrimSpace(e.TerminalReason))
		}
		o.observe("terminal_reason", terminalReason)
	}
	var resultFailure string
	if e.IsError || e.Subtype != "success" {
		resultFailure = fmt.Sprintf("result failed: %q", bounded.Text(aicli.RedactText(e.Subtype, o.apiKey)))
		switch e.APIError {
		case "usage_limit_reached", "model_requires_usage_credits", "long_context_credits_required", "provider_credentials":
			resultFailure += "; API error=" + e.APIError
		}
		var status int
		if json.Unmarshal(e.APIErrorStatus, &status) == nil && status >= 100 && status <= 599 {
			resultFailure += fmt.Sprintf("; HTTP status=%d", status)
		}
		o.observe("result_error", resultFailure)
	}
	o.observeResultDetail("result_detail", e.Result)
	if raw := bytes.TrimSpace(e.Errors); len(raw) > 2 && raw[0] == '[' && len(bytes.TrimSpace(raw[1:len(raw)-1])) != 0 {
		o.observeResultDetail("result_errors", string(raw))
	}
	if o.structured {
		structured := bytes.TrimSpace(e.StructuredOutput)
		if len(structured) != 0 && !bytes.Equal(structured, []byte("null")) {
			o.observeResultDetail("structured_output_observed", string(structured))
		}
	}
	if failure := errors.Join(o.usageLimit, o.taskFailure); failure != nil {
		o.fail(failure)
		return
	}
	if resultFailure != "" {
		o.fail(errors.New(resultFailure))
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
		if !terminalValid || terminalReason != "completed" {
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

func (o *jsonlOutput) captureMessage(e event) {
	if o.onEvent == nil || len(e.Message) == 0 || e.Type != "assistant" && (e.Type != "user" || e.IsSynthetic) {
		return
	}
	kind := "assistant"
	if e.Type == "user" {
		kind = "user"
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &message) != nil {
		o.emitContent(e, kind, "")
		return
	}
	content := bytes.TrimSpace(message.Content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return
	}
	if content[0] == '"' {
		var text string
		if json.Unmarshal(content, &text) != nil {
			o.emitContent(e, kind, "")
		} else if text != "" {
			o.emitContent(e, kind, text)
		}
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		o.emitContent(e, kind, "")
		return
	}
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &block) != nil {
			o.emitContent(e, kind, "")
			continue
		}
		switch {
		case e.Type == "assistant" && block.Type == "text" && block.Text != "":
			o.emitContent(e, "assistant", block.Text)
		case e.Type == "assistant" && block.Type == "tool_use":
			o.emitContent(e, "tool_request", string(raw))
		case e.Type == "user" && block.Type == "tool_result":
			o.emitContent(e, "tool_result", string(raw))
		case e.Type == "user" && block.Type == "text" && block.Text != "":
			o.emitContent(e, "user", block.Text)
		case block.Type == "thinking" || block.Type == "redacted_thinking":
		default:
			o.emitContent(e, kind, "")
		}
	}
}

func (o *jsonlOutput) emitContent(e event, kind, text string) {
	if e.SessionID == "" || e.SessionID != o.sessionID {
		o.onEvent(aicli.StreamEvent{Kind: kind})
		return
	}
	parent := bytes.TrimSpace(e.ParentToolUseID)
	if bytes.Equal(parent, []byte("null")) {
		o.emitEventWithID(o.sessionID, kind, text)
		return
	}
	var helper string
	if json.Unmarshal(parent, &helper) == nil && helper != "" {
		o.emitEventWithID(e.AgentID, kind, text)
		return
	}
	o.emitEvent(kind, "") // Unknown parentage cannot prove root coverage.
}

func (o *jsonlOutput) emitEvent(kind, text string) {
	o.emitEventWithID(o.sessionID, kind, text)
}

func (o *jsonlOutput) emitEventWithID(id, kind, text string) {
	if o.onEvent == nil {
		return
	}
	if !o.identitySeen || id == "" {
		o.onEvent(aicli.StreamEvent{Kind: kind})
		return
	}
	redacted := aicli.RedactText(text, o.apiKey)
	if redacted == "" {
		o.onEvent(aicli.StreamEvent{Kind: kind})
		return
	}
	o.onEvent(aicli.StreamEvent{NativeID: id, Kind: kind, Text: redacted})
}

func (o *jsonlOutput) recordTaskStop(status, summary string) {
	// A new root response must assess an interrupted task before a later
	// successful result can certify completion. Ambient cleanup is ignored.
	o.observe("task_stopped", strings.TrimSpace(status+": "+summary))
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
	header, _, _ := strings.Cut(text, "<summary>")
	_, status, found := strings.Cut(header, "<status>")
	if !found {
		return
	}
	status, _, found = strings.Cut(status, "</status>")
	status = strings.TrimSpace(html.UnescapeString(status))
	if _, taskID, ok := strings.Cut(header, "<task-id>"); found && ok {
		if end := strings.Index(taskID, "</task-id>"); end >= 0 {
			id := strings.TrimSpace(html.UnescapeString(taskID[:end]))
			if id != "" && o.onProgress != nil && !o.progressSeen["task_ambiguous:"+id] {
				o.recordTaskProgress(id, "", status)
			}
		}
	}
	if !found || status != "killed" && status != "stopped" {
		return
	}
	_, summary, _ := strings.Cut(text, "<summary>")
	if end := strings.LastIndex(summary, "</summary>"); end >= 0 {
		summary = summary[:end]
	}
	o.recordTaskStop(status, html.UnescapeString(summary))
}

// Progress is native execution, including task usage changes, but never
// result-only usage, result waiting, or repeated snapshots. The callback is
// separate from transcript capture and sends no text.
func (o *jsonlOutput) recordProgress(e event) {
	if e.Ambient || !o.acceptsSession(e.SessionID) {
		return
	}
	if e.Type == "stream_event" {
		o.recordStreamProgress(e)
		return
	}
	if o.onProgress == nil {
		return
	}
	switch e.Type {
	case "assistant":
		var message struct {
			ID      string          `json:"id"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(e.Message, &message) != nil || message.ID == "" {
			return
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			ID   string `json:"id"`
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil && text != "" {
			o.recordUniqueProgress("assistant:" + message.ID)
			return
		}
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "text" && block.Text != "" || block.Type == "tool_use" && block.ID != "" {
					o.recordUniqueProgress("assistant:" + message.ID)
					return
				}
			}
		}
	case "user":
		if e.IsSynthetic {
			return
		}
		var message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		}
		if json.Unmarshal(e.Message, &message) == nil {
			for _, block := range message.Content {
				if block.Type == "tool_result" && block.ToolUseID != "" {
					o.recordUniqueProgress("tool_result:" + block.ToolUseID)
				}
			}
		}
	case "system":
		switch e.Subtype {
		case "task_started":
			o.recordTaskProgress(e.TaskID, e.RunID, "started")
		case "task_notification":
			o.recordTaskProgress(e.TaskID, e.RunID, e.Status)
		case "task_progress":
			o.recordTaskUsageProgress(e)
		case "thinking_tokens":
			if o.sessionID != "" && e.SessionID == o.sessionID && e.UUID != "" && e.ThinkingDelta > 0 {
				o.recordUniqueProgress("thinking_tokens:" + e.UUID)
			}
		}
	}
}

func (o *jsonlOutput) recordStreamProgress(e event) {
	var native struct {
		Type    string `json:"type"`
		Index   *int   `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(e.StreamEvent, &native) != nil {
		return
	}
	key := "root"
	if e.AgentID != "" {
		key = "agent:" + e.AgentID
	} else if parent := bytes.TrimSpace(e.ParentToolUseID); len(parent) != 0 && !bytes.Equal(parent, []byte("null")) {
		key = "parent:" + string(parent)
	}
	if o.progressStreams == nil {
		o.progressStreams = make(map[string]*progressStream)
	}
	state := o.progressStreams[key]
	if native.Type == "message_start" {
		var parentID string
		helper := e.AgentID != "" || json.Unmarshal(e.ParentToolUseID, &parentID) == nil && parentID != ""
		if native.Message.ID == "" {
			delete(o.progressStreams, key)
			if o.results != 0 && !helper {
				o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
			}
			return
		}
		startKey := "stream_start:" + native.Message.ID
		_, assistantSeen := o.assistantUsage[native.Message.ID]
		replay := assistantSeen || o.progressSeen["assistant:"+native.Message.ID] || o.progressSeen[startKey] || o.streamSeen[native.Message.ID]
		if !replay {
			// Without a callback, cap replay memory; missed IDs fail closed as fresh work.
			if o.onProgress != nil {
				if !o.rememberProgress(startKey) {
					return
				}
			} else if len(o.streamSeen) < maxProgressHistoryEntries && len(native.Message.ID) <= maxProgressHistoryBytes-o.streamSeenBytes {
				if o.streamSeen == nil {
					o.streamSeen = make(map[string]bool)
				}
				o.streamSeen[native.Message.ID] = true
				o.streamSeenBytes += len(native.Message.ID)
			}
			if o.results != 0 && !helper {
				o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
			}
		}
		state = &progressStream{messageID: native.Message.ID, replay: replay}
		o.progressStreams[key] = state
		return
	}
	if native.Type == "message_stop" {
		delete(o.progressStreams, key)
		return
	}
	if state == nil {
		return // A delta without a native message identity could be replayed.
	}
	if state.replay {
		return
	}
	switch native.Type {
	case "content_block_start":
		if native.Index == nil || native.ContentBlock.Type != "tool_use" {
			return
		}
		if state.tools[*native.Index] != 0 {
			return
		}
		if state.tools == nil {
			state.tools = make(map[int]uint8)
		}
		state.tools[*native.Index] = 1
		o.markStreamProgress(state)
	case "content_block_delta":
		switch native.Delta.Type {
		case "text_delta":
			if native.Delta.Text != "" {
				o.markStreamProgress(state)
			}
		case "thinking_delta":
			if native.Delta.Thinking != "" {
				o.markStreamProgress(state)
			}
		case "input_json_delta":
			if native.Delta.PartialJSON != "" {
				o.markStreamProgress(state)
			}
		}
	case "content_block_stop":
		if native.Index != nil && state.tools[*native.Index] == 1 {
			state.tools[*native.Index] = 2
			o.markStreamProgress(state)
		}
	}
}

func (o *jsonlOutput) markStreamProgress(state *progressStream) {
	if o.onProgress == nil {
		return
	}
	key := "assistant:" + state.messageID
	if !o.progressSeen[key] && !o.reserveProgress(1, len(key)) {
		return
	}
	if o.progressSeen == nil {
		o.progressSeen = make(map[string]bool)
	}
	o.progressSeen[key] = true
	o.onProgress()
}

func (o *jsonlOutput) recordUniqueProgress(key string) {
	if o.rememberProgress(key) {
		o.onProgress()
	}
}

func (o *jsonlOutput) rememberProgress(key string) bool {
	if o.progressSeen[key] {
		return false
	}
	if !o.reserveProgress(1, len(key)) {
		return false
	}
	if o.progressSeen == nil {
		o.progressSeen = make(map[string]bool)
	}
	o.progressSeen[key] = true
	if o.openTaskRuns != nil {
		if task, ok := strings.CutPrefix(key, "task_start:"); ok {
			if id, runID, ok := strings.Cut(task, "\x00"); ok {
				o.addOpenTaskRun(id, runID)
			}
		} else if task, ok := strings.CutPrefix(key, "task_finish:"); ok {
			if id, runID, ok := strings.Cut(task, "\x00"); ok {
				o.removeOpenTaskRun(id, runID)
			}
		}
	}
	return true
}

func (o *jsonlOutput) reserveProgress(entries, bytes int) bool {
	entryLimit, byteLimit := maxProgressHistoryEntries, int64(maxProgressHistoryBytes)
	if o.progressEntryLimit > 0 {
		entryLimit = min(entryLimit, o.progressEntryLimit)
	}
	if o.progressByteLimit > 0 {
		byteLimit = min(byteLimit, o.progressByteLimit)
	}
	if o.err != nil || entries > entryLimit-o.progressEntries || int64(bytes) > byteLimit-o.progressBytes {
		o.fail(fmt.Errorf("%w: Claude progress history limit reached", aicli.ErrOutputLimit))
		return false
	}
	o.progressEntries += entries
	o.progressBytes += int64(bytes)
	return true
}

func taskRunKey(id, runID string) string {
	if runID == "" {
		return id
	}
	return id + "\x00" + runID
}

func (o *jsonlOutput) openTaskRun(id string) string {
	if o.openTaskRuns == nil {
		// Build once from bounded history when the first run-less notice arrives.
		o.openTaskRuns = make(map[string]openTaskRuns)
		for key := range o.progressSeen {
			if task, ok := strings.CutPrefix(key, "task_start:"); ok {
				if taskID, runID, ok := strings.Cut(task, "\x00"); ok {
					o.addOpenTaskRun(taskID, runID)
				}
			} else if task, ok := strings.CutPrefix(key, "task_finish:"); ok {
				if taskID, _, ok := strings.Cut(task, "\x00"); ok {
					runs := o.openTaskRuns[taskID]
					runs.finished = true
					o.openTaskRuns[taskID] = runs
				}
			}
		}
		for key := range o.taskProgress {
			if taskID, runID, ok := strings.Cut(key, "\x00"); ok {
				o.addOpenTaskRun(taskID, runID)
			}
		}
	}
	runs := o.openTaskRuns[id]
	if runs.many != nil {
		return "" // More than one unfinished run is ambiguous.
	}
	return runs.only
}

func (o *jsonlOutput) hasAmbiguousOpenRun() bool {
	if o.ambiguousOpen == nil {
		o.openTaskRun("") // Rebuild the bounded native-run index after resume.
		o.ambiguousOpen = make(map[string]bool)
		for id, runs := range o.openTaskRuns {
			if o.progressSeen["task_ambiguous:"+id] && (runs.only != "" || len(runs.many) != 0) {
				o.ambiguousOpen[id] = true
			}
		}
	}
	return len(o.ambiguousOpen) != 0
}

func (o *jsonlOutput) addOpenTaskRun(id, runID string) {
	if runID == "" || o.progressSeen["task_finish:"+taskRunKey(id, runID)] {
		return
	}
	runs := o.openTaskRuns[id]
	if runs.many != nil {
		runs.many[runID] = struct{}{}
	} else if runs.only == "" {
		runs.only = runID
		o.openTaskRuns[id] = runs
	} else if runs.only != runID {
		runs.many = map[string]struct{}{runs.only: {}, runID: {}}
		runs.only = ""
		o.openTaskRuns[id] = runs
	}
	if o.progressSeen["task_ambiguous:"+id] {
		if o.ambiguousOpen == nil {
			o.ambiguousOpen = make(map[string]bool)
		}
		o.ambiguousOpen[id] = true
	}
}

func (o *jsonlOutput) removeOpenTaskRun(id, runID string) {
	runs := o.openTaskRuns[id]
	runs.finished = true
	if runs.many != nil {
		delete(runs.many, runID)
		if len(runs.many) == 1 {
			for remaining := range runs.many {
				runs.only = remaining
			}
			runs.many = nil
		} else if len(runs.many) == 0 {
			runs.many = nil
		}
	} else if runs.only == runID {
		runs.only = ""
	}
	o.openTaskRuns[id] = runs
	if runs.only == "" && len(runs.many) == 0 {
		delete(o.ambiguousOpen, id)
	}
}

func (o *jsonlOutput) markAmbiguousRunless(id string) {
	if !o.rememberProgress("task_ambiguous:" + id) {
		return
	}
	o.openTaskRun(id)
	runs := o.openTaskRuns[id]
	if runs.only != "" || len(runs.many) != 0 {
		if o.ambiguousOpen == nil {
			o.ambiguousOpen = make(map[string]bool)
		}
		o.ambiguousOpen[id] = true
	}
}

func (o *jsonlOutput) bindRunLessTaskStart(id, runID string) bool {
	key := taskRunKey(id, runID)
	if !o.progressSeen["task_start:"+id] || o.progressSeen["task_finish:"+id] ||
		o.progressSeen["task_ambiguous:"+id] || o.progressSeen["task_finish:"+key] {
		return false
	}
	open := o.openTaskRun(id)
	if (open != "" && open != runID) || o.openTaskRuns[id].many != nil {
		o.markAmbiguousRunless(id)
		return false
	}
	o.rememberProgress("task_start:" + key)
	if o.err != nil {
		return true
	}
	delete(o.progressTasks, id)
	if o.progressTasks == nil {
		o.progressTasks = make(map[string]bool)
	}
	o.progressTasks[key] = true
	if o.runlessListRuns == nil {
		o.runlessListRuns = make(map[string]string)
	}
	o.runlessListRuns[id] = runID
	return true
}

func (o *jsonlOutput) bindRunLessTaskCompletion(id, runID string, hadNative bool) bool {
	key := taskRunKey(id, runID)
	start, finish := "task_start:"+key, "task_finish:"+key
	match := "task_synthetic_match:" + id
	// A previously identified run makes a later run-less finish ambiguous.
	if hadNative || !o.progressSeen["task_finish:"+id] || o.progressSeen["task_ambiguous:"+id] ||
		o.progressSeen[match] || o.progressSeen[start] || o.progressSeen[finish] {
		return false
	}
	// A run-less completion can identify one native run; preserve both native
	// lifecycle identities so either replay order stays silent.
	if o.reserveProgress(3, len(start)+len(finish)+len(match)) {
		o.progressSeen[start], o.progressSeen[finish], o.progressSeen[match] = true, true, true
		if o.openTaskRuns != nil {
			o.removeOpenTaskRun(id, runID)
		}
	}
	return true
}

func (o *jsonlOutput) recordTaskProgress(id, runID, status string) {
	if o.onProgress == nil || id == "" {
		return
	}
	switch status {
	case "started", "completed", "failed", "stopped", "killed":
	default:
		return
	}
	hadNative := o.progressSeen["task_native:"+id]
	if runID == "" {
		if o.progressSeen["task_ambiguous:"+id] {
			return
		}
		o.openTaskRun(id)
		if o.openTaskRuns[id].finished {
			o.markAmbiguousRunless(id)
			return
		}
		if status != "started" {
			if alias := o.runlessListRuns[id]; alias != "" {
				open := o.openTaskRun(id)
				if (open != "" && open != alias) || o.openTaskRuns[id].many != nil {
					o.markAmbiguousRunless(id)
					return
				}
				runID = alias
			} else if hadNative {
				runID = o.openTaskRun(id)
				if runID == "" {
					return // A run-less notice cannot identify an open native run.
				}
			}
		}
	}
	if runID != "" {
		// Native identity lets later run-less replay notices be ignored after
		// the run finishes.
		o.rememberProgress("task_native:" + id)
		if o.err != nil {
			return
		}
	}
	switch status {
	case "started":
		key := taskRunKey(id, runID)
		if runID != "" && o.progressSeen["task_finish:"+key] {
			return // A delayed start for an already finished native run is replay.
		}
		if runID != "" && o.runlessListRuns[id] != "" && o.runlessListRuns[id] != runID {
			o.markAmbiguousRunless(id)
		}
		if runID != "" && (o.bindRunLessTaskStart(id, runID) || o.bindRunLessTaskCompletion(id, runID, hadNative)) {
			return
		}
		if o.err != nil {
			return
		}
		o.recordUniqueProgress("task_start:" + key)
	case "completed", "failed", "stopped", "killed":
		key := taskRunKey(id, runID)
		finish := "task_finish:" + key
		if runID != "" {
			o.bindRunLessTaskStart(id, runID) // A native finish may be the first identified event for an active list entry.
			if o.err != nil || o.bindRunLessTaskCompletion(id, runID, hadNative) {
				return
			}
		}
		o.recordUniqueProgress(finish)
		if o.err == nil && runID != "" && o.runlessListRuns[id] == runID && !o.progressSeen["task_ambiguous:"+id] {
			o.rememberProgress("task_finish:" + id)
		}
	}
}

func (o *jsonlOutput) recordTaskUsageProgress(e event) {
	if e.TaskID == "" {
		return
	}
	if e.RunID == "" && o.progressSeen["task_native:"+e.TaskID] {
		o.markAmbiguousRunless(e.TaskID)
		return
	}
	if e.RunID != "" {
		o.rememberProgress("task_native:" + e.TaskID)
		if o.err != nil {
			return
		}
	}
	var usage struct {
		TotalTokens *int64 `json:"total_tokens"`
		ToolUses    *int64 `json:"tool_uses"`
	}
	if json.Unmarshal(e.Usage, &usage) != nil {
		usage.TotalTokens, usage.ToolUses = nil, nil
	}
	key := taskRunKey(e.TaskID, e.RunID)
	state, knownTask := o.taskProgress[key]
	changed := false
	if usage.TotalTokens != nil && *usage.TotalTokens > state.totalTokens {
		state.totalTokens = *usage.TotalTokens
		changed = true
	}
	if usage.ToolUses != nil && *usage.ToolUses > state.toolUses {
		state.toolUses = *usage.ToolUses
		changed = true
	}
	if changed {
		entries, bytes := 0, 0
		if !knownTask {
			entries, bytes = 1, len(key)
		}
		if !o.reserveProgress(entries, bytes) {
			return
		}
		if o.taskProgress == nil {
			o.taskProgress = make(map[string]taskProgressState)
		}
		o.taskProgress[key] = state
		if !knownTask && o.openTaskRuns != nil && e.RunID != "" {
			o.addOpenTaskRun(e.TaskID, e.RunID)
		}
		o.onProgress()
	}
}

func (o *jsonlOutput) recordTaskListProgress(active map[string]bool) {
	if o.onProgress == nil {
		return
	}
	for id := range active {
		if !o.progressTasks[id] {
			o.recordUniqueProgress("task_start:" + id)
		}
	}
	for id := range o.progressTasks {
		if !active[id] {
			o.recordUniqueProgress("task_finish:" + id)
		}
	}
	o.progressTasks = active
}

func (o *jsonlOutput) recordAssistantUsage(raw, parent json.RawMessage) {
	var message struct {
		ID    string          `json:"id"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &message) != nil || message.ID == "" {
		o.hasCumulative = false
		o.uncovered = o.aggregateGeneration != 0
		if o.results != 0 {
			o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
		}
		return
	}
	root := bytes.Equal(bytes.TrimSpace(parent), []byte("null"))
	var parentID string
	unknown := !root && (json.Unmarshal(parent, &parentID) != nil || parentID == "")
	old, seen := o.assistantUsage[message.ID]
	if o.baselineUnknown || !seen {
		o.hasCumulative = false
	}
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
		if (root || unknown) && o.results != 0 {
			// Unknown parentage cannot prove a delayed delegate; require a new
			// result without treating it as a root assessment of stopped work.
			o.completed, o.result.Answer = false, ""
		}
	}
	markUsageUncertain := func() {
		if o.aggregateGeneration != 0 {
			// A post-result block may revise previously covered counters. If its
			// usage cannot be incorporated, the result no longer proves coverage.
			o.hasCumulative, o.uncovered, o.result.UsageComplete = false, true, false
		}
	}
	var rawUsage eventUsage
	valid := json.Unmarshal(message.Usage, &rawUsage) == nil
	var tokens usage.Tokens
	if valid {
		tokens, valid = rawUsage.tokens()
	}
	if !valid {
		markUsageUncertain()
		return
	}
	tokens = old.tokens.Max(tokens)
	if !tokens.Valid() {
		markUsageUncertain()
		return
	}
	total, valid := o.assistantTokens.Sub(old.tokens).AddChecked(tokens)
	if !valid {
		markUsageUncertain()
		return
	}
	if old.root && old.generation == o.aggregateGeneration {
		rootTotal, valid := o.rootTokens.Sub(old.tokens).AddChecked(tokens)
		if !valid {
			markUsageUncertain()
			return
		}
		o.rootTokens = rootTotal
	}
	o.assistantTokens = total
	old.tokens = tokens
	o.assistantUsage[message.ID] = old
	if o.hasCumulative {
		lower, valid := o.assistantLowerBound()
		o.hasCumulative = valid && covers(o.cumulative.Sub(o.baseline), lower)
	}
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

func isWaitCeilingStop(line []byte) bool {
	// Managed CLI settings can override its launch environment. Match this
	// diagnostic independently of the separately retained stderr tail.
	line = bytes.TrimRight(line, " \t\r")
	return bytes.HasPrefix(line, []byte("Background tasks still running ")) && bytes.HasSuffix(line, []byte("; stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."))
}
