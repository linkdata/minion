package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

// ErrStopConflict means the selected attempt has changed or already ended.
var ErrStopConflict = errors.New("selected attempt is no longer stoppable")

type selectedStopCancel struct {
	jobID, attemptID string
	cancel           context.CancelFunc
}

type savedStopRequest struct {
	jobID, attemptID string
	request          StopRequest
}

// RequestStop saves the first request and fences new assignment/publication
// before canceling the live selected execution. A repeat preserves its author.
func (s *Store) RequestStop(jobID, attemptID, requestedBy string, at time.Time) (StopRequest, error) {
	return s.requestStop(jobID, attemptID, requestedBy, at, s.ReadSavedDurable)
}

func (s *Store) requestStop(jobID, attemptID, requestedBy string, at time.Time, readSavedDurable func() (Data, error)) (StopRequest, error) {
	if !workspace.ValidIdentitySegment(jobID) || !workspace.ValidIdentitySegment(attemptID) ||
		!validStopRequester(requestedBy) || at.IsZero() {
		return StopRequest{}, errors.New("invalid stop request")
	}
	var request StopRequest
	err := s.WithLock(func() error {
		cached := func(stop StopRequest) bool {
			return s.savedStop.jobID == jobID && s.savedStop.attemptID == attemptID && s.savedStop.request == stop
		}
		remember := func(stop StopRequest) { s.savedStop = savedStopRequest{jobID, attemptID, stop} }
		if active := s.activeStop; active == nil || active.jobID != jobID || active.attemptID != attemptID {
			// Deferred preparation and terminal cleanup have no live stop slot.
			// Confirm the saved request before acknowledging a repeat.
			if accepted := s.Data.Accepted; accepted != nil && accepted.JobID == jobID && accepted.AttemptID == attemptID &&
				accepted.StopRequest != (StopRequest{}) && cached(accepted.StopRequest) {
				request = accepted.StopRequest
				return nil
			}
			if decision := s.Data.TerminalDecisions[jobID]; decision.AttemptID == attemptID &&
				decision.StopRequest != (StopRequest{}) && cached(decision.StopRequest) {
				request = decision.StopRequest
				return nil
			}
			saved, err := readSavedDurable()
			if err != nil {
				return fmt.Errorf("read saved stop request: %w", err)
			}
			if accepted := saved.Accepted; accepted != nil && accepted.JobID == jobID && accepted.AttemptID == attemptID &&
				accepted.StopRequest != (StopRequest{}) {
				request = accepted.StopRequest
				remember(request)
				return nil
			}
			if decision := saved.TerminalDecisions[jobID]; decision.AttemptID == attemptID && decision.StopRequest != (StopRequest{}) {
				request = decision.StopRequest
				remember(request)
				return nil
			}
			return ErrStopConflict
		}
		accepted := s.Data.Accepted
		if accepted == nil || accepted.JobID != jobID || accepted.AttemptID != attemptID {
			if decision := s.Data.TerminalDecisions[jobID]; decision.AttemptID == attemptID && decision.StopRequest != (StopRequest{}) {
				request = decision.StopRequest // A completed request is safe to repeat.
				return nil
			}
			return ErrStopConflict
		}
		if _, decided := s.Data.TerminalDecisions[jobID]; accepted.StopRequest == (StopRequest{}) && (accepted.Terminal || decided) {
			return ErrStopConflict
		}
		if accepted.StopRequest != (StopRequest{}) {
			request = accepted.StopRequest
			if cached(request) {
				s.cancelSelectedLocked(jobID, attemptID)
				return nil
			}
			saved, err := readSavedDurable()
			if err != nil {
				return fmt.Errorf("read saved stop request: %w", err)
			}
			if current := saved.Accepted; current != nil && current.JobID == jobID && current.AttemptID == attemptID &&
				current.StopRequest == request {
				remember(request)
				s.cancelSelectedLocked(jobID, attemptID)
				return nil
			}
		}
		previous := *accepted
		previousLastStop := s.Data.LastStopJobID
		if accepted.StopRequest == (StopRequest{}) {
			accepted.StopRequest = StopRequest{RequestedBy: requestedBy, RequestedAt: at.UTC(), Status: "requested"}
			accepted.Terminal = true
			s.Data.LastStopJobID = jobID
		}
		request = accepted.StopRequest
		if err := s.Save(); err != nil {
			// A failed directory sync can leave the replacement visible but
			// unconfirmed. Stop its live worker even when that sync also fails.
			saved, readErr := readSavedDurable()
			if readErr != nil {
				saved, readErr = s.ReadSaved()
			}
			visible := readErr == nil && saved.Accepted != nil && saved.Accepted.JobID == jobID &&
				saved.Accepted.AttemptID == attemptID && saved.Accepted.Terminal && saved.Accepted.StopRequest == request
			if visible || readErr != nil {
				s.cancelSelectedLocked(jobID, attemptID)
			} else {
				*accepted = previous
				s.Data.LastStopJobID = previousLastStop
			}
			return fmt.Errorf("save stop request: %w", err)
		}
		remember(request)
		s.cancelSelectedLocked(jobID, attemptID)
		return nil
	})
	return request, err
}

