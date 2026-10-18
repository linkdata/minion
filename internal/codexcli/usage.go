package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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

// collectUsage reads bounded first-line headers of recent rollouts to identify
// this action's agent tree. Unrelated transcripts are never read past their header.
func collectUsage(ctx context.Context, home, rootID string, started time.Time, fallback *usage.Tokens, expectedChildren []string) (usage.Tokens, bool, error) {
	files, ambiguous, complete := usageFiles(ctx, home, rootID, started, nil)
	return collectUsageFiles(ctx, rootID, fallback, expectedChildren, files, ambiguous, complete)
}

func collectUsageFiles(ctx context.Context, rootID string, fallback *usage.Tokens, expectedChildren []string, files map[string]rolloutMeta, ambiguous map[string]bool, complete bool) (usage.Tokens, bool, error) {
	var total usage.Tokens
	if rootID == "" || ctx.Err() != nil {
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

// usageFiles reads only bounded headers. Known paths bypass discovery timestamps,
// so a resumed conversation remains visible after its original directory ages.
func usageFiles(ctx context.Context, home, rootID string, started time.Time, known map[string]rolloutMeta) (map[string]rolloutMeta, map[string]bool, bool) {
	files := map[string]rolloutMeta{}
	ambiguous := map[string]bool{}
	complete := true
	entries, candidates := 0, 0
	visited := map[string]bool{}
	readHeader := func(path string) {
		if visited[path] {
			return
		}
		visited[path] = true
		candidates++
		if candidates > maxUsageFiles {
			complete = false
			return
		}
		f, err := os.Open(path)
		if err != nil {
			complete = false
			return
		}
		scanner := bufio.NewScanner(io.LimitReader(f, maxUsageHeaderBytes+1))
		scanner.Buffer(make([]byte, 4096), maxUsageHeaderBytes+2)
		var event rolloutEvent
		var meta rolloutMeta
		ok := scanner.Scan() && len(scanner.Bytes()) <= maxUsageHeaderBytes && json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "session_meta" && json.Unmarshal(event.Payload, &meta) == nil && meta.ID != "" && scanner.Err() == nil
		if err := f.Close(); err != nil {
			complete = false
		}
		if !ok {
			complete = false
			return
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
			return
		}
		meta.path = path
		files[meta.ID] = meta
	}
	for _, meta := range known {
		if ctx.Err() != nil {
			complete = false
			break
		}
		readHeader(meta.path)
	}
	if rootID != "" {
		err := walkUsageFiles(ctx, filepath.Join(home, "sessions"), started, func(path string, entry fs.DirEntry) error {
			entries++
			if entries > maxUsageWalkEntries || candidates > maxUsageFiles {
				complete = false
				return fs.SkipAll
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			if visited[path] {
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
			readHeader(path)
			return nil
		})
		if err != nil {
			complete = false
		}
	}
	return files, ambiguous, complete && ctx.Err() == nil
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
	var skipped error
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return errors.Join(skipped, err)
		}
		last := len(pending) - 1
		dir := pending[last]
		pending = pending[:last]
		children, err := readUsageDir(ctx, dir, recent)
		if err != nil {
			if errors.Is(err, fs.SkipAll) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return errors.Join(skipped, err)
			}
			skipped = errors.Join(skipped, err)
		}
		pending = append(pending, children...)
	}
	return skipped
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
				return children, err
			}
			name := filepath.Join(path, entry.Name())
			if err := visit(name, entry); err != nil {
				if err == fs.SkipDir && entry.IsDir() {
					continue
				}
				return children, err
			}
			if entry.IsDir() {
				children = append(children, name)
			}
		}
		if err == io.EOF {
			return children, nil
		}
		if err != nil {
			return children, err
		}
	}
}

// Pull transcript framing checks cancellation while dropping long lines and
// distinguishes an incomplete final line.
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
			*complete = false
			if newline >= 0 {
				dropping = false
				return newline + 1, nil, nil
			}
			dropping = true
			return len(data), nil, nil
		}
		if atEOF && len(data) != 0 && data[len(data)-1] != '\n' {
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
	return readRolloutUsageFrom(ctx, meta, rootID, budget, nil, nil, nil)
}

