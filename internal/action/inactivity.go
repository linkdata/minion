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
// The assignment receipt time is its origin even when admission takes time.
type inactivityTimer struct {
	mu      sync.Mutex
	timer   *time.Timer
	last    time.Time
	timeout time.Duration
	cancel  context.CancelCauseFunc
	stopped bool
}

func startInactivityTimer(start time.Time, timeout time.Duration, cancel context.CancelCauseFunc) *inactivityTimer {
	w := &inactivityTimer{last: start, timeout: timeout, cancel: cancel}
	w.timer = time.AfterFunc(max(0, timeout-time.Since(start)), w.expire)
	return w
}

func (w *inactivityTimer) expire() {
	w.mu.Lock()
	if w.stopped {
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

func (w *inactivityTimer) stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		w.timer.Stop()
		if !time.Now().Before(w.last.Add(w.timeout)) {
			w.cancel(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity))
		}
	}
	w.mu.Unlock()
}
