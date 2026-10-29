package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

const publicationRecheckInterval = 30 * time.Second
const publicationReconcileBudget = 10 * time.Second
const publicationLastChanceInterval = 100 * time.Millisecond

// ReconcilePublicationWrites checks issued writes without accepting or running a new action.
func (m *Monitor) ReconcilePublicationWrites(ctx context.Context, store *state.Store) error {
	return m.reconcilePublicationWrites(ctx, store, "", "")
}

// Reconcile only remote effects of writes recorded before their external operation.
// The action's first terminal decision is independent of this observation.
func (m *Monitor) reconcilePublicationWrites(ctx context.Context, store *state.Store, onlyJobID, skipJobID string) error {
	pending := make(map[string]state.PublicationWrite)
	var cursor string
	var activePushID string
	var activeDeadline time.Time
	if err := store.WithLock(func() error {
		cursor = store.Data.PublicationScanCursor
		if accepted := store.Data.Accepted; accepted != nil {
			activePushID, activeDeadline = accepted.JobID+"-push", accepted.Deadline
		}
		for jobID, write := range store.Data.PublicationWrites {
			if write.Status == state.PublicationWritePending && jobID != skipJobID && jobID != skipJobID+"-push" &&
				(onlyJobID == "" || jobID == onlyJobID || jobID == onlyJobID+"-push") {
				if write.Kind == "push" {
					pending, exists := store.Data.PendingFixes[strings.TrimSuffix(jobID, "-push")]
					if !exists || pending.SHA != write.SHA {
						return fmt.Errorf("%w: issued fix push differs from its commit checkpoint", ErrAcceptanceState)
					}
				}
				pending[jobID] = write
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(pending) > 0 && m.GitHub == nil {
		return fmt.Errorf("%w: GitHub client is missing for issued publication reconciliation", ErrAcceptanceState)
	}
	ids := make([]string, 0, len(pending))
	for jobID := range pending {
		ids = append(ids, jobID)
	}
	sort.Strings(ids)
	start := 0
	passCtx := ctx
	var passDeadline time.Time
	if onlyJobID == "" && len(ids) > 0 {
		start = sort.Search(len(ids), func(i int) bool { return ids[i] > cursor }) % len(ids)
		var cancel context.CancelFunc
		passDeadline = time.Now().Add(publicationReconcileBudget)
		passCtx, cancel = context.WithDeadline(ctx, passDeadline)
		defer cancel()
	}
	lastChecked := ""
	for offset := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if passCtx.Err() != nil || !passDeadline.IsZero() && !time.Now().Before(passDeadline) {
			break
		}
		jobID := ids[(start+offset)%len(ids)]
		write := pending[jobID]
		now := m.now().UTC()
		expired := !now.Before(write.Deadline)
		lookupTimeout := 10 * time.Second
		if !expired {
			lookupTimeout = min(lookupTimeout, write.Deadline.Sub(now))
		}
		if jobID == activePushID && write.Kind == "push" {
			if remaining := activeDeadline.Sub(now); remaining > 0 {
				// Leave time for another check and the saved fix reply.
				lookupTimeout = min(lookupTimeout, remaining/2)
			}
		}
		lookupCtx, cancel := context.WithTimeout(passCtx, lookupTimeout)
		var found bool
		var err error
		if write.Kind == "push" {
			branchClient, ok := m.GitHub.(interface {
				Branch(context.Context, string, string) (github.Branch, error)
			})
			if !ok {
				cancel()
				return fmt.Errorf("%w: GitHub client cannot inspect an issued fix push", ErrAcceptanceState)
			}
			var branch github.Branch
			branch, err = branchClient.Branch(lookupCtx, write.Repository, write.Branch)
			if err == nil {
				found = branch.Name == write.Branch && branch.Commit.SHA == write.SHA
			} else {
				var status *github.StatusError
				if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
					err = nil
				}
			}
		} else {
			found, err = m.GitHub.HasVerifiedBotPublication(lookupCtx, write.Repository, write.Number, write.Since,
				github.User{ID: write.BotID, Login: write.BotLogin}, write.Marker, write.Kind)
		}
		cancel()
		lastChecked = jobID
		target := fmt.Sprintf("%s #%d", write.Repository, write.Number)
		if write.Kind == "push" {
			target = fmt.Sprintf("%s branch %s at %s", write.Repository, write.Branch, write.SHA)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.logf("check issued publication %s in %s: %v", jobID, target, err)
			found = false
		}
		if found {
			changed, err := storePublicationWriteOutcome(store, jobID, write, state.PublicationWriteObserved, m.now().UTC())
			if err != nil {
				return fmt.Errorf("%w: record observed publication %s: %w", ErrAcceptanceState, jobID, err)
			}
			if changed {
				m.logf("observed issued publication %s in %s on GitHub", jobID, target)
			}
			continue
		}
		if expired {
			changed, err := storePublicationWriteOutcome(store, jobID, write, state.PublicationWriteUncertain, m.now().UTC())
			if err != nil {
				return fmt.Errorf("%w: close issued publication %s: %w", ErrAcceptanceState, jobID, err)
			}
			if changed {
				m.logf("publication write %s in %s reached its fixed reconciliation deadline; remote effect is uncertain", jobID, target)
			}
		}
	}
	if onlyJobID == "" && lastChecked != "" {
		if err := store.WithLock(func() error {
			if store.Data.PublicationScanCursor == lastChecked {
				return nil
			}
			store.Data.PublicationScanCursor = lastChecked
			return store.Save()
		}); err != nil {
			return fmt.Errorf("%w: save publication scan cursor: %w", ErrAcceptanceState, err)
		}
	}
	return nil
}

func storePublicationWriteOutcome(store *state.Store, jobID string, original state.PublicationWrite, status string, at time.Time) (bool, error) {
	changed := false
	err := store.WithLock(func() error {
		current, exists := store.Data.PublicationWrites[jobID]
		if !exists || current != original || current.Status != state.PublicationWritePending {
			return nil
		}
		current.Status = status
		current.ResolvedAt = at
		store.Data.PublicationWrites[jobID] = current
		if status == state.PublicationWriteObserved && current.Kind == "push" {
			if accepted := store.Data.Accepted; accepted != nil && accepted.JobID+"-push" == jobID &&
				accepted.AttemptID == current.AttemptID && accepted.ResultReady {
				accepted.RetryAt = time.Time{}
			}
		}
		if err := store.Save(); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func publicationWriteWake(store *state.Store, at time.Time) time.Time {
	return publicationWriteWakeExcept(store, at, "")
}

func publicationWriteWakeExcept(store *state.Store, at time.Time, skipJobID string) time.Time {
	var next time.Time
	_ = store.WithLock(func() error {
		for jobID, write := range store.Data.PublicationWrites {
			if jobID == skipJobID || write.Status != state.PublicationWritePending {
				continue
			}
			var actionDeadline time.Time
			if accepted := store.Data.Accepted; accepted != nil && write.Kind == "push" &&
				jobID == accepted.JobID+"-push" && at.Before(accepted.Deadline) {
				actionDeadline = accepted.Deadline
			}
			checkAt := publicationWriteCheckAt(write, at, actionDeadline)
			if next.IsZero() || checkAt.Before(next) {
				next = checkAt
			}
		}
		return nil
	})
	return next
}

func publicationWriteCheckAt(write state.PublicationWrite, at, actionDeadline time.Time) time.Time {
	interval := publicationRecheckInterval
	if !actionDeadline.IsZero() && at.Before(actionDeadline) {
		remaining := actionDeadline.Sub(at)
		// Leave time for another lookup without a rapid loop at expiry.
		interval = min(interval, remaining, max(publicationLastChanceInterval, remaining/2))
	}
	checkAt := at.Add(interval)
	if write.Deadline.Before(checkAt) {
		checkAt = write.Deadline
	}
	return checkAt
}

func withPublicationWriteWake(next time.Time, store *state.Store, at time.Time) time.Time {
	pending := publicationWriteWake(store, at)
	if !pending.IsZero() && (next.IsZero() || pending.Before(next)) {
		return pending
	}
	return next
}