// A boundary excludes previous turns even when their final records arrive late.
func readRolloutUsageFrom(ctx context.Context, meta rolloutMeta, rootID string, budget *int64, before, capture *usageBoundary, spawned map[string]bool) (usage.Tokens, bool, []string, error) {
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
	hash := sha256.New()
	if before != nil {
		if !before.valid || before.size > limit || !matchUsagePrefix(ctx, reader, before, hash) {
			*budget -= min(limit, limit+1-reader.N)
			_ = f.Close()
			return usage.Tokens{}, false, nil, childError
		}
	}
	var input io.Reader = reader
	if capture != nil {
		input = io.TeeReader(reader, hash)
	}
	complete, readable := true, true
	scanner := rolloutScanner(ctx, input, &readable)
	var total, latest, inherited usage.Tokens
	seenRecord, seenSnapshot := false, false
	seenHeader, inheritedWithoutBoundary := before != nil, false
	if before != nil {
		inherited = before.snapshot
	}
	lateOld := usage.Tokens{}
	snapshotKnown := before == nil || (before.hasSnapshot && !before.unfinished)
	responses := map[string]usage.Tokens{}
	responseTurns := map[string]string{}
	turns := map[string]bool{}
	abortedTurns := map[string]bool{}
	ownedTurns := map[string]bool{}
	latestOwnTurn := ""
	latestOwnDone := false
	var children []string
	seenChildren := map[string]bool{}
	tooManyChildren := false
	appendChildren := func(ids ...string) {
		if !appendChildIDs(&children, seenChildren, ids...) {
			tooManyChildren, complete = true, false
		}
	}
	appendSpawned := func(ids ...string) {
		appendChildren(ids...)
		if spawned == nil {
			return
		}
		for _, id := range ids {
			if !seenChildren[id] {
				continue
			}
			if !spawned[id] && len(spawned) >= maxCodexChildren {
				tooManyChildren, complete = true, false
				continue
			}
			spawned[id] = true
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
				if capture != nil {
					capture.valid = false
				}
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
			if capture != nil && record.TurnID != "" {
				capture.rememberTurn(record.TurnID)
			}
			if record.SessionID != rootID || record.ResponseID == "" || record.TurnID == "" || !valid {
				complete = false
				if capture != nil {
					capture.valid = false
				}
				if before != nil {
					snapshotKnown = false
				}
				continue
			}
			if before != nil {
				if old, exists := before.responses[record.ResponseID]; exists {
					if old != tokens || !before.turns[record.TurnID] {
						complete, snapshotKnown = false, false
					}
					continue
				}
				if before.turns[record.TurnID] {
					if old, exists := responses[record.ResponseID]; exists {
						if old != tokens {
							complete, snapshotKnown = false, false
						}
						continue
					}
					responses[record.ResponseID] = tokens
					responseTurns[record.ResponseID] = record.TurnID
					lateOld, valid = lateOld.AddChecked(tokens)
					snapshotKnown = snapshotKnown && valid
					continue
				}
			}
			ownedTurns[record.TurnID] = true
			if old, exists := responses[record.ResponseID]; exists {
				complete = complete && old == tokens
				if before != nil && responseTurns[record.ResponseID] != record.TurnID {
					complete, snapshotKnown = false, false
				}
				continue
			}
			responses[record.ResponseID] = tokens
			if before != nil {
				responseTurns[record.ResponseID] = record.TurnID
			}
			if capture != nil && len(responses) > maxUsageWalkEntries {
				capture.valid = false
			}
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
			if own && (payload.ThreadID == "" || payload.ThreadID == meta.ID) && payload.TurnID != "" {
				if capture != nil {
					capture.rememberTurn(payload.TurnID)
				}
				if before != nil && before.turns[payload.TurnID] {
					continue
				}
			}
			if own && (payload.ThreadID == "" || payload.ThreadID == meta.ID) {
				if payload.Type == "collab_agent_spawn_end" && payload.NewThreadID != "" {
					appendSpawned(payload.NewThreadID)
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
						appendSpawned(payload.Item.ReceiverID...)
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
						}
					}
					if payload.Type == "turn_aborted" {
						if capture != nil {
							abortedTurns[payload.TurnID] = true
						}
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
	if capture != nil {
		capture.size = consumed
		if len(responses) <= maxUsageWalkEntries {
			capture.responses = responses
		} else {
			capture.valid = false
		}
		copy(capture.hash[:], hash.Sum(nil))
		capture.snapshot, capture.hasSnapshot = latest, seenSnapshot
		for turn, done := range turns {
			capture.unfinished = capture.unfinished || (!done && !abortedTurns[turn])
		}
		for turn := range ownedTurns {
			capture.unfinished = capture.unfinished || (!turns[turn] && !abortedTurns[turn])
		}
		capture.valid = capture.valid && readable && seenHeader && ctx.Err() == nil
	}
	if before != nil {
		var added bool
		inherited, added = inherited.AddChecked(lateOld)
		snapshotKnown = snapshotKnown && added
		if !snapshotKnown {
			if seenSnapshot {
				complete = false
			}
			seenSnapshot = false
		}
	}
	complete = complete && readable && (meta.ID == rootID || seenHeader)
	// Known helpers require a fully read, attributable completed latest turn.
	// Missing counters alone do not prevent that lifecycle proof.
	if seenHeader && readable && ctx.Err() == nil && !inheritedWithoutBoundary && latestOwnTurn != "" && latestOwnDone {
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
			if before == nil && meta.ID == rootID && snapshot.Valid() {
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
