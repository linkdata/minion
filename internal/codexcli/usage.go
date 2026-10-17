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

var errUnverifiedChildren = errors.New("helper agent work did not complete or completion could not be verified")

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
	complete := true
	files := map[string]rolloutMeta{}
	ambiguous := map[string]bool{}
	entries, candidates := 0, 0
	if rootID != "" && home != "" {
		err := walkUsageFiles(ctx, filepath.Join(home, "sessions"), started, func(path string, entry fs.DirEntry) error {
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
				ambiguous[meta.ID] = true
				return nil
			}
			meta.path = path
			files[meta.ID] = meta
			return nil
		})
		if err != nil {
			complete = false
		}
	}

	if rootID == "" || home == "" || ctx.Err() != nil {
		if fallback != nil {
			total = *fallback
		}
		if len(expectedChildren) != 0 {
			return total, false, errUnverifiedChildren
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
			childError = errors.Join(childError, errUnverifiedChildren)
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
	for i, id := range ids {
		if ctx.Err() != nil {
			complete = false
			if i == 0 && fallback != nil {
				total = *fallback
			}
			if len(ids) > 1 {
				childError = errors.Join(childError, errUnverifiedChildren)
			}
			break
		}
		meta, exists := files[id]
		if id != rootID && ambiguous[id] {
			childError = errors.Join(childError, errUnverifiedChildren)
		}
		if !exists {
			complete = false
			if id == rootID && fallback != nil {
				total = *fallback
			}
			continue
		}
		tokens, ok, children, workError := readRolloutUsage(ctx, meta, rootID, &budget)
		childError = errors.Join(childError, workError)
		for _, child := range children {
			if _, exists := files[child]; !exists || !selected[child] {
				ok = false
				childError = errors.Join(childError, errUnverifiedChildren)
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
	return total, complete && ctx.Err() == nil, childError
}

// ReadDir with a positive count keeps a single large directory from being
// loaded into memory before the entry limit can stop the scan.
func walkUsageFiles(ctx context.Context, root string, started time.Time, visit func(string, fs.DirEntry) error) error {
	// Codex creates local-date YYYY/MM/DD directories. Keep the preceding UTC
	// day too, covering timezone offsets and actions crossing local midnight.
	oldest := started.UTC().AddDate(0, 0, -1).Format("2006/01/02")
	recent := func(path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			rel, err := filepath.Rel(root, path)
			day := filepath.ToSlash(rel)
			if _, dateErr := time.Parse("2006/01/02", day); err == nil && dateErr == nil && day < oldest {
				return fs.SkipDir
			}
		}
		return visit(path, entry)
	}
	pending := []string{root}
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := len(pending) - 1
		dir := pending[last]
		pending = pending[:last]
		children, err := readUsageDir(ctx, dir, recent)
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
				if err == fs.SkipDir && entry.IsDir() {
					continue
				}
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

// Pull framing stops after a header and checks cancellation while dropping a
// long line; transcript reads also distinguish an incomplete final line.
func rolloutScanner(ctx context.Context, r io.Reader, complete *bool) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), aicli.MaxEventBytes+2)
	dropping := false
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		newline := bytes.IndexByte(data, '\n')
		if dropping || newline > aicli.MaxEventBytes || (newline < 0 && len(data) > aicli.MaxEventBytes) {
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
	return fields.tokens()
}

func readRolloutUsage(ctx context.Context, meta rolloutMeta, rootID string, budget *int64) (usage.Tokens, bool, []string, error) {
	var childError error
	if meta.ID != rootID {
		childError = errUnverifiedChildren
	}
	if ctx.Err() != nil || *budget <= 0 {
		return usage.Tokens{}, false, nil, childError
	}
	f, err := os.Open(meta.path)
	if err != nil {
		return usage.Tokens{}, false, nil, childError
	}
	limit := min(int64(maxUsageRolloutBytes), *budget)
	reader := &io.LimitedReader{R: f, N: limit + 1}
	complete, readable := true, true
	scanner := rolloutScanner(ctx, reader, &readable)
	var total, latest, inherited usage.Tokens
	seenRecord, seenSnapshot := false, false
	seenHeader, inheritedWithoutBoundary := false, false
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
		case "session_meta":
			if !seenHeader {
				seenHeader = true
				var header rolloutMeta
				if json.Unmarshal(event.Payload, &header) != nil || header.ID != meta.ID {
					complete, readable = false, false
				}
			} else if meta.ID != rootID && meta.HistoryStart == nil {
				// A fresh child has no copied header. Without a boundary, a
				// later session header prevents attributing inherited lifecycle.
				inheritedWithoutBoundary, complete = true, false
			}
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
			if meta.ID != rootID && meta.HistoryStart != nil && event.Ordinal < *meta.HistoryStart {
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
			own := meta.ID == rootID || (!inheritedWithoutBoundary && (meta.HistoryStart == nil || event.Ordinal >= *meta.HistoryStart))
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
	complete = complete && readable && (meta.ID == rootID || seenHeader)
	// Known helpers require a fully read, attributable completed latest turn.
	// Missing counters alone do not prevent that lifecycle proof.
	if seenHeader && readable && ctx.Err() == nil && !inheritedWithoutBoundary && latestOwnTurn != "" && latestOwnDone && !latestOwnAborted {
		childError = nil
	}
	if tooManyChildren {
		childError = errors.Join(childError, fmt.Errorf("%w: Codex usage agent count exceeded the limit", aicli.ErrOutputLimit))
	}
	if !seenRecord {
		if meta.ID != rootID && !complete {
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
		if _, observed := turns[turn]; !observed && meta.ID != rootID {
			childError = errors.Join(childError, errUnverifiedChildren)
			break
		}
	}
	return total, complete, children, childError
}
