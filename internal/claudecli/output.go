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
	"strconv"
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
	maxResultEvents           = 128
	toolProofBytes            = 64 // Bounded allowance for one transient tool map entry.
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
	tokens = usage.Tokens{Input: *u.Input + *u.Created + *u.Cached, CacheCreationInput: *u.Created,
		CachedInput: *u.Cached, Output: *u.Output}
	valid = tokens.Valid()
	return
}

func partialEventTokens(u eventUsage) (usage.Tokens, bool) {
	if u.Input == nil && u.Created == nil && u.Cached == nil && u.Output == nil {
		return usage.Tokens{}, false
	}
	zero := int64(0)
	if u.Input == nil {
		u.Input = &zero
	}
	if u.Created == nil {
		u.Created = &zero
	}
	if u.Cached == nil {
		u.Cached = &zero
	}
	if u.Output == nil {
		u.Output = &zero
	}
	return u.tokens()
}

func knownTokenFloor(a, b usage.Tokens) (usage.Tokens, bool) {
	if !a.Valid() || !b.Valid() {
		return usage.Tokens{}, false
	}
	// Keep one reported input/cache tuple; categories from separate snapshots
	// may describe the same tokens. Keep the larger independent output floor.
	if b.Input > a.Input {
		a.Input, a.CacheCreationInput, a.CachedInput = b.Input, b.CacheCreationInput, b.CachedInput
	}
	a.Output = max(a.Output, b.Output)
	return a, a.Valid()
}

func strongerTokenFloor(a, b usage.Tokens) usage.Tokens {
	if !a.Valid() {
		return b
	}
	if !b.Valid() {
		return a
	}
	if b.Total() > a.Total() || b.Total() == a.Total() &&
		(b.Input > a.Input || b.Input == a.Input &&
			(b.CacheCreationInput > a.CacheCreationInput || b.CacheCreationInput == a.CacheCreationInput && b.CachedInput > a.CachedInput)) {
		return b
	}
	return a
}