// SelectedStopActive reports whether a new request can target this live run.
func (s *Store) SelectedStopActive(jobID, attemptID string) bool {
	active := false
	_ = s.WithLock(func() error {
		active = s.activeStop != nil && s.activeStop.jobID == jobID && s.activeStop.attemptID == attemptID
		return nil
	})
	return active
}

func (s *Store) cancelSelectedLocked(jobID, attemptID string) {
	if active := s.activeStop; active != nil && active.jobID == jobID && active.attemptID == attemptID {
		active.cancel()
	}
}

// RegisterSelectedStop binds one run to the durable request. Registration and
// RequestStop use the same lock, so request-before-bind cannot miss cancellation.
func (s *Store) RegisterSelectedStop(jobID, attemptID string, cancel context.CancelFunc) func() bool {
	var unregister func() bool
	_ = s.WithLock(func() error {
		unregister = s.RegisterSelectedStopLocked(jobID, attemptID, cancel)
		return nil
	})
	return unregister
}

// RegisterSelectedStopLocked replaces the selected cancel while the caller holds WithLock.
// Its release reports a matching saved request under that same lock.
func (s *Store) RegisterSelectedStopLocked(jobID, attemptID string, cancel context.CancelFunc) func() bool {
	registration := &selectedStopCancel{jobID: jobID, attemptID: attemptID, cancel: cancel}
	s.activeStop = registration
	if accepted := s.Data.Accepted; accepted != nil && accepted.JobID == jobID &&
		accepted.AttemptID == attemptID && accepted.StopRequest != (StopRequest{}) {
		cancel()
	}
	return func() bool {
		stopped := false
		_ = s.WithLock(func() error {
			if s.activeStop == registration {
				s.activeStop = nil
			}
			if accepted := s.Data.Accepted; accepted != nil && accepted.JobID == jobID &&
				accepted.AttemptID == attemptID && accepted.StopRequest != (StopRequest{}) {
				stopped = true
			}
			return nil
		})
		return stopped
	}
}

// ResolveStop records actual worker cleanup evidence without changing the
// already chosen terminal outcome. Recovery calls this with unverified if the
// process exited before it could observe the worker's return.
func (s *Store) ResolveStop(jobID, attemptID, status string) error {
	if status != "confirmed" && status != "unverified" {
		return errors.New("invalid stop resolution")
	}
	return s.WithLock(func() error {
		accepted := s.Data.Accepted
		if accepted == nil || accepted.JobID != jobID || accepted.AttemptID != attemptID || accepted.StopRequest == (StopRequest{}) {
			return nil
		}
		if accepted.StopRequest.Status == "requested" {
			accepted.StopRequest.Status = status
		}
		if decision, ok := s.Data.TerminalDecisions[jobID]; ok && decision.AttemptID == attemptID {
			decision.StopRequest = accepted.StopRequest
			s.Data.TerminalDecisions[jobID] = decision
		}
		if err := s.Save(); err != nil {
			return fmt.Errorf("save stop resolution: %w", err)
		}
		s.savedStop = savedStopRequest{jobID, attemptID, accepted.StopRequest}
		return nil
	})
}
