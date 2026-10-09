package codexcli

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/usage"
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
func collectUsage(home, rootID string, started time.Time, fallback *usage.Tokens, expectedChildren []string) (usage.Tokens, bool) {
	var total usage.Tokens
	if rootID == "" || home == "" {
		if fallback != nil {
			total = *fallback
		}
		return total, false
	}
	complete := true
	files := map[string]rolloutMeta{}
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			complete = false
			return nil
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
		f, err := os.Open(path)
		if err != nil {
			complete = false
			return nil
		}
		scanner := rolloutScanner(f, nil)
		var event rolloutEvent
		var meta rolloutMeta
		ok := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "session_meta" && json.Unmarshal(event.Payload, &meta) == nil && meta.ID != ""
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
	selected := map[string]bool{rootID: true}
	for changed := true; changed; {
		changed = false
		for id, meta := range files {
			if !selected[id] && (meta.SessionID == rootID || selected[meta.ParentID]) {
				selected[id], changed = true, true
			}
		}
	}
	for _, id := range expectedChildren {
		if !selected[id] {
			complete = false
		}
	}
	for id := range selected {
		meta, exists := files[id]
		if !exists {
			complete = false
			if id == rootID && fallback != nil {
				total.Add(*fallback)
			}
			continue
		}
		tokens, ok, children := readRolloutUsage(meta, rootID)
		for _, child := range children {
			if _, exists := files[child]; !exists || !selected[child] {
				ok = false
			}
		}
		if id == rootID && fallback != nil && tokens != *fallback {
			ok = false
			if fallback.Total() > tokens.Total() {
				tokens = *fallback
			}
		}
		total.Add(tokens)
		complete = complete && ok
	}
	return total, complete
}

func rolloutScanner(f *os.File, complete *bool) *bufio.Scanner {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if complete != nil && atEOF && len(data) != 0 && data[len(data)-1] != '\n' {
			*complete = false
		}
		return bufio.ScanLines(data, atEOF)
	})
	return scanner
}

func validTokens(tokens usage.Tokens) bool {
	return tokens.Input >= 0 && tokens.CachedInput >= 0 && tokens.CachedInput <= tokens.Input && tokens.Output >= 0
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

func readRolloutUsage(meta rolloutMeta, rootID string) (usage.Tokens, bool, []string) {
	f, err := os.Open(meta.path)
	if err != nil {
		return usage.Tokens{}, false, nil
	}
	complete := true
	scanner := rolloutScanner(f, &complete)
	var total, latest, inherited usage.Tokens
	seenRecord, seenSnapshot := false, false
	responses := map[string]usage.Tokens{}
	turns := map[string]bool{}
	ownedTurns := map[string]bool{}
	var children []string
	for scanner.Scan() {
		var event rolloutEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			complete = false
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
			total.Add(tokens)
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
				complete = false
				continue
			}
			own := meta.ID == rootID || (meta.HistoryStart != nil && event.Ordinal > *meta.HistoryStart)
			if own && (payload.ThreadID == "" || payload.ThreadID == meta.ID) {
				if payload.Type == "collab_agent_spawn_end" && payload.NewThreadID != "" {
					children = append(children, payload.NewThreadID)
				}
				if payload.Type == "item_completed" && payload.Item != nil {
					if payload.Item.Type == "SubAgentActivity" {
						if payload.Item.AgentID == "" {
							complete = false
						} else {
							children = append(children, payload.Item.AgentID)
						}
					}
					if payload.Item.Type == "CollabAgentToolCall" && payload.Item.Tool == "spawn_agent" && payload.Item.Status == "completed" {
						children = append(children, payload.Item.ReceiverID...)
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
			if payload.TurnID != "" && (own || ownedTurns[payload.TurnID]) {
				switch payload.Type {
				case "task_started":
					turns[payload.TurnID] = false
				case "task_complete":
					turns[payload.TurnID] = true
				case "turn_aborted":
					turns[payload.TurnID] = false
					complete = false
				}
			}
		}
	}
	if err := f.Close(); err != nil {
		complete = false
	}
	if scanner.Err() != nil {
		complete = false
	}
	if !seenRecord {
		if meta.ID != rootID && (!complete || meta.HistoryStart == nil) {
			return usage.Tokens{}, false, children
		}
		total = usage.Tokens{Input: latest.Input - inherited.Input, CachedInput: latest.CachedInput - inherited.CachedInput, Output: latest.Output - inherited.Output}
		if !seenSnapshot || !validTokens(total) {
			return usage.Tokens{}, false, children
		}
	} else if seenSnapshot {
		snapshot := usage.Tokens{Input: latest.Input - inherited.Input, CachedInput: latest.CachedInput - inherited.CachedInput, Output: latest.Output - inherited.Output}
		if snapshot != total {
			complete = false
			// Child counters may include an inherited baseline absent from a damaged
			// log. Only its individually attributed responses remain a lower bound.
			if meta.ID == rootID && validTokens(snapshot) && snapshot.Total() > total.Total() {
				total = snapshot
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
	return total, complete && (meta.ID == rootID || meta.HistoryStart != nil), children
}