func boundaryReceiptFloor(boundary, receipt usage.Tokens) usage.Tokens {
	if combined, ok := boundary.AddChecked(receipt); ok {
		return combined
	}
	if combined, ok := knownTokenFloor(receipt, boundary); ok {
		return combined
	}
	return strongerTokenFloor(boundary, receipt)
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
	TotalCostUSD      *float64          `json:"total_cost_usd"`
	Errors            json.RawMessage   `json:"errors"`
	StructuredOutput  json.RawMessage   `json:"structured_output"`
	Usage             json.RawMessage   `json:"usage"`
	ModelUsage        json.RawMessage   `json:"modelUsage"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           json.RawMessage   `json:"message"`
	StreamEvent       json.RawMessage   `json:"event"`
	ParentToolUseID   json.RawMessage   `json:"parent_tool_use_id"`
	parentID          string
	parentForm        parentForm
	TaskID            string          `json:"task_id"`
	RunID             string          `json:"run_id"`
	Status            string          `json:"status"`
	Ambient           bool            `json:"ambient"`
	Summary           string          `json:"summary"`
	APIError          string          `json:"api_error"`
	APIErrorStatus    json.RawMessage `json:"api_error_status"`
	IsSynthetic       bool            `json:"isSynthetic"`
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
	resultIndex  int64
	postStream   bool
	root         bool
	historical   bool
}

type assistantSnapshot struct {
	tokens     usage.Tokens
	hasTokens  bool
	hasContent bool
}

type progressStream struct {
	key            string
	messageID      string
	resultIndex    int64 // Number of accepted results at this message's start.
	stopped        bool
	activeIndex    int
	sealedIndex    int
	hasSealed      bool
	tools          map[int]toolProgress
	deltas         map[streamDelta]bool
	signatureBytes int64
	lastDeltaProof [32]byte
	lastWasNoUUID  bool
}

type toolProgress struct {
	status   uint8 // 1 started, 2 stopped.
	proof    [32]byte
	hasProof bool
}

type streamDelta struct {
	uuid, kind, content string
	index               int
}

type streamIdentity struct {
	key, messageID string
}

type streamUsageSample struct {
	input, created, cached, output int64
	resultIndex                    int64 // Number of accepted results when this ID first appeared.
}

func (s streamUsageSample) tokens() (usage.Tokens, bool) {
	return (eventUsage{Input: &s.input, Created: &s.created, Cached: &s.cached, Output: &s.output}).tokens()
}

func (id streamIdentity) progressKey(kind string) string {
	// Length-prefix the stream key: native IDs may contain separators.
	return kind + ":" + strconv.Itoa(len(id.key)) + ":" + id.key + id.messageID
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
	streamUsage            map[streamIdentity]streamUsageSample // Fresh invocation only; never persisted for replay.
	streamedSinceAssistant map[streamIdentity]bool
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
	transientBytes         int64 // Stream proof lives only for this invocation.
	transientEntries       int
	progressByteLimit      int64 // Tests may lower the cumulative history limit.
	progressEntryLimit     int   // Tests may lower the cumulative history limit.
	identitySeen           bool
	baseline               usage.Tokens
	baselineUnknown        bool
	costBaselineUSD        float64
	costBaselineUnknown    bool
	costObservedUSD        float64 // Comparison floor, including prior raw observations of unknown scope.
	costContradiction      bool
	nativeCostUSD          *float64
	perCallResults         bool
	resumed                bool
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
	resultBoundary         usage.Tokens // Known subtotal at the last accepted result.
	boundaryRootOnly       bool         // Known boundary has root usage without proved whole-tree coverage.
	result                 aicli.Result
	finalModelUsage        bool
	finalTopUsage          bool
	finalTopTokens         usage.Tokens
	finalReportPresent     bool
	finalMalformedUsage    bool
	cumulativeUnattributed bool
	aggregateGeneration    int64
	aggregateTokens        usage.Tokens
	assistantUsage         map[streamIdentity]messageUsage
	assistantHistory       map[streamIdentity]assistantSnapshot
	assistantTotal         usage.Tokens // Sum of per-message counter maxima.
	assistantTotalReady    bool
	assistantTotalOverflow bool
	provenanceGap          bool
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
	if !valid {
		o.provenanceGap = true
		if combined, ok := knownTokenFloor(o.result.Tokens, o.assistantTokens); ok {
			lower = combined
		} else {
			lower = strongerTokenFloor(o.result.Tokens, o.assistantTokens)
		}
		valid = lower.Valid() // An unrepresentable sum can still have a known standalone floor.
	}
	if !o.hasCumulative && !o.resumed {
		streamLower, streamValid := usage.Tokens{}, true
		if len(o.streamUsage) != 0 {
			streamLower, _, _, streamValid = o.streamUsageLowerBound()
		}
		if !streamValid {
			o.provenanceGap = true
		}
		if len(o.streamUsage) != 0 && streamLower.Valid() {
			if !valid {
				o.provenanceGap = true
				lower = o.result.Tokens
				if !lower.Valid() || streamLower.Total() > lower.Total() {
					lower = streamLower
				}
				valid = lower.Valid()
			} else if combined, ok := knownTokenFloor(streamLower, lower); ok {
				lower = combined
			} else {
				o.provenanceGap = true // Different cache categories need not be additive.
				if streamLower.Total() > lower.Total() {
					lower = streamLower
				}
			}
		}
		if o.results != 0 {
			post, _, postValid := o.resultBoundaryEvidence()
			if !postValid {
				o.provenanceGap = true
			}
			if post.Valid() {
				if combined, ok := knownTokenFloor(lower, post); ok {
					lower, valid = combined, true
				} else {
					o.provenanceGap = true
					if !valid || post.Total() > lower.Total() {
						lower, valid = post, true
					}
				}
			}
		}
	}
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
	coverage := usage.Coverage{
		MissingFinalReport:     !o.finalReportPresent,
		MalformedFinalReport:   o.finalMalformedUsage,
		UnverifiedHelpers:      o.pending != 0 || o.taskFailure != nil,
		CumulativeUnattributed: o.cumulativeUnattributed,
	}
	switch {
	case o.result.UsageComplete:
		coverage.Scope = "invocation"
	case o.finalTopUsage && (!o.finalModelUsage || o.cumulativeUnattributed && o.aggregateGeneration == 0) && o.result.Tokens == o.finalTopTokens:
		coverage.Scope = "root_only"
	case o.result.Tokens != (usage.Tokens{}):
		coverage.Scope = "known_subset"
	default:
		coverage.Scope = "unknown"
	}
	if !o.result.UsageComplete && (coverage.Scope == "known_subset" || coverage.Scope == "root_only") {
		coverage.PartialReport = true
	}
	o.result.UsageCoverage = coverage
}

func addStreamFloor(total, full, partial int64) (int64, bool) {
	if partial <= full {
		return total, true
	}
	if total > math.MaxInt64-(partial-full) {
		return total, false
	}
	return total + partial - full, true
}

func coversStreamUsage(tokens, required usage.Tokens, regular int64) bool {
	return tokens.Valid() && tokens.Input >= required.Input &&
		tokens.CacheCreationInput >= required.CacheCreationInput && tokens.CachedInput >= required.CachedInput &&
		tokens.Output >= required.Output &&
		tokens.Input-tokens.CacheCreationInput-tokens.CachedInput >= regular
}

func (o *jsonlOutput) streamUsageLowerBound() (usage.Tokens, usage.Tokens, int64, bool) {
	total := o.assistantTokens
	best := total
	required := total // Coverage-only component floors may not form a valid token tuple.
	regular := total.Input - total.CacheCreationInput - total.CachedInput
	valid, sumValid := true, true
	for id, sample := range o.streamUsage {
		partial, ok := sample.tokens()
		if !ok {
			valid = false
			continue
		}
		full := o.assistantUsage[id].tokens
		if !full.Valid() {
			valid = false
			continue
		}
		fullRegular := full.Input - full.CacheCreationInput - full.CachedInput
		for _, field := range []struct {
			target        *int64
			full, partial int64
		}{
			{&required.Input, full.Input, partial.Input},
			{&required.CacheCreationInput, full.CacheCreationInput, partial.CacheCreationInput},
			{&required.CachedInput, full.CachedInput, partial.CachedInput},
			{&required.Output, full.Output, partial.Output},
			{&regular, fullRegular, sample.input},
		} {
			if next, ok := addStreamFloor(*field.target, field.full, field.partial); ok {
				*field.target = next
			} else {
				valid = false
			}
		}
		merged, ok := knownTokenFloor(partial, full)
		if !ok {
			valid = false
			continue
		}
		best = strongerTokenFloor(best, merged)
		if !sumValid {
			continue
		}
		without := total.Sub(full)
		if !without.Valid() {
			valid, sumValid = false, false
			continue
		}
		if next, ok := without.AddChecked(merged); ok {
			total = next
		} else {
			valid, sumValid = false, false
		}
	}
	if !sumValid {
		return best, required, regular, false // Keep a valid individual floor.
	}
	return total, required, regular, valid
}

func (o *jsonlOutput) resultBoundaryEvidence() (usage.Tokens, usage.Tokens, bool) {
	post, helpers := o.resultBoundary, usage.Tokens{}
	bestPost, bestHelper, valid := post, usage.Tokens{}, true
	for id, old := range o.assistantUsage {
		if old.resultIndex != o.results || old.historical && !old.postStream {
			continue
		}
		helper := helperStreamKey(id.key)
		if !old.root && !helper {
			continue // Unknown parentage cannot establish additive usage.
		}
		if helper {
			bestHelper = strongerTokenFloor(bestHelper, old.tokens)
			if valid {
				var ok bool
				helpers, ok = helpers.AddChecked(old.tokens)
				valid = valid && ok
			}
		}
		if o.results != 0 && (old.root || helper && o.boundaryRootOnly) {
			bestPost = strongerTokenFloor(bestPost, boundaryReceiptFloor(o.resultBoundary, old.tokens))
			if valid {
				var ok bool
				post, ok = post.AddChecked(old.tokens)
				valid = valid && ok
			}
		}
	}
	for id, sample := range o.streamUsage {
		if sample.resultIndex != o.results {
			continue // Earlier IDs can revise a floor, but are not new additive work.
		}
		root := id.key == "root"
		helper := helperStreamKey(id.key)
		if !root && !helper {
			continue
		}
		partial, ok := sample.tokens()
		if !ok {
			valid = false
			continue
		}
		full, included, includedHelper := usage.Tokens{}, usage.Tokens{}, usage.Tokens{}
		if old, seen := o.assistantUsage[id]; seen {
			full = old.tokens
			if old.resultIndex == o.results && (!old.historical || old.postStream) && helper {
				includedHelper = old.tokens
			}
			if old.resultIndex == o.results && (!old.historical || old.postStream) && (old.root || helper && o.boundaryRootOnly) {
				included = old.tokens
			}
		}
		merged, ok := knownTokenFloor(partial, full)
		if !ok {
			valid = false
			continue
		}
		if helper {
			bestHelper = strongerTokenFloor(bestHelper, merged)
			if valid {
				without := helpers.Sub(includedHelper)
				if without.Valid() {
					helpers, ok = without.AddChecked(merged)
				} else {
					ok = false
				}
				valid = valid && ok
			}
		}
		if o.results != 0 && (root || helper && o.boundaryRootOnly) {
			bestPost = strongerTokenFloor(bestPost, boundaryReceiptFloor(o.resultBoundary, merged))
			if valid {
				without := post.Sub(included)
				if without.Valid() {
					post, ok = without.AddChecked(merged)
				} else {
					ok = false
				}
				valid = valid && ok
			}
		}
	}
	if !valid {
		return bestPost, bestHelper, false // A deterministic valid floor survives unrepresentable sums.
	}
	return post, helpers, true
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
	if e.Ambient {
		return // Unrelated background work cannot change this call or its receipt.
	}
	e.parentID, e.parentForm = decodeAssistantParent(e.ParentToolUseID, e.AgentID)
	if e.Type == "user" && !o.acceptsSession(e.SessionID) {
		o.captureMessage(e) // Foreign content is only an unidentified receipt marker.
		return
	}
	if e.Type == "result" || e.Type == "system" && e.Subtype == "init" {
		if e.parentForm == parentInvalid {
			o.markProvenanceGap(true)
			return
		}
		if e.parentForm != parentMissing && e.parentForm != parentRoot {
			// A helper cannot claim this invocation's root conversation/result.
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
	freshAssistantContent, streamedAssistant, assistantContentful, assistantReplay, assistantID := false, false, false, false, ""
	if e.Type == "assistant" && o.acceptsSession(e.SessionID) {
		freshAssistantContent, assistantID, assistantContentful, assistantReplay = o.recordAssistantUsage(e)
		streamKey, _ := streamProgressKey(e)
		streamID := streamIdentity{streamKey, assistantID}
		streamedAssistant = o.streamedSinceAssistant[streamID]
		if assistantContentful {
			delete(o.streamedSinceAssistant, streamID)
		}
		if assistantContentful && assistantID != "" && o.err == nil {
			o.sealStreamBlock(e, assistantID, freshAssistantContent)
		}
	}
	stop := e.Type == "system" && e.Subtype == "task_notification" &&
		(e.Status == "stopped" || e.Status == "killed") && o.acceptsSession(e.SessionID)
	if stop && !o.acceptTaskStop(e.TaskID, e.RunID, e.UUID, e.Summary) {
		if e.TaskID != "" && e.RunID != "" {
			// An ambiguous notice can share this UUID without proving the tagged finish.
			o.finishTaskRun(e.TaskID, e.RunID, true)
		}
		return
	}
	o.recordProgress(e, freshAssistantContent, streamedAssistant, assistantID)
	if stop {
		o.recordTaskStop(e.Status, e.Summary)
	}
	if e.Type == "rate_limit_event" && o.acceptsSession(e.SessionID) &&
		e.RateLimitInfo.Status == "rejected" && !e.RateLimitInfo.IsUsingOverage && !e.RateLimitInfo.OverageInUse {
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
	if e.Type == "system" && e.Subtype == "background_tasks_changed" && o.acceptsSession(e.SessionID) {
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
				o.rememberProgress("runless_id:" + task.TaskID)
				if o.err != nil {
					return
				}
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
		if e.IsSynthetic && json.Unmarshal(content, &text) == nil {
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
			synthetic := e.IsSynthetic
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
	if o.err == nil {
		switch {
		case e.Type == "assistant" && assistantReplay:
			// A settled full frame contributes neither counters nor content again.
		case e.Type == "assistant" && o.resumed && assistantID == "":
			if assistantContentful && o.onEvent != nil {
				o.onEvent(aicli.StreamEvent{Kind: "content_missing"})
			}
		case e.Type == "assistant" && o.resumed:
			// Exact historical replay has no current receipt content.
		case e.Type == "assistant" && assistantID == "":
			receipt := e
			receipt.SessionID = "" // Keep block kinds without attributing rejected content.
			o.captureMessage(receipt)
		default:
			o.captureMessage(e)
		}
	}
	if e.Type != "result" {
		return
	}
	if o.results >= maxResultEvents {
		o.fail(fmt.Errorf("%w: Claude result count exceeded the limit", aicli.ErrOutputLimit))
		return
	}
	// A resumed Claude conversation can report a cumulative estimate. Keep
	// only a proven delta for this invocation. A settled later snapshot can
	// reanchor attribution after a gap. A crash can emit zeroed cost after
	// spending tokens, so that zero is not an estimate.
	o.nativeCostUSD = nil
	if e.TotalCostUSD != nil && usage.ValidEstimatedCostUSD(e.TotalCostUSD) &&
		!(e.Subtype == "error_during_execution" && *e.TotalCostUSD == 0) {
		if *e.TotalCostUSD < o.costObservedUSD {
			o.costContradiction = true
		} else {
			o.costObservedUSD = *e.TotalCostUSD
		}
		if o.costContradiction {
			o.result.EstimatedCostUSD = nil // Contradictory final snapshot, not a known subtotal.
		} else if o.costBaselineUnknown {
			o.nativeCostUSD = e.TotalCostUSD // Reanchor only after a settled final result.
		} else {
			delta := *e.TotalCostUSD - o.costBaselineUSD
			if usage.ValidEstimatedCostUSD(&delta) {
				o.nativeCostUSD, o.result.EstimatedCostUSD = e.TotalCostUSD, &delta
			}
		}
	}
	tokens, complete := e.tokens()
	modelTokens := tokens
	if complete {
		current := tokens.Sub(o.baseline)
		if !current.Valid() || !covers(current, o.resultBoundary) || current.Output <= o.resultBoundary.Output {
			for id, streamed := range o.streamedSinceAssistant {
				if streamed && !o.assistantHistory[id].hasContent {
					o.markProvenanceGap(!helperStreamKey(id.key)) // New text needs reported output or a full receipt.
					break
				}
			}
		}
	}
	if !complete || o.provenanceGap || o.assistantTotalOverflow {
		o.nativeCostUSD = nil // This report cannot certify a cost snapshot.
	}
	streamLower, streamRequired, streamRegular, streamValid := usage.Tokens{}, usage.Tokens{}, int64(0), true
	if !o.resumed && len(o.streamUsage) != 0 {
		streamLower, streamRequired, streamRegular, streamValid = o.streamUsageLowerBound()
		if !streamValid {
			o.provenanceGap = true
			o.nativeCostUSD = nil
		}
	}
	postFloor, helpers, postValid := o.resultBoundaryEvidence()
	if !postValid {
		o.provenanceGap = true
		o.nativeCostUSD = nil
	}
	coverageFloor := postFloor
	if o.results != 0 && !o.boundaryRootOnly {
		var coverageValid bool
		coverageFloor, coverageValid = coverageFloor.AddChecked(helpers)
		if !coverageValid {
			o.provenanceGap = true
			o.nativeCostUSD = nil
		}
	}
	var top eventUsage
	if json.Unmarshal(e.Usage, &top) == nil {
		o.finalTopTokens, o.finalTopUsage = top.tokens()
	} else {
		o.finalTopTokens, o.finalTopUsage = usage.Tokens{}, false
	}
	o.finalModelUsage = complete
	o.finalReportPresent = reportPresent(e.ModelUsage) || reportPresent(e.Usage)
	o.finalMalformedUsage = !complete && (reportPresent(e.ModelUsage) || !o.finalTopUsage && reportPresent(e.Usage))
	rootHelperFloor := usage.Tokens{}
	if o.finalTopUsage {
		var rootHelperValid bool
		rootHelperFloor, rootHelperValid = o.finalTopTokens.AddChecked(helpers)
		if !rootHelperValid {
			o.provenanceGap = true
			o.nativeCostUSD = nil
		} else if !complete {
			tokens = rootHelperFloor // Top-level usage excludes helpers; this is an incomplete floor.
		}
	}
	o.hasCumulative = false
	if complete {
		if !o.perCallResults {
			o.ensureAssistantTotal()
		}
		cumulative := tokens
		tokens = tokens.Sub(o.baseline)
		root, rootValid := o.finalTopTokens, o.finalTopUsage
		// An exact snapshot must cover the prior cumulative floor plus all
		// attributable current counters before it can become the next baseline.
		lower, valid := o.assistantLowerBound()
		if streamLower != (usage.Tokens{}) {
			if combined, ok := knownTokenFloor(lower, streamLower); ok {
				lower = combined
			} else {
				valid = false
				o.provenanceGap = true
			}
		}
		for _, floor := range []usage.Tokens{postFloor, rootHelperFloor} {
			if floor == (usage.Tokens{}) {
				continue
			}
			if combined, ok := knownTokenFloor(floor, lower); ok {
				lower = combined
			} else {
				valid = false
				o.provenanceGap = true
			}
		}
		assistantFloor, assistantValid := o.assistantTotal, !o.assistantTotalOverflow
		if o.perCallResults {
			assistantFloor = o.assistantTokens
		}
		if !assistantValid || o.provenanceGap || !covers(cumulative, assistantFloor) ||
			!valid || !covers(tokens, lower) || !covers(tokens, coverageFloor) || !covers(tokens, rootHelperFloor) ||
			!coversStreamUsage(tokens, streamRequired, streamRegular) {
			o.nativeCostUSD = nil // The final cost snapshot cannot certify all assistant work.
		}
		complete = tokens.Valid() && valid && assistantValid && !o.provenanceGap &&
			covers(tokens, lower) && covers(tokens, coverageFloor) && covers(tokens, rootHelperFloor) &&
			coversStreamUsage(tokens, streamRequired, streamRegular) &&
			covers(cumulative, assistantFloor) &&
			(!rootValid || covers(tokens, root))
		if complete {
			o.cumulative, o.hasCumulative = cumulative, true
		}
		if o.baselineUnknown || !tokens.Valid() {
			// An uncertain prior baseline cannot identify this invocation's delta.
			o.cumulativeUnattributed = true
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
	for _, floor := range []usage.Tokens{postFloor, rootHelperFloor} {
		if floor == (usage.Tokens{}) {
			continue
		}
		if combined, ok := knownTokenFloor(floor, o.result.Tokens); ok {
			o.result.Tokens = combined
		} else {
			o.provenanceGap = true
			complete = false
			o.nativeCostUSD = nil
			if floor.Valid() && (!o.result.Tokens.Valid() || floor.Total() > o.result.Tokens.Total()) {
				o.result.Tokens = floor
			}
		}
	}
	if covers(tokens, o.result.Tokens) {
		o.result.Tokens = tokens
	} else {
		if lower, ok := knownTokenFloor(o.result.Tokens, tokens); ok {
			o.result.Tokens = lower
		} else if lower := o.result.Tokens.Max(tokens); lower.Valid() {
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
	modelDelta := modelTokens.Sub(o.baseline)
	modelCovers := func(floor usage.Tokens) bool {
		return o.finalModelUsage && modelDelta.Valid() && covers(modelDelta, floor)
	}
	o.boundaryRootOnly = o.finalTopUsage && !modelCovers(o.finalTopTokens) ||
		o.boundaryRootOnly && !modelCovers(o.result.Tokens)
	o.resultBoundary = o.result.Tokens
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

func reportPresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null"))
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
		case e.Type == "assistant" && block.Type == "text":
			if block.Text != "" {
				o.emitContent(e, "assistant", block.Text)
			} else {
				wrapped := make([]byte, 0, len(raw)+2)
				wrapped = append(wrapped, '[')
				wrapped = append(wrapped, raw...)
				wrapped = append(wrapped, ']')
				if _, _, unprojectable := assistantContentProof(wrapped); unprojectable {
					o.emitContent(e, kind, "")
				}
			}
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
	if e.parentForm == parentRoot {
		o.emitEventWithID(o.sessionID, kind, text)
		return
	}
	if e.parentForm == parentTagged {
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
		if uuid != "" && !uuidSeen && !o.rememberProgress("task_notice_uuid:"+uuid) {
			return false
		}
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
	if !o.acceptsSession(e.SessionID) {
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
			streamKey, _ := streamProgressKey(e)
			o.recordAssistantContentProgress(streamIdentity{streamKey, assistantID}, streamedAssistant)
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
			if o.sessionID != "" && e.SessionID == o.sessionID && e.UUID != "" && e.ThinkingDelta > 0 &&
				e.parentForm == parentRoot {
				o.recordUniqueProgress("thinking_tokens:" + e.UUID)
			}
		}
	}
}

func (o *jsonlOutput) recordAssistantContentProgress(id streamIdentity, streamed bool) {
	if streamed {
		return // Stream output already reported this message's activity.
	}
	key := id.progressKey("assistant")
	if o.progressSeen[key] {
		o.onProgress() // The new content snapshot already reserved bounded history.
	} else {
		o.recordUniqueProgress(key)
	}
}

type parentForm uint8

const (
	parentInvalid parentForm = iota
	parentMissing
	parentRoot
	parentTagged
	parentUntagged
)

func decodeAssistantParent(raw json.RawMessage, agentID string) (id string, form parentForm) {
	parent := bytes.TrimSpace(raw)
	if len(parent) == 0 {
		if agentID == "" {
			return "", parentMissing
		}
		return "", parentInvalid
	}
	if bytes.Equal(parent, []byte("null")) {
		if agentID == "" {
			return "", parentRoot
		}
		return "", parentInvalid
	}
	if json.Unmarshal(parent, &id) != nil || id == "" {
		return "", parentInvalid
	}
	if agentID != "" {
		return id, parentTagged
	}
	return id, parentUntagged
}

func streamProgressKey(e event) (key string, root bool) {
	switch e.parentForm {
	case parentMissing:
		return "root-untagged", true
	case parentRoot:
		return "root", true
	case parentTagged:
		return taggedStreamKey(e.AgentID, e.parentID), false
	case parentUntagged:
		return "parent:" + e.parentID, false
	default:
		return "unknown-parent", true // Invalid parentage cannot prove a root answer.
	}
}

func taggedStreamKey(agentID, parentID string) string {
	return "agent:" + strconv.Itoa(len(agentID)) + ":" + agentID + parentID
}

func helperStreamKey(key string) bool {
	return strings.HasPrefix(key, "agent:") || strings.HasPrefix(key, "parent:")
}

func (o *jsonlOutput) markUncoveredStream(root bool) {
	o.hasCumulative = false
	o.result.UsageComplete = false
	o.nativeCostUSD = nil // The prior result remains a known subtotal only.
	if root {
		o.completed, o.result.Answer = false, ""
	}
}

func (o *jsonlOutput) markProvenanceGap(root bool) {
	o.provenanceGap = true
	o.markUncoveredStream(root)
}

func (o *jsonlOutput) hasAssistantIdentity(id streamIdentity) bool {
	_, current := o.assistantUsage[id]
	_, historical := o.assistantHistory[id]
	return current || historical || o.progressSeen[id.progressKey("stream_start")] ||
		o.progressSeen[id.progressKey("assistant")]
}

func (o *jsonlOutput) claimAssistantOrigin(e event, messageID string) bool {
	if e.parentForm == parentInvalid {
		o.markProvenanceGap(true)
		return false
	}
	if e.parentForm == parentRoot || e.parentForm == parentMissing {
		other := "root"
		if e.parentForm == parentRoot {
			other = "root-untagged"
		}
		if o.hasAssistantIdentity(streamIdentity{other, messageID}) {
			o.markProvenanceGap(true)
			return false
		}
		return true
	}
	// A helper can be wrapped with agent_id or only parent_tool_use_id.
	// Opposite forms sharing a parent and message ID cannot prove two calls.
	id := streamIdentity{e.parentID, messageID}
	own, opposite := "assistant_tagged", "assistant_untagged"
	if e.parentForm == parentUntagged {
		own, opposite = opposite, own
	}
	if o.progressSeen[id.progressKey(opposite)] {
		o.markProvenanceGap(false)
		return false
	}
	key := id.progressKey(own)
	return o.progressSeen[key] || o.rememberProgress(key)
}

func (o *jsonlOutput) progressCheckpoint() (bytes int64, entries int) {
	return o.progressBytes - o.transientBytes, o.progressEntries - o.transientEntries
}

func (o *jsonlOutput) discardStream(key string) {
	if state := o.progressStreams[key]; state != nil {
		entries := len(state.deltas) + len(state.tools)
		bytes := state.signatureBytes + int64(toolProofBytes*len(state.tools))
		o.progressEntries -= entries
		o.progressBytes -= bytes
		o.transientEntries -= entries
		o.transientBytes -= bytes
	}
	delete(o.progressStreams, key)
}

func (o *jsonlOutput) sealStreamBlock(e event, id string, freshContent bool) {
	key, _ := streamProgressKey(e)
	state := o.progressStreams[key]
	if state == nil || state.messageID != id || state.hasSealed && !freshContent {
		return
	}
	// Full frames seal stream blocks but do not add replay proof.
	state.sealedIndex = state.activeIndex
	state.hasSealed = true
}

func (o *jsonlOutput) observeStreamUsage(id streamIdentity, root bool, raw json.RawMessage) {
	if o.resumed || o.results != 0 && (!o.indexed || id.key != "root" && !helperStreamKey(id.key)) || !reportPresent(raw) {
		return // Only fresh bounded identities with a possible next result can establish usage.
	}
	var reported eventUsage
	if json.Unmarshal(raw, &reported) != nil {
		return
	}
	snapshot, valid := partialEventTokens(reported)
	if !valid {
		return
	}
	prior := o.streamUsage[id]
	if snapshot == (usage.Tokens{}) && prior == (streamUsageSample{}) {
		return
	}
	next := prior
	contradictory := false
	for _, field := range []struct {
		value *int64
		prior int64
		dest  *int64
	}{
		{reported.Input, prior.input, &next.input},
		{reported.Created, prior.created, &next.created},
		{reported.Cached, prior.cached, &next.cached},
	} {
		if field.value == nil {
			continue
		}
		if *field.value < field.prior {
			contradictory = true
		} else {
			*field.dest = *field.value
		}
	}
	if _, ok := next.tokens(); !ok || contradictory {
		o.markProvenanceGap(root)
		o.retainStandaloneFloor(snapshot)
		// Contradictory cumulative fields cannot replace the earlier tuple.
		next.input, next.created, next.cached = prior.input, prior.created, prior.cached
	}
	if reported.Output != nil {
		if *reported.Output < prior.output {
			o.markProvenanceGap(root)
		} else {
			next.output = *reported.Output
		}
	}
	if _, ok := next.tokens(); !ok {
		o.markProvenanceGap(root)
		next.output = prior.output
	}
	if next == prior {
		return
	}
	if o.streamUsage == nil {
		o.streamUsage = make(map[streamIdentity]streamUsageSample)
	}
	if _, seen := o.streamUsage[id]; !seen {
		state := o.progressStreams[id.key]
		if state == nil || state.messageID != id.messageID {
			return
		}
		// The accepted stream start bounds IDs; reserve transient payload space too.
		bytes := len(id.key) + len(id.messageID) + 40
		if !o.reserveProgress(1, bytes) {
			return
		}
		o.transientEntries++
		o.transientBytes += int64(bytes)
		next.resultIndex = state.resultIndex
	}
	o.streamUsage[id] = next
}

func (o *jsonlOutput) recordStreamProgress(e event) {
	if e.parentForm == parentInvalid {
		o.markProvenanceGap(true)
		return
	}
	key, root := streamProgressKey(e)
	var native struct {
		Type    string          `json:"type"`
		Index   *int            `json:"index"`
		Usage   json.RawMessage `json:"usage"`
		Message struct {
			ID      string          `json:"id"`
			Usage   json.RawMessage `json:"usage"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        json.RawMessage `json:"delta"`
	}
	var delta struct {
		Type         string          `json:"type"`
		Text         *string         `json:"text"`
		Thinking     *string         `json:"thinking"`
		PartialJSON  *string         `json:"partial_json"`
		Usage        json.RawMessage `json:"usage"`
		Signature    json.RawMessage `json:"signature"`
		StopReason   json.RawMessage `json:"stop_reason"`
		StopSequence json.RawMessage `json:"stop_sequence"`
	}
	if json.Unmarshal(e.StreamEvent, &native) != nil {
		if o.results != 0 {
			o.markUncoveredStream(root) // Malformed nested content cannot prove replay.
		}
		return
	}
	deltaRaw := bytes.TrimSpace(native.Delta)
	if len(deltaRaw) != 0 && !bytes.Equal(deltaRaw, []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(deltaRaw))
		decoder.DisallowUnknownFields()
		if deltaRaw[0] != '{' || decoder.Decode(&delta) != nil {
			if o.results != 0 {
				o.markUncoveredStream(root)
			}
			return
		}
	} else if o.results != 0 && native.Type == "content_block_delta" {
		o.markUncoveredStream(root) // A content event needs an object payload.
		return
	}
	var block struct {
		Type string `json:"type"`
	}
	if native.Type == "content_block_start" {
		content := bytes.TrimSpace(native.ContentBlock)
		if len(content) == 0 || content[0] != '{' || json.Unmarshal(content, &block) != nil || block.Type == "" {
			if o.results != 0 {
				o.markUncoveredStream(root)
			}
			return
		}
	}
	if o.results != 0 && (reportPresent(native.Usage) || reportPresent(native.Message.Usage) || reportPresent(delta.Usage)) {
		o.markUncoveredStream(root) // A later result must cover newly observed usage.
	}
	hasDeltaContent := delta.Text != nil && *delta.Text != "" || delta.Thinking != nil && *delta.Thinking != "" ||
		delta.PartialJSON != nil && *delta.PartialJSON != ""
	_, messageContent, unprojectableMessage := assistantContentProof(native.Message.Content)
	controlDelta := reportPresent(native.Delta) &&
		(native.Type == "message_start" || native.Type == "message_stop" || native.Type == "content_block_stop")
	if o.results != 0 && (messageContent || unprojectableMessage || controlDelta ||
		hasDeltaContent && native.Type != "content_block_delta" ||
		reportPresent(native.ContentBlock) && native.Type != "content_block_start") {
		o.markUncoveredStream(root) // Unproved content on any event cannot certify the result.
		return
	}
	if o.results != 0 && key == "unknown-parent" && native.Type != "message_start" && native.Type != "message_stop" {
		o.markUncoveredStream(true) // Invalid parentage cannot certify a later replay.
	}
	state := o.progressStreams[key]
	if native.Type == "message_start" {
		if native.Message.ID == "" {
			o.discardStream(key)
			if o.results != 0 {
				o.markUncoveredStream(root)
			}
			return
		}
		id := streamIdentity{key, native.Message.ID}
		startKey := id.progressKey("stream_start")
		_, assistantSeen := o.assistantUsage[id]
		_, historicalAssistant := o.assistantHistory[id]
		replay := assistantSeen || historicalAssistant || o.progressSeen[id.progressKey("assistant")] || o.progressSeen[startKey]
		if state != nil && state.messageID != native.Message.ID && o.results != 0 {
			oldID := streamIdentity{key, state.messageID}
			if _, seen := o.assistantUsage[oldID]; !seen && o.streamedSinceAssistant[oldID] {
				o.markProvenanceGap(false) // Replaced content cannot certify a later result.
			}
		}
		if replay {
			if state != nil && state.messageID == native.Message.ID {
				return // Preserve an active stream; later content may be fresh.
			}
			o.discardStream(key)
			return // An old ID alone cannot prove that later tagged content is a replay.
		}
		// A fresh message replaces the old stream's transient signatures before
		// reserving its start, so proof no longer in use cannot exhaust the quota.
		o.discardStream(key)
		if !o.claimAssistantOrigin(e, native.Message.ID) {
			return
		}
		if !o.rememberProgress(startKey) {
			return
		}
		if o.results != 0 {
			o.markUncoveredStream(root)
		}
		if o.stopped && root {
			if o.rootStartsAfterStop == nil {
				o.rootStartsAfterStop = make(map[string]bool)
			}
			o.rootStartsAfterStop[native.Message.ID] = true
		}
		if o.progressStreams == nil {
			o.progressStreams = make(map[string]*progressStream)
		}
		state = &progressStream{key: key, messageID: native.Message.ID, resultIndex: o.results}
		o.progressStreams[key] = state
		o.observeStreamUsage(id, root, native.Message.Usage)
		return
	}
	if native.Type == "message_stop" {
		// Keep bounded stream proof through this invocation. A later identical
		// UUID/content can replay after the native stop event.
		if state != nil {
			state.stopped = true
		}
		return
	}
	if native.Type == "message_delta" {
		if state != nil && !state.stopped {
			o.observeStreamUsage(streamIdentity{key, state.messageID}, root, native.Usage)
		}
		return
	}
	if native.Type == "content_block_stop" && o.results != 0 && (native.Index == nil || *native.Index < 0) {
		o.markUncoveredStream(root)
		return
	}
	if state == nil {
		if o.results != 0 &&
			(native.Type == "content_block_start" ||
				native.Type == "content_block_delta" || native.Type == "content_block_stop") {
			o.markUncoveredStream(root)
		}
		return
	}
	if native.Index != nil {
		switch native.Type {
		case "content_block_start", "content_block_delta", "content_block_stop":
			state.activeIndex = max(state.activeIndex, *native.Index)
		}
	}
	switch native.Type {
	case "content_block_start":
		if block.Type != "tool_use" || native.Index == nil || *native.Index < 0 {
			if o.results != 0 {
				o.markUncoveredStream(root)
			}
			return
		}
		wrapped := make([]byte, 0, len(native.ContentBlock)+2)
		wrapped = append(wrapped, '[')
		wrapped = append(wrapped, native.ContentBlock...)
		wrapped = append(wrapped, ']')
		proof, valid, unprojectable := assistantContentProof(wrapped)
		valid = valid && !unprojectable
		tool := state.tools[*native.Index]
		if state.hasSealed && *native.Index <= state.sealedIndex || tool.status != 0 {
			if o.results != 0 && (!valid || !tool.hasProof || tool.proof != proof) {
				o.markProvenanceGap(root)
			}
			return
		}
		if o.results != 0 {
			if state.hasSealed {
				o.markProvenanceGap(root) // New tool content after a full frame revises settled work.
			} else {
				o.markUncoveredStream(root)
			}
		}
		if !o.reserveProgress(1, toolProofBytes) {
			return
		}
		o.transientEntries++
		o.transientBytes += toolProofBytes
		if state.tools == nil {
			state.tools = make(map[int]toolProgress)
		}
		tool.status, tool.proof, tool.hasProof = 1, proof, valid
		state.tools[*native.Index] = tool
		o.markStreamProgress(state)
	case "content_block_delta":
		var content string
		valid := true
		switch delta.Type {
		case "text_delta":
			valid = delta.Text != nil && delta.Thinking == nil && delta.PartialJSON == nil
			if delta.Text != nil {
				content = *delta.Text
			}
		case "thinking_delta":
			valid = delta.Thinking != nil && delta.Text == nil && delta.PartialJSON == nil
			if delta.Thinking != nil {
				content = *delta.Thinking
			}
		case "input_json_delta":
			valid = delta.PartialJSON != nil && delta.Text == nil && delta.Thinking == nil
			if delta.PartialJSON != nil {
				content = *delta.PartialJSON
			}
		case "signature_delta":
			valid = !hasDeltaContent // Signature changes are metadata, not new work.
		default:
			valid = false
		}
		if !valid {
			if o.results != 0 {
				o.markUncoveredStream(root)
			}
			return // A mixed payload is not an exact replay witness.
		}
		if o.results != 0 && hasDeltaContent && (native.Index == nil || *native.Index < 0) {
			o.markUncoveredStream(root)
			return // Missing position cannot prove a replay.
		}
		if native.Index == nil || *native.Index < 0 {
			return
		}
		index := *native.Index
		if state.hasSealed && index <= state.sealedIndex {
			if content != "" && o.results != 0 &&
				(e.UUID == "" || !state.deltas[streamDelta{uuid: e.UUID, index: index, kind: delta.Type, content: content}]) {
				o.markProvenanceGap(root)
			}
			return
		}
		if content != "" && o.markStreamDelta(state, e.UUID, index, delta.Type, content) && o.results != 0 {
			if state.hasSealed {
				o.markProvenanceGap(root) // A new block after a full frame is not an exact replay.
			} else {
				o.markUncoveredStream(root)
			}
		}
	case "content_block_stop":
		if o.results != 0 && state.tools[*native.Index].status == 0 {
			o.markUncoveredStream(root) // No proved start for this block.
			return
		}
		if native.Index != nil && state.tools[*native.Index].status == 1 {
			tool := state.tools[*native.Index]
			tool.status = 2
			state.tools[*native.Index] = tool
			if o.results != 0 && !(state.hasSealed && *native.Index <= state.sealedIndex) {
				o.markUncoveredStream(root)
			}
			o.markStreamProgress(state)
		}
	}
}

