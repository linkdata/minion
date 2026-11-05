package action

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/linkdata/minion/internal/monitor"
)

var errExecutorInactivity = errors.New("executor inactivity timeout")

// inactivityTimer measures execution progress, not HTTP contact or Status delivery.
// The caller supplies its origin so admission time counts before first progress.
type inactivityTimer struct {
	mu                    sync.Mutex
	timer                 *time.Timer
	last                  time.Time
	pausedAt              time.Time
	timeout               time.Duration
	cancel                context.CancelCauseFunc
	stopped               bool
	stoppedBeforeDeadline bool
}

func startInactivityTimer(start time.Time, timeout time.Duration, cancel context.CancelCauseFunc) *inactivityTimer {
	w := &inactivityTimer{last: start, timeout: timeout, cancel: cancel}
	w.timer = time.AfterFunc(max(0, timeout-time.Since(start)), w.expire)
	w.expire()
	return w
}

func (w *inactivityTimer) expire() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	if !w.pausedAt.IsZero() {
		w.mu.Unlock()
		return
	}
	if remaining := w.timeout - time.Since(w.last); remaining > 0 {
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.cancel(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity))
	w.mu.Unlock()
}

func (w *inactivityTimer) progress() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	if !w.pausedAt.IsZero() {
		w.mu.Unlock()
		return
	}
	now := time.Now()
	if !now.Before(w.last.Add(w.timeout)) {
		w.stopped = true
		w.timer.Stop()
		w.cancel(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity))
		w.mu.Unlock()
		return
	}
	w.last = now
	w.timer.Reset(w.timeout)
	w.mu.Unlock()
}

// pause excludes selected CLI discovery from model inactivity while the
// absolute action deadline continues to run.
func (w *inactivityTimer) pause() func() {
	w.mu.Lock()
	now := time.Now()
	if w.stopped || !now.Before(w.last.Add(w.timeout)) {
		w.mu.Unlock()
		w.expire()
		return func() {}
	}
	w.pausedAt = now
	w.timer.Stop()
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		if !w.stopped && !w.pausedAt.IsZero() {
			w.last = w.last.Add(time.Since(w.pausedAt))
			w.pausedAt = time.Time{}
			w.timer.Reset(max(0, time.Until(w.last.Add(w.timeout))))
		}
		w.mu.Unlock()
	}
}

func (w *inactivityTimer) stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		w.timer.Stop()
		deadline := w.last.Add(w.timeout)
		if !w.pausedAt.IsZero() {
			deadline = deadline.Add(time.Since(w.pausedAt))
		}
		if time.Now().Before(deadline) {
			w.stoppedBeforeDeadline = true
		} else {
			w.cancel(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity))
		}
	}
	w.mu.Unlock()
}

func (w *inactivityTimer) deadline() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stoppedBeforeDeadline {
		return time.Time{} // Completion ended the inactivity window.
	}
	deadline := w.last.Add(w.timeout)
	if !w.pausedAt.IsZero() {
		deadline = deadline.Add(time.Since(w.pausedAt))
	}
	return deadline
}
