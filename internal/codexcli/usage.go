package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/usage"
)

const (
	maxUsageWalkEntries  = 100000
	maxUsageFiles        = 1024
	maxUsageAgents       = maxCodexChildren + 1
	maxUsageHeaderBytes  = 64 << 10
	maxUsageRolloutBytes = 128 << 20
	maxUsageScanBytes    = 512 << 20
)

type rolloutMeta struct {
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	ParentID     string          `json:"parent_thread_id"`
	HistoryStart *int64          `json:"subagent_history_start_ordinal"`
	Source       json.RawMessage `json:"source"`
	path         string
}

type rolloutEvent struct {
	Type    string          `json:"type"`
	Ordinal int64           `json:"ordinal"`
	Payload json.RawMessage `json:"payload"`
}

// collectUsage reads only new rollout headers until it has identified this
// action's agent tree. Full transcripts of unrelated sessions are never read.
func collectUsage(ctx context.Context, home, rootID string, started time.Time, fallback *usage.Tokens, expectedChildren []string) (usage.Tokens, bool, error) {
	var total usage.Tokens
	if rootID == "" || home == "" || ctx.Err() != nil {
		if fallback != nil {
			total = *fallback
		}
		return total, false, nil
	}
	complete := true
	files := map[string]rolloutMeta{}
	entries, candidates := 0, 0
	err := walkUsageFiles(ctx, filepath.Join(home, "sessions"), func(path string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > maxUsageWalkEntries {
			complete = false
			return fs.SkipAll
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			complete = false
			return nil
		}
		if info.ModTime().Before(started.Add(-time.Minute)) {
			return nil
		}
		candidates++
		if candidates > maxUsageFiles {
			complete = false
			return fs.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			complete = false
			return nil
		}
		scanner := rolloutScanner(ctx, io.LimitReader(f, maxUsageHeaderBytes+1), nil)
		var event rolloutEvent
		var meta rolloutMeta
		ok := scanner.Scan() && len(scanner.Bytes()) <= maxUsageHeaderBytes && json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "session_meta" && json.Unmarshal(event.Payload, &meta) == nil && meta.ID != ""
		ok = ok && scanner.Err() == nil
		if err := f.Close(); err != nil {
			complete = false
		}
		if !ok {
			complete = false
			return nil
		}
		if meta.ParentID == "" {
			var source struct {
				Subagent struct {
					Spawn struct {
						ParentID string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			}
			if json.Unmarshal(meta.Source, &source) == nil {
				meta.ParentID = source.Subagent.Spawn.ParentID
			}
		}
		if _, exists := files[meta.ID]; exists {
			complete = false
			return nil
		}
		meta.path = path
		files[meta.ID] = meta
		return nil
	})
	if err != nil {
		complete = false
	}
	if ctx.Err() != nil {
		if fallback != nil {
			return *fallback, false, nil
		}
		return total, false, nil
	}
	selected := map[string]bool{rootID: true}
	for changed := true; changed; {
		changed = false
		for id, meta := range files {
			if !selected[id] && (meta.SessionID == rootID || selected[meta.ParentID]) {
				selected[id], changed = true, true
			}
		}
	}
	var childError error
	if len(selected) > maxUsageAgents {
		complete = false
		childError = fmt.Errorf("%w: Codex usage agent count exceeded the limit", aicli.ErrOutputLimit)
	}
	for _, id := range expectedChildren {
		if !selected[id] {
			complete = false
		}
	}
	ids := make([]string, 0, len(selected)-1)
	for id := range selected {
		if id != rootID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	ids = append([]string{rootID}, ids[:min(len(ids), maxCodexChildren)]...)
	budget := int64(maxUsageScanBytes)
	rootCounted := false
	for _, id := range ids {
		if ctx.Err() != nil {
			complete = false
			break
		}
		if id == rootID {
			rootCounted = true
		}
		meta, exists := files[id]
		if !exists {
			complete = false
			if id == rootID && fallback != nil {
				total.Add(*fallback)
			}
			continue
		}
		tokens, ok, children, workError := readRolloutUsage(ctx, meta, rootID, &budget)
		childError = errors.Join(childError, workError)
		for _, child := range children {
			if _, exists := files[child]; !exists || !selected[child] {
				ok = false
			}
		}
		if id == rootID && fallback != nil && tokens != *fallback {
			ok = false
			if lower := tokens.Max(*fallback); lower.Valid() {
				tokens = lower
			}
		}
		var added bool
		total, added = total.AddChecked(tokens)
		complete = complete && ok && added
	}
	if ctx.Err() != nil && !rootCounted && fallback != nil {
		total.Add(*fallback)
	}
	return total, complete && ctx.Err() == nil, childError
}

// ReadDir with a positive count keeps a single large directory from being
// loaded into memory before the entry limit can stop the scan.
func walkUsageFiles(ctx context.Context, root string, visit func(string, fs.DirEntry) error) error {
	pending := []string{root}
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := len(pending) - 1
		dir := pending[last]
		pending = pending[:last]
		children, err := readUsageDir(ctx, dir, visit)
		if err != nil {
			return err
		}
		pending = append(pending, children...)
	}
	return nil
}

func readUsageDir(ctx context.Context, path string, visit func(string, fs.DirEntry) error) (children []string, err error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	for {
		entries, err := dir.ReadDir(256)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := filepath.Join(path, entry.Name())
			if err := visit(name, entry); err != nil {
				return nil, err
			}
			if entry.IsDir() {
				children = append(children, name)
			}
		}
		if err == io.EOF {
			return children, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func rolloutScanner(ctx context.Context, r io.Reader, complete *bool) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes+2)
	dropping := false
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		newline := bytes.IndexByte(data, '\n')
		if dropping || newline > maxEventBytes || (newline < 0 && len(data) > maxEventBytes) {
			if complete != nil {
				*complete = false
			}
			if newline >= 0 {
				dropping = false
				return newline + 1, nil, nil
			}
			dropping = true
			return len(data), nil, nil
		}
		if complete != nil && atEOF && len(data) != 0 && data[len(data)-1] != '\n' {
			*complete = false
		}
		return bufio.ScanLines(data, atEOF)
	})
	return scanner
}