func (o *jsonlOutput) markStreamDelta(state *progressStream, uuid string, index int, kind, content string) bool {
	if uuid == "" {
		// An identical untagged adjacent frame cannot prove new progress.
		// shortcut: nonadjacent untagged replay is ambiguous, use native sequence IDs if available.
		proof := sha256.Sum256([]byte(strconv.Itoa(index) + ":" + strconv.Itoa(len(kind)) + ":" + kind + content))
		if state.lastWasNoUUID && state.lastDeltaProof == proof {
			return false
		}
		state.lastDeltaProof, state.lastWasNoUUID = proof, true
	} else {
		state.lastWasNoUUID = false
		// Keep distinct chunks if the CLI reuses a UUID within one message.
		key := streamDelta{uuid: uuid, index: index, kind: kind, content: content}
		if state.deltas[key] {
			return false
		}
		if !o.reserveProgress(1, len(uuid)+len(kind)+len(content)) {
			return false
		}
		state.signatureBytes += int64(len(uuid) + len(kind) + len(content))
		o.transientEntries++
		o.transientBytes += int64(len(uuid) + len(kind) + len(content))
		if state.deltas == nil {
			state.deltas = make(map[streamDelta]bool)
		}
		state.deltas[key] = true
	}
	o.markStreamProgress(state)
	return true
}

