package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/cleanup"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

const privateRecordRetention = 30 * 24 * time.Hour

// PrunePrivateRecords removes only records bound to a finished attempt past retention.
func (e *Orchestrator) PrunePrivateRecords(ctx context.Context, store *state.Store, now time.Time) error {
	eligible := make(map[string]bool)
	if err := store.WithLock(func() error {
		protected := make(map[string]bool)
		if accepted := store.Data.Accepted; accepted != nil {
			protected[accepted.AttemptID] = true
		}
		for _, write := range store.Data.PublicationWrites {
			if write.Status == state.PublicationWritePending {
				protected[write.AttemptID] = true
			}
		}
		for _, decision := range store.Data.TerminalDecisions {
			if !workspace.ValidIdentitySegment(decision.AttemptID) {
				return errors.New("invalid terminal attempt ID for private retention")
			}
			if !protected[decision.AttemptID] && !now.Before(decision.DecidedAt.Add(privateRecordRetention)) {
				eligible[decision.AttemptID] = true
			}
		}
		return nil
	}); err != nil || len(eligible) == 0 {
		return err
	}
	root, err := workspace.CanonicalPath(e.WorkRoot)
	if err != nil {
		return err
	}
	if err := workspace.CheckRoot(root); err != nil {
		return err
	}
	var removed int
	var pruneErr error
	for _, item := range []struct{ dir, suffix string }{
		{"attempt-records", ".jsonl"}, {"reviews", ".json"}, {"worker-diagnostics", ".json"},
	} {
		count, err := prunePrivateFiles(ctx, root, item.dir, item.suffix, eligible)
		removed += count
		pruneErr = errors.Join(pruneErr, err)
	}
	count, err := prunePrivateAudits(ctx, root, eligible)
	removed += count
	pruneErr = errors.Join(pruneErr, err)
	if removed != 0 {
		e.logf("private artifacts expired count=%d", removed)
	}
	return pruneErr
}

func privateRetentionEntries(root, name string) (string, []os.DirEntry, error) {
	dir := filepath.Join(root, name)
	if err := workspace.CheckRoot(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dir, nil, nil
		}
		return dir, nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return dir, nil, nil
	}
	return dir, entries, err
}

func prunePrivateFiles(ctx context.Context, root, name, suffix string, eligible map[string]bool) (int, error) {
	dir, entries, err := privateRetentionEntries(root, name)
	if err != nil {
		return 0, fmt.Errorf("read %s retention entries: %w", name, err)
	}
	var removed int
	var removeErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(removeErr, err)
		}
		id, match := strings.CutSuffix(entry.Name(), suffix)
		if !match || !eligible[id] || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				removeErr = errors.Join(removeErr, err)
			}
		} else {
			removed++
		}
	}
	if removed != 0 {
		removeErr = errors.Join(removeErr, workspace.SyncDirectories(dir))
	}
	return removed, removeErr
}

func prunePrivateAudits(ctx context.Context, root string, eligible map[string]bool) (int, error) {
	dir, entries, err := privateRetentionEntries(root, "audits")
	if err != nil {
		return 0, fmt.Errorf("read audit retention entries: %w", err)
	}
	var removed int
	var removeErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(removeErr, err)
		}
		if !entry.IsDir() || !eligible[auditAttemptID(entry.Name())] {
			continue
		}
		if err := cleanup.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			removeErr = errors.Join(removeErr, err)
		} else {
			removed++
		}
	}
	if removed != 0 {
		removeErr = errors.Join(removeErr, workspace.SyncDirectories(dir))
	}
	return removed, removeErr
}

func auditAttemptID(name string) string {
	if id, ok := strings.CutPrefix(name, "complete-"); ok {
		return id
	}
	if id, ok := strings.CutPrefix(name, ".complete-"); ok {
		return id
	}
	if rest, ok := strings.CutPrefix(name, "incomplete-"); ok {
		id, suffix, found := strings.Cut(rest, "+")
		if found && suffix != "" {
			return id
		}
	}
	return ""
}