func rolloutTokens(raw json.RawMessage) (usage.Tokens, bool) {
	var fields eventUsage
	if json.Unmarshal(raw, &fields) != nil {
		return usage.Tokens{}, false
	}
	tokens := fields.tokens()
	if tokens == nil {
		return usage.Tokens{}, false
	}
	return *tokens, true
}

func readRolloutUsage(ctx context.Context, meta rolloutMeta, rootID string, budget *int64) (usage.Tokens, bool, []string, error) {
	if ctx.Err() != nil || *budget <= 0 {
		return usage.Tokens{}, false, nil, nil
	}
	f, err := os.Open(meta.path)
	if err != nil {
		return usage.Tokens{}, false, nil, nil
	}
	limit := int64(maxUsageRolloutBytes)
	if *budget < limit {
		limit = *budget
	}
	reader := &io.LimitedReader{R: f, N: limit + 1}
	complete, readable := true, true
	scanner := rolloutScanner(ctx, reader, &readable)
	var total, latest, inherited usage.Tokens
	seenRecord, seenSnapshot := false, false
	responses := map[string]usage.Tokens{}
	turns := map[string]bool{}
	ownedTurns := map[string]bool{}
	latestOwnTurn := ""
	latestOwnDone, latestOwnAborted := false, false
	var children []string
	seenChildren := map[string]bool{}
	tooManyChildren := false
	appendChildren := func(ids ...string) {
		if !appendChildIDs(&children, seenChildren, ids...) {
			tooManyChildren, complete = true, false
		}
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			complete = false
			break
		}
		var event rolloutEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			complete, readable = false, false
			continue
		}
		switch event.Type {
		case "token_usage_record":
			var record struct {
				ThreadID   string          `json:"thread_id"`
				SessionID  string          `json:"session_id"`
				TurnID     string          `json:"turn_id"`
				ResponseID string          `json:"response_id"`
				Usage      json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(event.Payload, &record) != nil {
				complete = false
				continue
			}
			// Forks can retain parent history and cumulative counters. Per-response
			// records carry their actual owner, so inherited work is counted once.
			if record.ThreadID != meta.ID {
				continue
			}
			if meta.ID != rootID && (meta.HistoryStart == nil || event.Ordinal <= *meta.HistoryStart) {
				complete = false
			}
			tokens, valid := rolloutTokens(record.Usage)
			if record.SessionID != rootID || record.ResponseID == "" || record.TurnID == "" || !valid {
				complete = false
				continue
			}
			ownedTurns[record.TurnID] = true
			if old, exists := responses[record.ResponseID]; exists {
				complete = complete && old == tokens
				continue
			}
			responses[record.ResponseID] = tokens
			total, valid = total.AddChecked(tokens)
			complete = complete && valid
			seenRecord = true
		case "event_msg":
			var payload struct {
				Type        string `json:"type"`
				TurnID      string `json:"turn_id"`
				ThreadID    string `json:"thread_id"`
				NewThreadID string `json:"new_thread_id"`
				Item        *struct {
					Type       string   `json:"type"`
					AgentID    string   `json:"agent_thread_id"`
					Tool       string   `json:"tool"`
					Status     string   `json:"status"`
					ReceiverID []string `json:"receiver_thread_ids"`
				} `json:"item"`
				Info *struct {
					Total json.RawMessage `json:"total_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				complete, readable = false, false
				continue
			}
			own := meta.ID == rootID || (meta.HistoryStart != nil && event.Ordinal > *meta.HistoryStart)
			if own && (payload.ThreadID == "" || payload.ThreadID == meta.ID) {
				if payload.Type == "collab_agent_spawn_end" && payload.NewThreadID != "" {
					appendChildren(payload.NewThreadID)
				}
				if payload.Type == "item_completed" && payload.Item != nil {
					if payload.Item.Type == "SubAgentActivity" {
						if payload.Item.AgentID == "" {
							complete = false
						} else {
							appendChildren(payload.Item.AgentID)
						}
					}
					if payload.Item.Type == "CollabAgentToolCall" && payload.Item.Tool == "spawn_agent" && payload.Item.Status == "completed" {
						appendChildren(payload.Item.ReceiverID...)
					}
				}
			}
			if payload.Type == "token_count" && payload.Info != nil {
				tokens, valid := rolloutTokens(payload.Info.Total)
				if !valid {
					complete = false
					continue
				}
				if own {
					latest, seenSnapshot = tokens, true
				} else {
					inherited = tokens
				}
			}
			if payload.TurnID != "" && (payload.ThreadID == "" || payload.ThreadID == meta.ID) && (own || ownedTurns[payload.TurnID]) {
				switch payload.Type {
				case "task_started", "task_complete", "turn_aborted":
					done := payload.Type == "task_complete"
					turns[payload.TurnID] = done
					if own {
						if payload.Type == "task_started" || latestOwnTurn == "" {
							latestOwnTurn = payload.TurnID
						}
						if payload.TurnID == latestOwnTurn {
							latestOwnDone = done
							latestOwnAborted = payload.Type == "turn_aborted"
						}
					}
					if payload.Type == "turn_aborted" {
						complete = false
					}
				}
			}
		}
	}
	consumed := limit + 1 - reader.N
	if consumed > limit {
		complete, readable = false, false
		consumed = limit
	}
	*budget -= consumed
	if err := f.Close(); err != nil {
		complete = false
	}
	if scanner.Err() != nil {
		readable = false
	}
	complete = complete && readable
	// The latest attributable abort, or an open latest turn in a fully read log,
	// proves unfinished child work. Accounting gaps alone do not.
	var childError error
	if meta.ID != rootID && (latestOwnAborted || readable && ctx.Err() == nil && latestOwnTurn != "" && !latestOwnDone) {
		childError = errors.New("helper agent work did not complete")
	}
	if tooManyChildren {
		childError = errors.Join(childError, fmt.Errorf("%w: Codex usage agent count exceeded the limit", aicli.ErrOutputLimit))
	}
	if !seenRecord {
		if meta.ID != rootID && (!complete || meta.HistoryStart == nil) {
			return usage.Tokens{}, false, children, childError
		}
		total = latest.Sub(inherited)
		if !seenSnapshot || !total.Valid() {
			return usage.Tokens{}, false, children, childError
		}
	} else if seenSnapshot {
		snapshot := latest.Sub(inherited)
		if snapshot != total {
			complete = false
			// Child counters may include an inherited baseline absent from a damaged
			// log. Only its individually attributed responses remain a lower bound.
			if meta.ID == rootID && snapshot.Valid() {
				if lower := total.Max(snapshot); lower.Valid() {
					total = lower
				}
			}
		}
	}
	if len(turns) == 0 {
		complete = false
	}
	for _, done := range turns {
		complete = complete && done
	}
	for turn := range ownedTurns {
		complete = complete && turns[turn]
	}
	// Older child rollouts without a history boundary cannot prove that a later
	// turn was not interrupted before its first usage record.
	return total, complete && (meta.ID == rootID || meta.HistoryStart != nil), children, childError
}
