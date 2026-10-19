package codexcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

// usageBoundary records an exact prefix, rather than relying on timestamps or
// event ordinals. Old turns remain old when their last records arrive late.
type usageBoundary struct {
	valid, prefixKnown, hasSnapshot, unfinished, completed bool
	size                                                   int64
	hash                                                   [32]byte
	turns                                                  map[string]bool
	responses                                              map[string]usage.Tokens
	snapshot                                               usage.Tokens
}

func matchUsagePrefix(ctx context.Context, r io.Reader, before *usageBoundary, h hash.Hash) bool {
	var buffer [32 << 10]byte
	remaining := before.size
	for remaining > 0 {
		if ctx.Err() != nil {
			return false
		}
		n, err := io.ReadFull(r, buffer[:min(int64(len(buffer)), remaining)])
		_, _ = h.Write(buffer[:n])
		remaining -= int64(n)
		if err != nil {
			return false
		}
	}
	return bytes.Equal(h.Sum(nil), before.hash[:])
}

// Unreferenced historical helpers need no new turn, but their bytes must still
// match the baseline before their absence from current accounting is certain.
func unchangedUsageRollout(ctx context.Context, name string, before *usageBoundary, budget *int64) bool {
	if before == nil || !before.prefixKnown || before.size > *budget || ctx.Err() != nil {
		return false
	}
	f, err := os.Open(name)
	if err != nil {
		return false
	}
	reader := &io.LimitedReader{R: f, N: before.size + 1}
	unchanged := matchUsagePrefix(ctx, reader, before, sha256.New())
	if unchanged {
		var extra [1]byte
		n, err := reader.Read(extra[:])
		unchanged = n == 0 && errors.Is(err, io.EOF)
	}
	*budget -= min(before.size, before.size+1-reader.N)
	closeErr := f.Close()
	return unchanged && closeErr == nil && ctx.Err() == nil
}

type usageSession struct {
	// Metadata/baselines use the header-file cap, not the current-call agent
	// cap: historical helpers must not consume current helper slots.
	known            map[string]rolloutMeta
	before           map[string]*usageBoundary
	budget           int64
	baselineComplete bool
}

func ownedUsageFiles(files map[string]rolloutMeta, rootID string) map[string]rolloutMeta {
	owned := map[string]rolloutMeta{}
	if meta, ok := files[rootID]; ok {
		owned[rootID] = meta
	}
	for changed := true; changed; {
		changed = false
		for id, meta := range files {
			if _, exists := owned[id]; exists {
				continue
			}
			_, parent := owned[meta.ParentID]
			if meta.SessionID == rootID || meta.ParentID == rootID || parent {
				owned[id], changed = meta, true
			}
		}
	}
	return owned
}

// begin snapshots only owned histories before a paid invocation. Missing or
// damaged evidence affects accounting/completion, rather than inventing a zero
// lifetime baseline or rejecting a call before its provider runs.
func (s *usageSession) begin(ctx context.Context, home, rootID string, started time.Time) {
	s.before = nil
	s.budget = maxUsageScanBytes
	s.baselineComplete = true
	if rootID == "" {
		s.known = nil
		return
	}
	files, ambiguous, complete := usageFiles(ctx, home, rootID, started, s.known)
	s.baselineComplete = complete
	s.known = ownedUsageFiles(files, rootID)
	s.before = map[string]*usageBoundary{}
	ids := []string{rootID}
	for _, id := range slices.Sorted(maps.Keys(s.known)) {
		if id != rootID {
			ids = append(ids, id)
		}
	}
	for i, id := range ids {
		if i >= maxUsageFiles {
			s.baselineComplete = false
			break
		}
		b := &usageBoundary{valid: true, turns: map[string]bool{}}
		s.before[id] = b
		meta, exists := s.known[id]
		if !exists || ambiguous[id] {
			b.valid = false
			continue
		}
		_, _, _, _ = readRolloutUsageFrom(ctx, meta, rootID, &s.budget, nil, b, nil, false)
	}
}

func (s *usageSession) collectUsage(ctx context.Context, home, rootID string, started time.Time, fallback *usage.Tokens, expectedChildren []string, observationOnly map[string]bool) (usage.Tokens, bool, error) {
	if s.before == nil {
		files, ambiguous, complete := usageFiles(ctx, home, rootID, started, nil)
		tokens, complete, err := collectUsageFiles(ctx, rootID, fallback, expectedChildren, files, ambiguous, complete)
		s.known = ownedUsageFiles(files, rootID)
		return tokens, complete, err
	}
	s.budget = maxUsageScanBytes // Collection has its own bound after the baseline pass.
	files, ambiguous, complete := usageFiles(ctx, home, rootID, started, s.known)
	complete = complete && s.baselineComplete
	owned := ownedUsageFiles(files, rootID)
	var total usage.Tokens
	var childError error
	visited, spawned := map[string]bool{}, map[string]bool{}
	queue := []string{rootID}
	// Current stream references are checked even when the rollout omitted the
	// matching activity event. They never select unrelated historical helpers.
	expected := append([]string(nil), expectedChildren...)
	for len(queue) != 0 || len(expected) != 0 {
		if len(queue) == 0 {
			queue, expected = expected, nil
		}
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		if len(visited) >= maxUsageAgents {
			complete = false
			childError = joinUsageErrors(childError, errUsageAgentLimit)
			break
		}
		visited[id] = true
		meta, exists := owned[id]
		boundary, prior := s.before[id]
		var tokens usage.Tokens
		var currentRoot *usageBoundary
		if id == rootID && fallback != nil {
			currentRoot = &usageBoundary{valid: true, turns: map[string]bool{}}
		}
		ok := false
		if exists && !ambiguous[id] && (prior || spawned[id]) {
			var children []string
			var workError error
			tokens, ok, children, workError = readRolloutUsageFrom(ctx, meta, rootID, &s.budget, boundary, currentRoot, spawned, id != rootID && observationOnly[id])
			childError = joinUsageErrors(childError, workError)
			queue = append(queue, children...)
		} else if id != rootID {
			childError = joinUsageErrors(childError, errUnverifiedChildren)
		}
		if currentRoot != nil && currentRoot.hasSnapshot && currentRoot.snapshot != *fallback {
			// Resume stdout is cumulative for the root, not this invocation
			// or its helpers. Compare like counters without enlarging the delta.
			ok = false
		}
		var added bool
		total, added = total.AddChecked(tokens)
		complete = complete && ok && added
	}
	// Lifetime ownership alone cannot attribute unreferenced new or changed
	// helper work to this invocation. Preserve the known lower bound and refuse
	// completion rather than silently certifying that omitted evidence.
	for _, id := range slices.Sorted(maps.Keys(owned)) {
		meta := owned[id]
		if !visited[id] && (ambiguous[id] || !unchangedUsageRollout(ctx, meta.path, s.before[id], &s.budget)) {
			complete = false
			childError = joinUsageErrors(childError, errUnverifiedChildren)
			break
		}
	}
	// Keep bounded lifetime ownership, including unresolved known identities, for the next invocation.
	s.known = owned
	return total, complete && ctx.Err() == nil, childError
}