func (o *jsonlOutput) markStreamProgress(state *progressStream) {
	key := (streamIdentity{state.key, state.messageID}).progressKey("assistant")
	if !o.progressSeen[key] && !o.reserveProgress(1, len(key)) {
		return
	}
	if o.progressSeen == nil {
		o.progressSeen = make(map[string]bool)
	}
	o.progressSeen[key] = true
	if o.streamedSinceAssistant == nil {
		o.streamedSinceAssistant = make(map[streamIdentity]bool)
	}
	o.streamedSinceAssistant[streamIdentity{state.key, state.messageID}] = true
	if o.onProgress != nil {
		o.onProgress()
	}
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

func (o *jsonlOutput) finishTaskRun(id, runID string, credit bool) {
	key := taskRunKey(id, runID)
	if o.progressSeen["task_finish:"+key] {
		return
	}
	o.ensureTaskRuns()
	if credit && o.onProgress != nil {
		o.recordUniqueProgress("task_finish:" + key)
	} else {
		o.rememberProgress("task_finish:" + key)
	}
	if o.err != nil {
		return
	}
	delete(o.taskProgress, key)
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
	creditFinish := true
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
			// A tagged run is fresh proof even when an ID-only notice used this UUID.
		}
		creditFinish = !replay
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
			o.rememberProgress("runless_id:" + id)
			if o.err != nil {
				return
			}
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
		o.finishTaskRun(id, runID, creditFinish)
	}
}

