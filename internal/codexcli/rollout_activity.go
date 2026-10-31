package codexcli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/linkdata/minion/internal/aicli"
)

// rolloutActivity observes only this call's root and announced helpers. Stdout
// remains the source of root discussion; rollout cursors supply helper content
// and progress when the root stream is quiet.
type rolloutActivity struct {
	ctx      context.Context
	home     string
	started  time.Time
	known    map[string]rolloutMeta
	before   map[string]*usageBoundary
	excluded map[string]bool
	emit     func(aicli.StreamEvent)
	progress func()

	mu        sync.Mutex // stdout announcements may arrive while a scan runs
	rootID    string
	announced map[string]bool
	spawned   map[string]bool
	refresh   bool
	overflow  bool

	scanMu            sync.Mutex
	files             map[string]rolloutMeta
	ambiguous         map[string]bool
	lastDiscover      time.Time
	discoveryFailed   bool
	discoveryFailures int
	cursors           map[string]*rolloutActivityCursor
	prefixFailed      map[string]bool
	missing           map[string]bool
	scanned           int64
	pendingBytes      int64
	stop              chan struct{}
	done              chan struct{}
	startOnce         sync.Once
	finishOnce        sync.Once
}

type rolloutActivityCursor struct {
	meta     rolloutMeta
	file     os.FileInfo
	offset   int64
	pending  []byte
	dropping bool
}

const maxActivityPendingBytes = 8 << 20
const maxActivityDiscoveryFailures = 3

