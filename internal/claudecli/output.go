package claudecli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	tokens       usage.Tokens
	prior        usage.Tokens
	priorMissing bool
	generation   int64
	root         bool
}

type assistantSnapshot struct {
	tokens    usage.Tokens
	hasTokens bool
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

type jsonlOutput struct {
	sessionID              string
	apiKey                 string
	onEvent                func(aicli.StreamEvent)
	onProgress             func()
	progressSeen           map[string]bool
	progressStreams        map[string]*progressStream
	streamedSinceAssistant map[string]bool
	rootStartsAfterStop    map[string]bool
	taskProgress           map[string]taskProgressState
	taskRuns               map[string]bool
	taskRunCounts          map[string]int
	runlessPending         map[string]int
	runlessExplicit        map[string]bool // Presence records a start or usage; true means a start was seen.
	runlessAmbiguous       map[string]bool
	runlessCount           int
	anonymousPending       int
	progressBytes          int64
	progressEntries        int
	progressByteLimit      int64 // Tests may lower the cumulative history limit.
	progressEntryLimit     int   // Tests may lower the cumulative history limit.
	identitySeen           bool
	baseline               usage.Tokens
	baselineUnknown        bool
	cumulative             usage.Tokens
	hasCumulative          bool
	cancel                 context.CancelFunc
	stream                 aicli.JSONL
	fatal                  func()
	fatalGap               bool
	stopped                bool
	err                    error
	usageLimit             error
	taskFailure            error
	completed              bool
	structured             bool
	pending                int
	results                int64
	indexed                bool
	result                 aicli.Result
	aggregateGeneration    int64
	aggregateTokens        usage.Tokens
	assistantUsage         map[string]messageUsage
	assistantHistory       map[string]assistantSnapshot
	assistantTokens        usage.Tokens
	rootTokens             usage.Tokens
	uncovered              bool
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
	o.updateTaskPending()
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
	freshAssistantContent, streamedAssistant, assistantID := false, false, ""
	if e.Type == "assistant" && !e.Ambient && o.acceptsSession(e.SessionID) {
		freshAssistantContent, assistantID = o.recordAssistantUsage(e.Message, e.ParentToolUseID)
		streamedAssistant = o.streamedSinceAssistant[assistantID]
		delete(o.streamedSinceAssistant, assistantID)
	}
	stop := e.Type == "system" && e.Subtype == "task_notification" &&
		(e.Status == "stopped" || e.Status == "killed") && !e.Ambient && o.acceptsSession(e.SessionID)
	if stop && !o.acceptTaskStop(e.TaskID, e.RunID, e.UUID, e.Summary) {
		if e.TaskID != "" && e.RunID != "" {
			// A duplicate notice can still carry the tagged run's only finish.
			o.recordTaskProgress(e.TaskID, e.RunID, e.Status, e.UUID)
		}
		return
	}
	o.recordProgress(e, freshAssistantContent, streamedAssistant, assistantID)
	if stop {
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
		// Native list UUIDs identify exact replays. A list with no UUID can add
		// pending work, but its disappearance cannot prove that work stopped.
		if e.UUID != "" && !o.rememberProgress("task_list_uuid:"+e.UUID) {
			return
		}
		if o.err != nil {
			return
		}
		listed := make(map[string]int)
		anonymous := 0
		for _, task := range tasks {
			if task.Ambient {
				continue
			}
			switch {
			case task.TaskID == "":
				anonymous++
			case task.RunID == "":
				listed[task.TaskID]++
				if _, explicit := o.runlessExplicit[task.TaskID]; explicit {
					// This row may describe a second task with the same ID.
					o.markRunlessAmbiguous(task.TaskID)
				}
			default:
				o.observeTaskRun(task.TaskID, task.RunID, "task_seen:", false)
				if o.err != nil {
					return
				}
			}
		}
		if e.UUID == "" {
			for id, count := range o.runlessPending {
				listed[id] = max(listed[id], count)
			}
			anonymous = max(anonymous, o.anonymousPending)
		}
		// An ID-only start or usage is independent evidence of pending work.
		// A later list may be stale even when it has a different UUID.
		for id := range o.runlessExplicit {
			listed[id] = max(listed[id], 1)
		}
		o.runlessPending = listed
		o.runlessCount = 0
		o.anonymousPending = anonymous
		for id, count := range o.runlessPending {
			o.runlessCount += count
			if count > 1 {
				o.markRunlessAmbiguous(id)
			}
		}
		for id := range o.runlessAmbiguous {
			if o.runlessPending[id] == 0 {
				delete(o.runlessAmbiguous, id)
			}
		}
		o.updateTaskPending()
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
			o.recordTaskNotice(text, e.UUID)
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
			synthetic := e.IsSynthetic && !e.Ambient && o.acceptsSession(e.SessionID)
			claimed := false
			var seenNotices map[string]bool
			for _, content := range blocks {
				notice := strings.TrimSpace(content.Text)
				if !synthetic || content.Type != "text" || !strings.HasPrefix(notice, "<task-notification>") {
					continue
				}
				if !claimed && e.UUID != "" {
					// The UUID identifies replay of the array, not each task inside it.
					if !o.rememberProgress("task_notice_uuid:" + e.UUID) {
						return
					}
					if o.err != nil {
						return
					}
					claimed = true
				}
				if seenNotices[notice] {
					continue
				}
				if seenNotices == nil {
					seenNotices = make(map[string]bool)
				}
				seenNotices[notice] = true
				o.recordTaskNotice(notice, "")
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
	o.updateTaskPending()
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
	o.rootStartsAfterStop = nil
	o.completed, o.result.Answer = false, ""
	if pressureStopSummary(summary) {
		// Keep parsing cumulative counters; later results cannot erase this stop.
		o.taskFailure = errors.New("background work stopped under provider memory pressure")
		o.result.UsageComplete = false
		o.startFailureGrace()
	}
}

func (o *jsonlOutput) acceptTaskStop(id, runID, uuid, summary string) bool {
	var runKey string
	if id != "" && runID != "" {
		runKey = "task_stop_run:" + taskRunKey(id, runID)
	}
	uuidSeen := uuid != "" && o.progressSeen["task_notice_uuid:"+uuid]
	runSeen := runKey != "" && o.progressSeen[runKey]
	// A later memory-pressure stop remains fatal even if an ordinary stop
	// already used this identity.
	if (uuidSeen || runSeen) && !(pressureStopSummary(summary) && o.taskFailure == nil) {
		if uuidSeen && !runSeen && runKey != "" {
			// Link the native run to the shared UUID for later replay suppression.
			o.rememberProgress(runKey)
		}
		return false
	}
	if uuid != "" && !uuidSeen && !o.rememberProgress("task_notice_uuid:"+uuid) {
		return false
	}
	if runKey != "" && !runSeen {
		o.rememberProgress(runKey)
	}
	return o.err == nil
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

func (o *jsonlOutput) recordTaskNotice(text, noticeUUID string) {
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
	if !found {
		return
	}
	status = strings.TrimSpace(html.UnescapeString(status))
	var id string
	if _, taskID, ok := strings.Cut(header, "<task-id>"); ok {
		if end := strings.Index(taskID, "</task-id>"); end >= 0 {
			id = strings.TrimSpace(html.UnescapeString(taskID[:end]))
		}
	}
	stop := status == "killed" || status == "stopped"
	var summary string
	if stop {
		_, summary, _ = strings.Cut(text, "<summary>")
		if end := strings.LastIndex(summary, "</summary>"); end >= 0 {
			summary = summary[:end]
		}
		summary = html.UnescapeString(summary)
		if !o.acceptTaskStop(id, "", noticeUUID, summary) {
			return
		}
	}
	if id != "" {
		o.recordTaskProgress(id, "", status, noticeUUID)
	}
	if stop {
		o.recordTaskStop(status, summary)
	}
}

// Progress is native execution, including task usage changes, but never
// result-only usage, result waiting, or repeated snapshots. The callback is
// separate from transcript capture and sends no text.
func (o *jsonlOutput) recordProgress(e event, freshAssistantContent, streamedAssistant bool, assistantID string) {
	if e.Ambient || !o.acceptsSession(e.SessionID) {
		return
	}
	if e.Type == "stream_event" {
		o.recordStreamProgress(e)
		return
	}
	if e.Type == "system" {
		switch e.Subtype {
		case "task_started":
			o.recordTaskProgress(e.TaskID, e.RunID, "started", e.UUID)
			return
		case "task_notification":
			o.recordTaskProgress(e.TaskID, e.RunID, e.Status, e.UUID)
			return
		case "task_progress":
			o.recordTaskUsageProgress(e)
			return
		}
	}
	if o.onProgress == nil {
		return
	}
	switch e.Type {
	case "assistant":
		if freshAssistantContent {
			o.recordAssistantContentProgress(assistantID, streamedAssistant)
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
		case "thinking_tokens":
			// Helper estimates lack a task/run identity; root estimates have an explicit null parent.
			parent := bytes.TrimSpace(e.ParentToolUseID)
			if o.sessionID != "" && e.SessionID == o.sessionID && e.UUID != "" && e.ThinkingDelta > 0 &&
				e.AgentID == "" && bytes.Equal(parent, []byte("null")) {
				o.recordUniqueProgress("thinking_tokens:" + e.UUID)
			}
		}
	}
}

func (o *jsonlOutput) recordAssistantContentProgress(id string, streamed bool) {
	if streamed {
		return // Stream output already reported this message's activity.
	}
	key := "assistant:" + id
	if o.progressSeen[key] {
		o.onProgress() // The new content snapshot already reserved bounded history.
	} else {
		o.recordUniqueProgress(key)
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
		_, historicalAssistant := o.assistantHistory[native.Message.ID]
		replay := assistantSeen || historicalAssistant || o.progressSeen["assistant:"+native.Message.ID] || o.progressSeen[startKey]
		if !replay {
			if !o.rememberProgress(startKey) {
				return
			}
			if o.results != 0 && !helper {
				o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
			}
			if o.stopped && !helper {
				if o.rootStartsAfterStop == nil {
					o.rootStartsAfterStop = make(map[string]bool)
				}
				o.rootStartsAfterStop[native.Message.ID] = true
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
	if o.streamedSinceAssistant == nil {
		o.streamedSinceAssistant = make(map[string]bool)
	}
	o.streamedSinceAssistant[state.messageID] = true
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
	return id + "\x00" + runID
}

// The open set is rebuilt once from bounded progress history on a resumed call.
// List disappearance cannot close a tagged run: lists may replay without a sequence ID.
func (o *jsonlOutput) ensureTaskRuns() {
	if o.taskRuns != nil {
		return
	}
	o.taskRuns = make(map[string]bool)
	o.taskRunCounts = make(map[string]int)
	for identity := range o.progressSeen {
		task, ok := strings.CutPrefix(identity, "task_start:")
		if !ok {
			task, ok = strings.CutPrefix(identity, "task_seen:")
		}
		id, runID, tagged := strings.Cut(task, "\x00")
		if !ok || !tagged || runID == "" ||
			o.progressSeen["task_finish:"+task] {
			continue
		}
		if !o.taskRuns[task] {
			o.taskRuns[task] = true
			o.taskRunCounts[id]++
		}
	}
}

func (o *jsonlOutput) updateTaskPending() {
	o.ensureTaskRuns()
	o.pending = len(o.taskRuns) + o.runlessCount + o.anonymousPending
	if o.pending != 0 {
		o.completed = false
		o.result.Answer = ""
	}
}

func (o *jsonlOutput) markRunlessAmbiguous(id string) {
	if o.runlessAmbiguous == nil {
		o.runlessAmbiguous = make(map[string]bool)
	}
	o.runlessAmbiguous[id] = true
}

func (o *jsonlOutput) observeTaskRun(id, runID, identity string, credit bool) {
	key := taskRunKey(id, runID)
	if o.progressSeen["task_finish:"+key] {
		return
	}
	o.ensureTaskRuns()
	if credit && o.onProgress != nil {
		o.recordUniqueProgress(identity + key)
	} else {
		o.rememberProgress(identity + key)
	}
	if o.err != nil {
		return
	}
	if !o.taskRuns[key] {
		o.taskRunCounts[id]++
	}
	o.taskRuns[key] = true
	o.updateTaskPending()
}

func (o *jsonlOutput) finishTaskRun(id, runID string) {
	key := taskRunKey(id, runID)
	if o.progressSeen["task_finish:"+key] {
		return
	}
	o.ensureTaskRuns()
	if o.onProgress != nil {
		o.recordUniqueProgress("task_finish:" + key)
	} else {
		o.rememberProgress("task_finish:" + key)
	}
	if o.err != nil {
		return
	}
	if o.taskRuns[key] {
		o.taskRunCounts[id]--
		if o.taskRunCounts[id] == 0 {
			delete(o.taskRunCounts, id)
		}
	}
	delete(o.taskRuns, key)
	o.updateTaskPending()
}

func (o *jsonlOutput) recordTaskProgress(id, runID, status, noticeUUID string) {
	if id == "" {
		return
	}
	switch status {
	case "started", "completed", "failed", "stopped", "killed":
	default:
		return
	}
	repeatedUnidentifiedNotice := false
	if runID == "" && noticeUUID == "" && status != "started" {
		// Without either identity, a later notice with this ID may replay this one.
		repeatedUnidentifiedNotice = !o.rememberProgress("task_notice_id_only:" + id)
		if o.err != nil {
			return
		}
	}
	if status == "completed" || status == "failed" {
		// A task ID and status cannot distinguish a new notice from replay.
		replay := runID != "" && o.progressSeen["task_finish:"+taskRunKey(id, runID)]
		if noticeUUID != "" {
			seenNotice := !o.rememberProgress("task_notice_uuid:" + noticeUUID)
			if o.err != nil {
				return
			}
			if seenNotice && runID == "" {
				return // A replay cannot finish a later ID-only start.
			}
			replay = replay || seenNotice
		}
		if o.completed && !replay {
			o.completed, o.result.Answer = false, ""
		}
	}
	if runID == "" && status != "started" {
		// A later ID-only finish may describe this task after its ID is reused.
		o.rememberProgress("task_id_only_terminal:" + id)
		if o.err != nil {
			return
		}
	}
	if runID == "" && status != "started" && o.runlessPending[id] != 0 {
		o.ensureTaskRuns()
		if o.taskRunCounts[id] != 0 {
			return // This ID-only notice may describe the tagged run instead.
		}
	}
	if runID == "" {
		if status == "started" {
			if noticeUUID != "" && !o.rememberProgress("task_start_uuid:"+noticeUUID) {
				return
			}
			if o.err != nil {
				return
			}
			if o.progressSeen["task_id_only_terminal:"+id] {
				o.markRunlessAmbiguous(id)
			}
			if o.runlessExplicit == nil {
				o.runlessExplicit = make(map[string]bool)
			}
			if o.runlessPending[id] > 0 {
				// A prior list row or usage may describe a different task.
				o.markRunlessAmbiguous(id)
			}
			o.runlessExplicit[id] = true
			if o.runlessPending == nil {
				o.runlessPending = make(map[string]int)
			}
			if o.runlessPending[id] == 0 {
				o.runlessPending[id] = 1
				o.runlessCount++
			}
		} else if o.runlessPending[id] == 1 && !o.runlessAmbiguous[id] && !repeatedUnidentifiedNotice {
			// Once duplicated, an ID-only notice cannot identify the remaining task.
			delete(o.runlessPending, id)
			delete(o.runlessExplicit, id)
			o.runlessCount--
		}
		o.updateTaskPending()
		return
	}
	if status == "started" {
		o.observeTaskRun(id, runID, "task_start:", true)
	} else {
		o.finishTaskRun(id, runID)
	}
}

func (o *jsonlOutput) recordTaskUsageProgress(e event) {
	if e.TaskID == "" {
		return
	}
	if e.RunID == "" {
		// ID-only usage proves pending work but cannot prove fresh work for the timer.
		if o.runlessExplicit == nil {
			o.runlessExplicit = make(map[string]bool)
		}
		started, seen := o.runlessExplicit[e.TaskID]
		if o.runlessPending[e.TaskID] > 0 && (!seen || started) {
			// Repeated usage is a snapshot; a start or list is not correlated.
			o.markRunlessAmbiguous(e.TaskID)
		}
		if !seen {
			o.runlessExplicit[e.TaskID] = false
		}
		if o.runlessPending == nil {
			o.runlessPending = make(map[string]int)
		}
		if o.runlessPending[e.TaskID] == 0 {
			o.runlessPending[e.TaskID] = 1
			o.runlessCount++
		}
		o.updateTaskPending()
		return
	}
	key := taskRunKey(e.TaskID, e.RunID)
	if o.progressSeen["task_finish:"+key] {
		return // Delayed cumulative usage is not fresh execution.
	}
	var counters struct {
		TotalTokens *int64 `json:"total_tokens"`
		ToolUses    *int64 `json:"tool_uses"`
	}
	validUsage := json.Unmarshal(e.Usage, &counters) == nil
	state, knownTask := o.taskProgress[key]
	changed := false
	if validUsage && counters.TotalTokens != nil && *counters.TotalTokens > state.totalTokens {
		state.totalTokens = *counters.TotalTokens
		changed = true
	}
	if validUsage && counters.ToolUses != nil && *counters.ToolUses > state.toolUses {
		state.toolUses = *counters.ToolUses
		changed = true
	}
	identity := "task_seen:"
	if changed || o.progressSeen["task_start:"+key] {
		identity = "task_start:"
	}
	o.observeTaskRun(e.TaskID, e.RunID, identity, false)
	if o.err != nil || !changed {
		return
	}
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
	if o.onProgress != nil {
		o.onProgress()
	}
}

func (o *jsonlOutput) recordAssistantUsage(raw, parent json.RawMessage) (freshContent bool, messageID string) {
	var message struct {
		ID      string          `json:"id"`
		Usage   json.RawMessage `json:"usage"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil || message.ID == "" {
		o.hasCumulative = false
		o.uncovered = o.aggregateGeneration != 0
		if o.results != 0 {
			o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
		}
		return
	}
	messageID = message.ID
	root := bytes.Equal(bytes.TrimSpace(parent), []byte("null"))
	var parentID string
	unknown := !root && (json.Unmarshal(parent, &parentID) != nil || parentID == "")
	old, seen := o.assistantUsage[message.ID]
	prior, historical := o.assistantHistory[message.ID]
	priorTokens := prior.tokens
	if !seen && !historical && !o.reserveProgress(1, len(message.ID)) {
		return // The bounded history limit is a fatal output error.
	}
	var rawUsage eventUsage
	valid := json.Unmarshal(message.Usage, &rawUsage) == nil
	var tokens usage.Tokens
	if valid {
		tokens, valid = rawUsage.tokens()
	}
	unknownPrior := historical && !prior.hasTokens
	firstHistoricalCounters := valid && (unknownPrior && !seen || seen && old.priorMissing)
	if firstHistoricalCounters {
		// The first counters for an old ID may include work before this call.
		// Only later increases are attributable without a cumulative result.
		priorTokens = tokens
		old.prior = tokens
		old.priorMissing = false
	}
	contentHash, hasContent := assistantContentHash(message.Content)
	contentKey := ""
	newContent := false
	if hasContent {
		contentKey = "assistant_content:" + message.ID + ":" + hex.EncodeToString(contentHash[:])
		newContent = !o.progressSeen[contentKey]
	}
	if historical && !seen {
		increased := valid && (!prior.hasTokens || prior.tokens.Max(tokens) != prior.tokens)
		if !increased && !newContent {
			return // Exact or older replay cannot change this call.
		}
	}
	if newContent && !o.rememberProgress(contentKey) {
		return // The bounded history limit is a fatal output error.
	}
	freshContent = newContent
	if newContent && o.results != 0 {
		o.completed, o.result.Answer, o.result.UsageComplete = false, "", false
	}
	if valid {
		if updated := prior.tokens.Max(tokens); updated.Valid() {
			prior.tokens, prior.hasTokens = updated, true
		} else {
			valid = false
		}
	}
	if o.assistantHistory == nil {
		o.assistantHistory = make(map[string]assistantSnapshot)
	}
	o.assistantHistory[message.ID] = prior
	if firstHistoricalCounters && !newContent {
		// A newly revealed absolute floor is not evidence of current work.
		// Preserve a covered result and charge only future increases.
		if seen {
			o.assistantUsage[message.ID] = old
		}
		return
	}
	if o.baselineUnknown || !seen {
		o.hasCumulative = false
	}
	if !seen {
		if o.assistantUsage == nil {
			o.assistantUsage = make(map[string]messageUsage)
		}
		old = messageUsage{prior: priorTokens, priorMissing: unknownPrior && !valid,
			generation: o.aggregateGeneration, root: root}
		// Remember IDs even before usable counters arrive. A later block from
		// an already covered message must not become a new billable call.
		o.assistantUsage[message.ID] = old
		o.uncovered = o.aggregateGeneration != 0
		if root && !historical && (!o.stopped || o.rootStartsAfterStop[message.ID] ||
			!o.progressSeen["stream_start:"+message.ID]) {
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
	if !valid {
		markUsageUncertain()
		return
	}
	tokens = old.tokens.Max(old.prior.Max(tokens).Sub(old.prior))
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
	return
}

func assistantContentHash(raw json.RawMessage) ([32]byte, bool) {
	content := bytes.TrimSpace(raw)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return [32]byte{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return sha256.Sum256(content), true
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return [32]byte{}, false
		}
	case []any:
		if len(typed) == 0 {
			return [32]byte{}, false
		}
		// Block metadata can change on replay without new model work.
		projected := make([]any, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			var field string
			switch block["type"] {
			case "text":
				field = "text"
			case "thinking":
				field = "thinking"
			case "redacted_thinking":
				field = "data"
			case "tool_use":
				id, ok := block["id"].(string)
				if !ok || id == "" {
					continue
				}
				contentBlock := map[string]any{"type": "tool_use", "id": id}
				for _, key := range []string{"name", "input"} {
					if value, exists := block[key]; exists {
						contentBlock[key] = value
					}
				}
				projected = append(projected, contentBlock)
				continue
			default:
				continue
			}
			if text, ok := block[field].(string); ok && text != "" {
				projected = append(projected, map[string]any{"type": block["type"], field: text})
			}
		}
		if len(projected) == 0 {
			return [32]byte{}, false
		}
		value = projected
	default:
		return [32]byte{}, false // Native assistant content is a string or list of known blocks.
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return sha256.Sum256(content), true
	}
	return sha256.Sum256(canonical), true
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