func (o *jsonlOutput) recordTaskUsageProgress(e event) {
	if e.TaskID == "" {
		return
	}
	if e.RunID == "" {
		// ID-only usage proves pending work but cannot prove fresh work for the timer.
		o.rememberProgress("runless_id:" + e.TaskID)
		if o.err != nil {
			return
		}
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

func (o *jsonlOutput) ensureAssistantTotal() {
	if o.assistantTotalReady {
		return
	}
	for _, snapshot := range o.assistantHistory {
		if !snapshot.hasTokens {
			continue
		}
		total, valid := o.assistantTotal.AddChecked(snapshot.tokens)
		if !valid {
			o.assistantTotalOverflow = true
			break
		}
		o.assistantTotal = total
	}
	o.assistantTotalReady = true
}

func (o *jsonlOutput) retainUnallocatedUsage(raw json.RawMessage) {
	var reported eventUsage
	if json.Unmarshal(raw, &reported) != nil {
		return
	}
	if tokens, valid := reported.tokens(); valid {
		o.retainStandaloneFloor(tokens)
	}
}

func (o *jsonlOutput) retainStandaloneFloor(tokens usage.Tokens) {
	floor, ok := knownTokenFloor(o.result.Tokens, tokens)
	if !ok {
		floor = strongerTokenFloor(o.result.Tokens, tokens)
	}
	o.result.Tokens = floor // Unallocated or contradictory counters cannot be added to another message.
}

func (o *jsonlOutput) recordAssistantUsage(e event) (freshContent bool, messageID string, hasContent, replay bool) {
	if len(bytes.TrimSpace(e.Message)) == 0 && !reportPresent(e.Usage) && !reportPresent(e.ModelUsage) && e.TotalCostUSD == nil {
		return // A metadata-only assistant notice reports no message work.
	}
	var message struct {
		ID      string          `json:"id"`
		Usage   json.RawMessage `json:"usage"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &message) != nil {
		o.markProvenanceGap(true)
		return false, "", true, false // Malformed content cannot certify receipt coverage.
	}
	if message.ID == "" {
		if !o.resumed {
			o.retainUnallocatedUsage(message.Usage)
		}
		o.markProvenanceGap(true)
		return false, "", assistantReceiptOmitted(message.Content), false
	}
	messageID = message.ID
	streamKey, streamRoot := streamProgressKey(e)
	id := streamIdentity{streamKey, message.ID}
	// Only an explicit root assistant frame can assess a stopped task.
	root := e.parentForm == parentRoot
	unknown := e.parentForm == parentMissing || e.parentForm == parentInvalid
	prior, historical := o.assistantHistory[id]
	if o.resumed {
		// Starts and full frames can both be replayed by a resumed CLI. Only a
		// stored message with the same origin, counters and content is an exact
		// replay; new work needs a scoped final result for attribution.
		hasUsage := reportPresent(message.Usage)
		contentHash, hasContent, unprojectable := assistantContentProof(message.Content)
		replay := historical && hasUsage == prior.hasTokens && hasContent == prior.hasContent && !unprojectable &&
			!reportPresent(e.Usage) && !reportPresent(e.ModelUsage) && e.TotalCostUSD == nil
		if replay && hasUsage {
			var reported eventUsage
			if json.Unmarshal(message.Usage, &reported) != nil {
				replay = false
			} else {
				tokens, valid := reported.tokens()
				replay = valid && tokens == prior.tokens
			}
		}
		if replay && hasContent {
			contentKey := id.progressKey("assistant_content") + ":" + hex.EncodeToString(contentHash[:])
			replay = o.progressSeen[contentKey]
		}
		if !replay && (o.results != 0 || hasUsage || hasContent || unprojectable || reportPresent(e.Usage) ||
			reportPresent(e.ModelUsage) || e.TotalCostUSD != nil) {
			o.markProvenanceGap(root || unknown)
		}
		if replay {
			return false, message.ID, false, true
		}
		return false, "", assistantReceiptOmitted(message.Content), false
	}
	stream := o.progressStreams[streamKey]
	streamResultIndex, postStream := o.results, false
	if stream != nil && stream.messageID == message.ID {
		streamResultIndex = stream.resultIndex
		postStream = o.results != 0 && stream.resultIndex == o.results
	}
	currentStreamProof := o.streamedSinceAssistant[id]
	old, seen := o.assistantUsage[id]
	streamMatch := (stream != nil && stream.messageID == message.ID) || currentStreamProof || seen
	if o.progressSeen[id.progressKey("stream_start")] && !streamMatch {
		// A replaced stream key cannot prove this full frame belongs to its
		// active stream. Keep only its standalone floor on a fresh call.
		o.retainUnallocatedUsage(message.Usage)
		o.markProvenanceGap(root || unknown)
		return false, "", false, false
	}
	if !o.claimAssistantOrigin(e, message.ID) {
		return false, "", false, false
	}
	streamedCurrentMessage := !seen && !historical && currentStreamProof
	var rawUsage eventUsage
	valid := json.Unmarshal(message.Usage, &rawUsage) == nil
	var tokens usage.Tokens
	if valid {
		tokens, valid = rawUsage.tokens()
	}
	if streamedCurrentMessage {
		// The result may already cover this streamed message. Its full counters
		// still enter this call's per-message maxima, but not root addition.
		historical = true
	}
	priorTokens := prior.tokens
	if !seen && !historical && !o.reserveProgress(1, len(streamKey)+len(message.ID)) {
		return // The bounded history limit is a fatal output error.
	}
	unknownPrior := historical && !prior.hasTokens
	firstHistoricalCounters := valid && !streamedCurrentMessage && (unknownPrior && !seen || seen && old.priorMissing)
	if firstHistoricalCounters {
		// The first counters for an old ID may include work before this call.
		// Only later increases are attributable without a cumulative result.
		priorTokens = tokens
		old.prior = tokens
		old.priorMissing = false
	}
	contentHash, hasContent, unprojectable := assistantContentProof(message.Content)
	contentKey := ""
	newContent := false
	if hasContent {
		contentKey = id.progressKey("assistant_content") + ":" + hex.EncodeToString(contentHash[:])
		newContent = !o.progressSeen[contentKey]
	}
	if seen && valid && prior.hasTokens && tokens == old.tokens && hasContent && !newContent && !unprojectable &&
		!reportPresent(e.Usage) && !reportPresent(e.ModelUsage) && e.TotalCostUSD == nil {
		return false, message.ID, false, true // Exact accepted full-frame replay has no new receipt content.
	}
	if stream != nil && stream.messageID == message.ID && o.results > max(1, stream.resultIndex) {
		// The first result may reconcile a stream started before any result.
		// A first full frame after a later result has uncertain scope; an
		// identical accepted receipt is only a replay of settled work.
		if seen && valid && prior.hasTokens && tokens == prior.tokens && !newContent && !unprojectable {
			return false, message.ID, false, true
		}
		o.retainUnallocatedUsage(message.Usage)
		o.markProvenanceGap(root || unknown)
		return false, "", false, false
	}
	// An empty full frame cannot consume the stream's first content snapshot.
	firstStreamFrame := !prior.hasContent && currentStreamProof
	firstStreamContent := hasContent && firstStreamFrame
	if o.results != 0 && reportPresent(message.Usage) && !valid {
		defer o.markUncoveredStream(false) // Unusable late counters cannot certify cost.
	}
	if o.results != 0 && (unprojectable || firstStreamFrame && hasContent) {
		// A late frame with no exact content proof may contain work absent from
		// the stream. Apply the gap after collecting any known counters.
		defer o.markUncoveredStream(streamRoot)
	}
	if streamedCurrentMessage || firstStreamContent {
		if newContent && !o.rememberProgress(contentKey) {
			return
		}
		newContent = false
	}
	if hasContent {
		prior.hasContent = true
	}
	if firstStreamContent {
		if o.assistantHistory == nil {
			o.assistantHistory = make(map[streamIdentity]assistantSnapshot)
		}
		o.assistantHistory[id] = prior
	}
	if firstStreamContent && seen && len(message.Usage) == 0 {
		// An empty frame already registered this ID. Its first full content
		// adds no work or counters when usage is absent.
		return
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
		if seen && old.resultIndex < o.results {
			o.markProvenanceGap(root || unknown) // A changed block revises settled work.
		}
	}
	if valid {
		updated := prior.tokens.Max(tokens)
		if prior.hasTokens && !covers(tokens, prior.tokens) {
			o.markProvenanceGap(false)
			if updated.Valid() {
				if floor, ok := knownTokenFloor(prior.tokens, tokens); ok {
					updated = floor
				}
			} else {
				o.retainStandaloneFloor(prior.tokens)
				o.retainStandaloneFloor(tokens)
			}
		}
		if !o.perCallResults {
			o.ensureAssistantTotal()
			if !o.assistantTotalOverflow {
				remaining := o.assistantTotal.Sub(prior.tokens)
				total, ok := remaining.AddChecked(updated)
				if ok {
					o.assistantTotal = total
				} else {
					o.assistantTotalOverflow = true
				}
			}
		}
		prior.tokens, prior.hasTokens = updated, true
		if !updated.Valid() {
			// Keep an impossible counter floor in history so a resumed call
			// cannot certify later cumulative usage as exact.
			o.assistantTotalOverflow = true
			valid = false
		}
	}
	if o.assistantHistory == nil {
		o.assistantHistory = make(map[streamIdentity]assistantSnapshot)
	}
	o.assistantHistory[id] = prior
	if o.hasCumulative && !o.perCallResults && (o.assistantTotalOverflow || valid && !covers(o.cumulative, o.assistantTotal)) {
		if !o.assistantTotalOverflow {
			if lower := o.result.Tokens.Max(o.assistantTotal.Sub(o.baseline)); lower.Valid() {
				o.result.Tokens = lower
			}
		}
		o.hasCumulative, o.uncovered = false, true
		o.result.UsageComplete = false
		o.nativeCostUSD = nil // Later counters exceed the settled cost snapshot.
	}
	if firstHistoricalCounters && !newContent {
		// A newly revealed absolute floor is not evidence of current work.
		// Keep the answer; only later counter increases are attributable.
		if seen {
			o.assistantUsage[id] = old
		}
		return
	}
	if o.results != 0 && (newContent || !seen && !streamedCurrentMessage) {
		// A cost snapshot before new assistant work is only a known subtotal.
		o.nativeCostUSD = nil
	}
	if o.baselineUnknown || !seen && !streamedCurrentMessage {
		o.hasCumulative = false
	}
	if !seen {
		if o.assistantUsage == nil {
			o.assistantUsage = make(map[streamIdentity]messageUsage)
		}
		old = messageUsage{prior: priorTokens, priorMissing: unknownPrior && !valid,
			generation: o.aggregateGeneration, resultIndex: streamResultIndex, postStream: postStream,
			root: root, historical: historical}
		// Remember identities even before usable counters arrive. A later block from
		// an already covered message must not become a new billable call.
		o.assistantUsage[id] = old
		if !streamedCurrentMessage {
			o.uncovered = o.aggregateGeneration != 0
		}
		if root && !historical && (!o.stopped || o.rootStartsAfterStop[message.ID] ||
			!o.progressSeen[id.progressKey("stream_start")]) {
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
	if seen && old.resultIndex < o.results && tokens != old.tokens {
		// A later result may already include a revised receipt. Its standalone
		// counters cannot prove an additive delta to that settled result.
		o.markProvenanceGap(false)
	}
	if o.results != 0 && !streamedCurrentMessage && !covers(old.tokens, tokens) {
		o.nativeCostUSD = nil
	}
	total, valid := o.assistantTokens.Sub(old.tokens).AddChecked(tokens)
	if !valid {
		o.markProvenanceGap(false)
		floor := strongerTokenFloor(o.result.Tokens, o.assistantTokens)
		if merged, ok := knownTokenFloor(tokens, floor); ok {
			o.result.Tokens = strongerTokenFloor(floor, merged)
		} else {
			o.result.Tokens = strongerTokenFloor(floor, tokens)
		}
		markUsageUncertain()
		return
	}
	if old.root && old.generation == o.aggregateGeneration {
		if old.historical {
			// A late increase on an ID from an earlier call may already be in
			// this result. Keep its delta as a floor without adding it again.
			markUsageUncertain()
		} else {
			rootTotal, valid := o.rootTokens.Sub(old.tokens).AddChecked(tokens)
			if !valid {
				markUsageUncertain()
				return
			}
			o.rootTokens = rootTotal
		}
	}
	o.assistantTokens = total
	if o.results != 0 && !covers(o.result.Tokens, total) {
		o.uncovered, o.result.UsageComplete = true, false
		o.nativeCostUSD = nil
	}
	old.tokens = tokens
	o.assistantUsage[id] = old
	if o.hasCumulative {
		lower, valid := o.assistantLowerBound()
		o.hasCumulative = valid && covers(o.cumulative.Sub(o.baseline), lower)
	}
	return
}

// assistantReceiptOmitted mirrors captureMessage's assistant content boundary:
// thinking is private, while any other block or malformed content needs a
// receipt marker when a resumed frame cannot be attributed.
func assistantReceiptOmitted(raw json.RawMessage) bool {
	content := bytes.TrimSpace(raw)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return false
	}
	if _, _, unprojectable := assistantContentProof(content); unprojectable {
		return true
	}
	if content[0] == '"' {
		var text string
		return json.Unmarshal(content, &text) != nil || text != ""
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return true
	}
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &block) != nil {
			return true
		}
		if block.Type == "thinking" || block.Type == "redacted_thinking" || block.Type == "text" && block.Text == "" {
			continue
		}
		return true
	}
	return false
}

func assistantContentProof(raw json.RawMessage) ([32]byte, bool, bool) {
	content := bytes.TrimSpace(raw)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return [32]byte{}, false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return [32]byte{}, false, true
	}
	unprojectable := false
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return [32]byte{}, false, false
		}
		value = []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		if len(typed) == 0 {
			return [32]byte{}, false, false
		}
		// Block metadata can change on replay without new model work.
		projected := make([]any, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				unprojectable = true
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
				if block["text"] != nil || block["thinking"] != nil || block["data"] != nil || block["partial_json"] != nil || block["content"] != nil {
					unprojectable = true
				}
				id, ok := block["id"].(string)
				if !ok || id == "" {
					unprojectable = true
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
				unprojectable = true
				continue
			}
			for _, extra := range []string{"text", "thinking", "data", "name", "input", "partial_json", "content"} {
				if extra != field && block[extra] != nil {
					unprojectable = true
				}
			}
			if text, ok := block[field].(string); ok && text != "" {
				projected = append(projected, map[string]any{"type": block["type"], field: text})
			} else if !ok {
				unprojectable = true
			}
		}
		if len(projected) == 0 {
			return [32]byte{}, false, unprojectable
		}
		value = projected
	default:
		return [32]byte{}, false, true // Native content is a string or list of known blocks.
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, false, true
	}
	return sha256.Sum256(canonical), true, unprojectable
}

func (o *jsonlOutput) assistantLowerBound() (usage.Tokens, bool) {
	tokens := o.result.Tokens.Max(o.assistantTokens)
	if o.aggregateGeneration != 0 {
		// Root responses follow the preceding result in the native stream.
		// Unknown/delegate messages may arrive late and are never added to it.
		root, valid := o.aggregateTokens.AddChecked(o.rootTokens)
		if !valid {
			floor := strongerTokenFloor(o.result.Tokens, o.assistantTokens)
			floor = strongerTokenFloor(floor, o.aggregateTokens)
			floor = strongerTokenFloor(floor, o.rootTokens)
			if combined, ok := knownTokenFloor(floor, o.assistantTokens); ok {
				floor = combined // Input and output can be independently proved.
			}
			return floor, false
		}
		tokens = tokens.Max(root)
	}
	return tokens, tokens.Valid()
}

func covers(a, b usage.Tokens) bool {
	return a.Valid() && b.Valid() && a.Max(b) == a &&
		a.Input-a.CacheCreationInput-a.CachedInput >= b.Input-b.CacheCreationInput-b.CachedInput
}

func isWaitCeilingStop(line []byte) bool {
	// Managed CLI settings can override its launch environment. Match this
	// diagnostic independently of the separately retained stderr tail.
	line = bytes.TrimRight(line, " \t\r")
	return bytes.HasPrefix(line, []byte("Background tasks still running ")) && bytes.HasSuffix(line, []byte("; stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."))
}