func newRolloutActivity(ctx context.Context, home string, started time.Time, known map[string]rolloutMeta, before map[string]*usageBoundary, excluded map[string]bool, emit func(aicli.StreamEvent), progress func()) *rolloutActivity {
	return &rolloutActivity{
		ctx: ctx, home: home, started: started, known: maps.Clone(known), before: maps.Clone(before),
		excluded: maps.Clone(excluded), emit: emit, progress: progress,
		announced: map[string]bool{}, spawned: map[string]bool{},
		cursors: map[string]*rolloutActivityCursor{}, prefixFailed: map[string]bool{}, missing: map[string]bool{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

func (a *rolloutActivity) root(id string) {
	if id == "" || len(id) > maxItemIDBytes {
		a.mu.Lock()
		a.overflow = true
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	if a.rootID != id {
		a.rootID, a.refresh = id, true
	}
	a.mu.Unlock()
}

// A wait/close observation can reactivate an owned resumed helper. New helper
// files require a completed current spawn, not merely an observed receiver ID.
func (a *rolloutActivity) child(id string, spawned bool) {
	if id == "" || len(id) > maxItemIDBytes {
		a.mu.Lock()
		a.overflow = true
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	if len(a.announced) < maxCodexChildren || a.announced[id] {
		if !a.announced[id] || spawned && !a.spawned[id] {
			a.refresh = true
		}
		a.announced[id] = true
		if spawned {
			a.spawned[id] = true
		}
	} else {
		a.overflow = true
	}
	a.mu.Unlock()
}

func (a *rolloutActivity) start() {
	a.startOnce.Do(func() {
		go func() {
			defer close(a.done)
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					a.poll()
				case <-a.stop:
					return
				case <-a.ctx.Done():
					return
				}
			}
		}()
	})
}

// finish is safe both after start and in deterministic synchronous tests.
func (a *rolloutActivity) finish() {
	a.finishOnce.Do(func() {
		close(a.stop)
		a.startOnce.Do(func() { close(a.done) })
		<-a.done
		a.mu.Lock()
		a.refresh = true // The process may have created a helper after the last discovery.
		a.mu.Unlock()
		for range maxUsageAgents {
			a.poll() // Newly observed nested spawns can require another discovery.
			a.mu.Lock()
			refresh := a.refresh
			a.mu.Unlock()
			if !refresh {
				break
			}
		}
		for id, cursor := range a.cursors {
			if len(cursor.pending) != 0 || cursor.dropping {
				a.contentMissing(id)
			}
		}
		a.mu.Lock()
		spawned, rootID, overflow := maps.Clone(a.spawned), a.rootID, a.overflow
		a.mu.Unlock()
		for id := range spawned {
			if _, captured := a.cursors[id]; !captured {
				a.contentMissing(id) // Missing, excluded, or ambiguous current helper.
			}
		}
		if overflow {
			a.contentMissing(rootID)
		}
	})
}

// poll is synchronous so file cursors and the final pass have one ordering.
func (a *rolloutActivity) poll() {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	if a.ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	rootID := a.rootID
	refresh := a.refresh
	announced, spawned := maps.Clone(a.announced), maps.Clone(a.spawned)
	a.refresh = false
	a.mu.Unlock()
	if rootID == "" {
		return
	}
	periodic := !a.discoveryFailed && time.Since(a.lastDiscover) >= 5*time.Second && a.needsDiscovery(rootID, announced, spawned)
	if refresh || a.files == nil || periodic {
		files, ambiguous, complete := usageFiles(a.ctx, a.home, rootID, a.started, a.known)
		a.files, a.ambiguous, a.lastDiscover = files, ambiguous, time.Now()
		if !complete {
			if _, err := os.Stat(filepath.Join(a.home, "sessions")); !errors.Is(err, os.ErrNotExist) {
				a.discoveryFailures++
				a.discoveryFailed = a.discoveryFailures >= maxActivityDiscoveryFailures
				if _, rootFound := files[rootID]; rootFound || len(spawned) != 0 {
					a.contentMissing(rootID)
				}
			}
		} else {
			a.discoveryFailures, a.discoveryFailed = 0, false
		}
	}
	files, ambiguous := a.files, a.ambiguous
	for id := range ambiguous {
		if id == rootID || spawned[id] || a.cursors[id] != nil {
			a.contentMissing(id) // A second path invalidates even an earlier live selection.
		}
	}
	progressed := false
	selected := map[string]bool{rootID: true} // Stdout identifies the root even before its rollout appears.
	if meta, ok := files[rootID]; ok && !ambiguous[rootID] && !meta.duplicate && meta.ParentID == "" && (meta.SessionID == "" || meta.SessionID == rootID) {
		a.scan(meta, rootID, &progressed)
	}
	for changed := true; changed; {
		changed = false
		a.mu.Lock()
		ids := slices.Sorted(maps.Keys(a.announced))
		announced, spawned := maps.Clone(a.announced), maps.Clone(a.spawned)
		a.mu.Unlock()
		for _, id := range ids {
			if selected[id] || !announced[id] || ambiguous[id] || len(selected) >= maxUsageAgents {
				continue
			}
			meta, ok := files[id]
			if !ok || meta.duplicate || meta.ID == rootID || meta.SessionID != "" && meta.SessionID != rootID {
				continue
			}
			if meta.ParentID != "" && !selected[meta.ParentID] || meta.ParentID == "" && meta.SessionID != rootID {
				continue
			}
			_, wasKnown := a.known[id]
			if !wasKnown && !spawned[id] {
				continue
			}
			selected[id], changed = true, true
			a.scan(meta, rootID, &progressed)
		}
	}
	if progressed && a.progress != nil {
		a.progress()
	}
}

func (a *rolloutActivity) needsDiscovery(rootID string, announced, spawned map[string]bool) bool {
	if _, found := a.files[rootID]; !found || a.ambiguous[rootID] {
		return true
	}
	for id := range announced {
		if !spawned[id] && a.known[id].ID == "" {
			continue // An observation does not require a new helper rollout.
		}
		if _, found := a.files[id]; !found || a.ambiguous[id] {
			return true
		}
	}
	return false
}

func (a *rolloutActivity) scan(meta rolloutMeta, rootID string, progressed *bool) {
	if a.prefixFailed[meta.ID] || meta.path == "" || a.excluded[meta.path] && a.known[meta.ID].path != meta.path {
		return
	}
	if meta.HistoryMode != "" && meta.HistoryMode != "legacy" && meta.HistoryMode != "paginated" {
		a.contentMissing(meta.ID) // A future persistence mode may omit response_item.
	}
	info, err := rolloutFileInfo(a.home, meta.path)
	if err != nil {
		a.contentMissing(meta.ID)
		return
	}
	cursor := a.cursors[meta.ID]
	if cursor == nil {
		f, err := os.Open(meta.path)
		if err != nil {
			a.contentMissing(meta.ID)
			return
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
			_ = f.Close()
			a.contentMissing(meta.ID)
			return
		}
		cursor = &rolloutActivityCursor{meta: meta, file: opened}
		if _, known := a.known[meta.ID]; known {
			baseline := a.before[meta.ID]
			if baseline == nil || !baseline.valid || !baseline.prefixKnown || baseline.size > maxUsageRolloutBytes || !matchUsagePrefix(a.ctx, f, baseline, sha256.New()) {
				_ = f.Close()
				a.prefixFailed[meta.ID] = true
				a.contentMissing(meta.ID)
				return
			}
			cursor.offset = baseline.size
		}
		_ = f.Close()
		a.cursors[meta.ID] = cursor
	} else if cursor.meta.path != meta.path || !sameRolloutMeta(cursor.meta, meta) || !os.SameFile(cursor.file, info) || info.Size() < cursor.offset {
		a.contentMissing(meta.ID)
		return
	}
	if info.Size() == cursor.offset {
		return // No complete or partial bytes have arrived since the last scan.
	}
	if cursor.offset >= maxUsageRolloutBytes || a.scanned >= maxUsageScanBytes {
		if info.Size() > cursor.offset {
			a.contentMissing(meta.ID)
		}
		return
	}
	f, err := os.Open(meta.path)
	if err != nil {
		a.contentMissing(meta.ID)
		return
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		a.contentMissing(meta.ID)
		return
	}
	if _, err = f.Seek(cursor.offset, io.SeekStart); err != nil {
		_ = f.Close()
		a.contentMissing(meta.ID)
		return
	}
	limit := min(opened.Size()-cursor.offset, maxUsageRolloutBytes-cursor.offset, maxUsageScanBytes-a.scanned)
	if limit <= 0 {
		_ = f.Close()
		return
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, limit), 64<<10)
	for {
		chunk, readErr := reader.ReadSlice('\n')
		cursor.offset += int64(len(chunk))
		a.scanned += int64(len(chunk))
		if !cursor.dropping {
			if len(cursor.pending)+len(chunk) <= aicli.MaxEventBytes && a.pendingBytes+int64(len(chunk)) <= maxActivityPendingBytes {
				cursor.pending = append(cursor.pending, chunk...)
				a.pendingBytes += int64(len(chunk))
			} else {
				a.pendingBytes -= int64(len(cursor.pending))
				cursor.pending, cursor.dropping = nil, true
				a.contentMissing(meta.ID)
			}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			a.contentMissing(meta.ID)
			break
		}
		if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' {
			break // Retain only bounded unfinished bytes for the next poll.
		}
		if !cursor.dropping {
			a.consume(meta, rootID, cursor.pending[:len(cursor.pending)-1], progressed)
		}
		a.pendingBytes -= int64(len(cursor.pending))
		cursor.pending, cursor.dropping = nil, false
		if readErr == io.EOF {
			break
		}
	}
	if err := f.Close(); err != nil {
		a.contentMissing(meta.ID)
	}
	if opened.Size() > cursor.offset && (cursor.offset >= maxUsageRolloutBytes || a.scanned >= maxUsageScanBytes) {
		a.contentMissing(meta.ID)
	}
}

func (a *rolloutActivity) consume(meta rolloutMeta, rootID string, line []byte, progressed *bool) {
	var event rolloutEvent
	if json.Unmarshal(line, &event) != nil {
		a.contentMissing(meta.ID)
		return
	}
	if event.Type == "session_meta" {
		return
	}
	uncertainOrdinal := false
	if meta.ID != rootID && meta.HistoryStart != nil {
		if event.Ordinal <= 0 && *meta.HistoryStart > 0 {
			uncertainOrdinal = true
			a.contentMissing(meta.ID) // A missing ordinal cannot prove fork ownership.
		} else if event.Ordinal < *meta.HistoryStart {
			return
		}
	}
	if event.Type == "response_item" {
		if meta.ID == rootID {
			return // Root stdout supplies discussion.
		}
		if a.consumeResponse(meta, event.Payload, uncertainOrdinal) && !uncertainOrdinal {
			*progressed = true
		}
		return
	}
	if event.Type == "inter_agent_communication_metadata" {
		return
	}
	if event.Type == "inter_agent_communication" {
		if meta.ID == rootID {
			return // Root stdout supplies its own tool discussion.
		}
		var message struct {
			Author, Recipient, Content string
			EncryptedContent           string `json:"encrypted_content"`
			Metadata                   struct {
				TurnID string `json:"turn_id"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if json.Unmarshal(event.Payload, &message) != nil {
			a.contentMissing(meta.ID)
			return
		}
		before := a.before[meta.ID]
		if before != nil && message.Metadata.TurnID != "" && before.turns[message.Metadata.TurnID] {
			return
		}
		if message.Author == "" || message.Recipient == "" || message.EncryptedContent != "" {
			a.contentMissing(meta.ID)
		}
		if message.Content != "" {
			uncertain := uncertainOrdinal || before != nil && message.Metadata.TurnID == "" || meta.HistoryStart == nil && before == nil
			a.emitVisible(meta.ID, "inter_agent_communication", message.Author+" -> "+message.Recipient+"\n"+message.Content, uncertain)
		}
		if !uncertainOrdinal && (message.Content != "" || message.EncryptedContent != "") {
			*progressed = true
		}
		return
	}
	if event.Type != "event_msg" {
		switch event.Type {
		case "turn_context", "token_usage_record", "world_state", "compacted":
			return // Known metadata, not visible assistant discussion.
		default:
			if meta.ID != rootID {
				a.contentMissing(meta.ID)
			}
			return
		}
	}
	var payload struct {
		Type        string          `json:"type"`
		ThreadID    string          `json:"thread_id"`
		TurnID      string          `json:"turn_id"`
		NewThreadID string          `json:"new_thread_id"`
		AgentID     string          `json:"agent_thread_id"`
		Kind        string          `json:"kind"`
		Message     string          `json:"message"`
		Text        string          `json:"text"`
		Item        json.RawMessage `json:"item"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		a.contentMissing(meta.ID)
		return
	}
	if payload.ThreadID != "" && payload.ThreadID != meta.ID {
		return
	}
	if b := a.before[meta.ID]; b != nil && payload.TurnID != "" && b.turns[payload.TurnID] {
		return
	}
	if !uncertainOrdinal && (a.before[meta.ID] == nil || payload.TurnID != "" || meta.ID == rootID && payload.Type == "task_started") {
		*progressed = true
	}
	if payload.Type == "collab_agent_spawn_end" {
		if a.spawnAttributable(meta, rootID, uncertainOrdinal) {
			a.rolloutSpawn(meta, payload.TurnID, payload.NewThreadID)
		}
		return
	}
	if payload.Type == "sub_agent_activity" {
		switch payload.Kind {
		case "started":
			if a.spawnAttributable(meta, rootID, uncertainOrdinal) {
				a.rolloutSpawn(meta, payload.TurnID, payload.AgentID)
			}
		case "interacted", "interrupted", "completed":
			if a.spawnAttributable(meta, rootID, uncertainOrdinal) {
				a.rolloutObserve(meta, payload.TurnID, payload.AgentID)
			}
		default:
			a.contentMissing(meta.ID)
		}
		return
	}
	if meta.ID != rootID && meta.HistoryMode != "paginated" {
		uncertain := uncertainOrdinal || a.before[meta.ID] != nil && payload.TurnID == "" || meta.HistoryStart == nil && a.before[meta.ID] == nil
		switch payload.Type {
		case "user_message":
			var input struct {
				Images      []json.RawMessage
				FileIDs     []json.RawMessage `json:"file_ids"`
				LocalImages []json.RawMessage `json:"local_images"`
				Audio       []json.RawMessage
				LocalAudio  []json.RawMessage `json:"local_audio"`
			}
			if json.Unmarshal(event.Payload, &input) != nil || len(input.Images)+len(input.FileIDs)+len(input.LocalImages)+len(input.Audio)+len(input.LocalAudio) != 0 {
				a.contentMissing(meta.ID) // Non-text attachments are not retained in discussion records.
			}
			a.emitVisible(meta.ID, "user_event_msg", payload.Message, uncertain)
			return
		case "agent_message":
			var output struct {
				Questions []json.RawMessage
			}
			if json.Unmarshal(event.Payload, &output) != nil || len(output.Questions) != 0 {
				a.contentMissing(meta.ID) // Structured questions are separate from message text.
			}
			a.emitVisible(meta.ID, "assistant_event_msg", payload.Message, uncertain)
			return
		case "agent_reasoning":
			a.emitVisible(meta.ID, "reasoning_summary_event_msg", payload.Text, uncertain)
			return
		case "patch_apply_end", "mcp_tool_call_end", "web_search_end", "image_generation_end":
			a.emitVisible(meta.ID, "tool_event_msg", string(event.Payload), uncertain)
			return
		}
	}
	if payload.Type != "item_completed" && payload.Type != "item_started" && payload.Type != "item_updated" || len(payload.Item) == 0 {
		switch payload.Type {
		case "task_started", "task_complete", "turn_aborted", "token_count", "thread_goal_updated", "thread_rolled_back", "thread_settings_applied", "entered_review_mode", "exited_review_mode", "context_compacted", "agent_reasoning_raw_content":
			return // Lifecycle/metadata or raw reasoning, never visible discussion.
		default:
			if meta.ID != rootID {
				a.contentMissing(meta.ID)
			}
			return
		}
	}
	var item struct {
		ID          string   `json:"id"`
		Type        string   `json:"type"`
		Kind        string   `json:"kind"`
		AgentID     string   `json:"agent_thread_id"`
		Tool        string   `json:"tool"`
		Status      string   `json:"status"`
		ReceiverIDs []string `json:"receiver_thread_ids"`
		Content     []struct {
			Type, Text string
		} `json:"content"`
		Summary []string `json:"summary_text"`
	}
	if json.Unmarshal(payload.Item, &item) != nil {
		a.contentMissing(meta.ID)
		return
	}
	if meta.ID != rootID && (payload.Type == "item_started" || payload.Type == "item_updated") {
		a.contentMissing(meta.ID) // Native persistence does not include partial item events.
		return
	}
	if payload.Type == "item_completed" && item.Type == "SubAgentActivity" && a.spawnAttributable(meta, rootID, uncertainOrdinal) {
		switch item.Kind {
		case "started":
			a.rolloutSpawn(meta, payload.TurnID, item.AgentID)
		case "interacted", "interrupted", "completed":
			a.rolloutObserve(meta, payload.TurnID, item.AgentID)
		default:
			a.contentMissing(meta.ID)
		}
	}
	if item.Type == "CollabAgentToolCall" && item.Tool == "spawn_agent" && item.Status == "completed" && a.spawnAttributable(meta, rootID, uncertainOrdinal) {
		for _, id := range item.ReceiverIDs {
			a.rolloutSpawn(meta, payload.TurnID, id)
		}
	}
	if meta.ID == rootID {
		return // Root stdout is canonical.
	}
	if meta.HistoryMode != "paginated" {
		// Legacy persists only a few completed item types. A visible
		// paginated-only item under a legacy header cannot be reconciled.
		switch item.Type {
		case "AgentMessage", "Reasoning", "CommandExecution", "DynamicToolCall", "CollabAgentToolCall", "WebSearch", "FileChange", "McpToolCall", "ImageView", "ImageGeneration":
			a.contentMissing(meta.ID)
		}
		return
	}
	uncertain := uncertainOrdinal || a.before[meta.ID] != nil && payload.TurnID == "" || meta.HistoryStart == nil && a.before[meta.ID] == nil
	kind, text := "", ""
	switch item.Type {
	case "SubAgentActivity", "Plan", "TodoList", "ContextCompaction":
		return // Lifecycle data are not discussion.
	case "UserMessage", "AgentMessage":
		kind = "assistant"
		if item.Type == "UserMessage" {
			kind = "user"
		}
		var parts []string
		for _, content := range item.Content {
			if content.Type != "Text" && content.Type != "text" {
				a.contentMissing(meta.ID)
				continue
			}
			parts = append(parts, content.Text)
		}
		text = strings.Join(parts, "\n")
	case "Reasoning":
		kind, text = "reasoning_summary", strings.Join(item.Summary, "\n")
	case "CommandExecution", "DynamicToolCall", "CollabAgentToolCall", "WebSearch", "FileChange", "McpToolCall", "FunctionCallOutput", "ImageView", "ImageGeneration", "Extension":
		kind, text = "tool", string(payload.Item)
	default:
		a.contentMissing(meta.ID) // Unknown completed item may contain visible data.
		return
	}
	a.emitVisible(meta.ID, kind, text, uncertain)
}

func (a *rolloutActivity) spawnAttributable(meta rolloutMeta, rootID string, uncertainOrdinal bool) bool {
	if meta.ID == rootID {
		return true
	}
	if uncertainOrdinal || meta.HistoryStart == nil && a.before[meta.ID] == nil {
		a.contentMissing(meta.ID)
		return false
	}
	return true
}

func (a *rolloutActivity) consumeResponse(meta rolloutMeta, raw json.RawMessage, uncertainOrdinal bool) bool {
	var item struct {
		Type, Role, Author, Recipient string
		Metadata                      struct {
			TurnID string `json:"turn_id"`
		} `json:"internal_chat_message_metadata_passthrough"`
		Content []struct {
			Type, Text string
		} `json:"content"`
		Summary []struct {
			Type, Text string
		} `json:"summary"`
	}
	if json.Unmarshal(raw, &item) != nil {
		a.contentMissing(meta.ID)
		return false
	}
	before := a.before[meta.ID]
	if before != nil && item.Metadata.TurnID != "" && before.turns[item.Metadata.TurnID] {
		return false
	}
	uncertain := uncertainOrdinal || before != nil && item.Metadata.TurnID == "" || meta.HistoryStart == nil && before == nil
	kind, text := "", ""
	switch item.Type {
	case "message":
		contentType := "output_text"
		switch item.Role {
		case "assistant":
			kind = "assistant"
		case "user":
			kind, contentType = "user", "input_text"
		default:
			return false // Developer and tool inputs are outside discussion capture.
		}
		var parts []string
		for _, content := range item.Content {
			if content.Type != contentType {
				a.contentMissing(meta.ID)
				continue
			}
			parts = append(parts, content.Text)
		}
		text = strings.Join(parts, "\n")
		if len(item.Content) != 0 && text == "" {
			a.contentMissing(meta.ID)
		}
	case "reasoning":
		kind = "reasoning_summary"
		var parts []string
		for _, summary := range item.Summary {
			if summary.Type != "summary_text" {
				a.contentMissing(meta.ID)
				continue
			}
			parts = append(parts, summary.Text)
		}
		text = strings.Join(parts, "\n") // Never inspect raw or encrypted reasoning.
	case "agent_message":
		kind = "inter_agent_message"
		var parts []string
		for _, content := range item.Content {
			if content.Type != "input_text" {
				a.contentMissing(meta.ID) // Encrypted or unknown content is unavailable.
				continue
			}
			parts = append(parts, content.Text)
		}
		if item.Author == "" || item.Recipient == "" {
			a.contentMissing(meta.ID)
		}
		if len(parts) != 0 {
			text = item.Author + " -> " + item.Recipient + "\n" + strings.Join(parts, "\n")
		}
	case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "local_shell_call", "web_search_call", "computer_call", "tool_search_call", "tool_search_output", "image_generation_call":
		kind, text = "tool", string(raw)
	case "additional_tools", "configuration_update", "compaction", "context_compaction":
		return false // Agent input and session metadata are not helper discussion.
	default:
		a.contentMissing(meta.ID)
		return false
	}
	if meta.HistoryMode == "paginated" {
		// Paginated response and completed-item records are independently
		// persisted. Both are exposed; neither proves the other was emitted.
		kind += "_response_item"
	}
	a.emitVisible(meta.ID, kind, text, uncertain)
	return true
}

func (a *rolloutActivity) emitVisible(id, kind, text string, uncertain bool) {
	if kind == "" || text == "" {
		return
	}
	if len(text) > aicli.MaxEventBytes || kind == "tool" && !json.Valid([]byte(text)) {
		a.contentMissing(id)
		return
	}
	if uncertain {
		kind += "_possibly_duplicate"
		a.contentMissing(id)
	}
	if a.emit != nil {
		a.emit(aicli.StreamEvent{NativeID: id, Kind: kind, Text: text})
	}
}

func (a *rolloutActivity) rolloutSpawn(meta rolloutMeta, turnID, id string) {
	if id == "" || len(id) > maxItemIDBytes {
		a.contentMissing(meta.ID)
		return
	}
	// On resume, a late old event without a turn cannot prove a current spawn.
	if b := a.before[meta.ID]; b != nil && turnID == "" {
		a.mu.Lock()
		current := a.spawned[id]
		a.mu.Unlock()
		if !current {
			a.contentMissing(meta.ID)
			return
		}
	}
	a.child(id, true)
}

func (a *rolloutActivity) rolloutObserve(meta rolloutMeta, turnID, id string) {
	if id == "" || len(id) > maxItemIDBytes {
		a.contentMissing(meta.ID)
		return
	}
	a.mu.Lock()
	spawned := a.spawned[id]
	a.mu.Unlock()
	if !spawned && a.known[id].ID == "" || a.before[meta.ID] != nil && turnID == "" {
		a.contentMissing(meta.ID) // Observation alone cannot prove a new or resumed spawn.
	}
	a.child(id, false)
}

func (a *rolloutActivity) contentMissing(id string) {
	if a.missing[id] {
		return
	}
	a.missing[id] = true
	if a.emit != nil {
		a.emit(aicli.StreamEvent{NativeID: id, Kind: "content_missing"})
	}
}
