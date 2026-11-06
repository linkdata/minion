package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

type blockedConfigUpdateHandler struct {
	entered chan struct{}
	release chan struct{}
}

type joinedConfigUpdateHandler struct{ failure error }

func (h *joinedConfigUpdateHandler) WaitCapacity(context.Context) error {
	return errors.Join(h.failure, monitor.ErrConfigUpdatePending)
}

func (*joinedConfigUpdateHandler) Execute(context.Context, monitor.Job) error {
	return errors.New("unexpected selected execution")
}

// clearAcceptedOnRetryContext models recovery clearing the accepted action on
// the monitor goroutine before it handles the pending configuration update.
type clearAcceptedOnRetryContext struct {
	context.Context
	store        *state.Store
	retryAt      time.Time
	retryReached chan<- struct{}
	clear        <-chan struct{}
}

func (c clearAcceptedOnRetryContext) Err() error {
	if !time.Now().Before(c.retryAt) && c.store.Data.Accepted != nil {
		c.retryReached <- struct{}{}
		<-c.clear
		c.store.Data.Accepted = nil
	}
	return c.Context.Err()
}

func (h *blockedConfigUpdateHandler) WaitCapacity(ctx context.Context) error {
	close(h.entered)
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*blockedConfigUpdateHandler) Execute(context.Context, monitor.Job) error {
	return errors.New("unexpected selected execution")
}

func TestRunMonitorLoopConfigUpdatePrecedesFirstPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
		handler := &blockedConfigUpdateHandler{entered: make(chan struct{}), release: make(chan struct{})}
		watcher := &monitor.Monitor{Handler: handler, SelectedExecutorID: "local"}
		updates := make(chan struct{}, 1)
		updates <- struct{}{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan int, 1)
		go func() {
			result <- runMonitorLoopWithConfigUpdates(ctx, watcher, store, true, nil, log.New(io.Discard, "", 0), false, updates)
		}()
		synctest.Wait()
		select {
		case code := <-result:
			if code != monitorConfigChanged {
				t.Fatalf("queued update returned %d, want config change", code)
			}
		case <-handler.entered:
			cancel()
			close(handler.release)
			synctest.Wait()
			t.Fatal("monitor polled with stale configuration before applying queued update")
		default:
			t.Fatal("monitor neither applied the update nor entered its first poll")
		}
	})
}

func TestRunMonitorLoopAcceptanceStateWinsConfigUpdate(t *testing.T) {
	now := time.Now().UTC()
	store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
	watcher := &monitor.Monitor{Handler: &joinedConfigUpdateHandler{failure: monitor.ErrAcceptanceState}, SelectedExecutorID: "local"}
	if code := runMonitorLoopWithConfigUpdates(t.Context(), watcher, store, true, nil, log.New(io.Discard, "", 0), false, nil); code != 1 {
		t.Fatalf("uncertain acceptance with queued update returned %d, want fatal exit", code)
	}
}

func TestRunMonitorLoopLogsPollFailureBeforeConfigUpdate(t *testing.T) {
	now := time.Now().UTC()
	store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
	watcher := &monitor.Monitor{Handler: &joinedConfigUpdateHandler{failure: errors.New("earlier poll failure")}, SelectedExecutorID: "local"}
	var logs bytes.Buffer
	if code := runMonitorLoopWithConfigUpdates(t.Context(), watcher, store, true, nil, log.New(&logs, "", 0), false, nil); code != monitorConfigChanged {
		t.Fatalf("config update with prior poll failure returned %d, want config change", code)
	}
	if !strings.Contains(logs.String(), "earlier poll failure") {
		t.Fatalf("prior poll failure was hidden: %q", logs.String())
	}
}

func TestRunMonitorLoopCancellationWinsQueuedUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
		handler := &blockedConfigUpdateHandler{entered: make(chan struct{}), release: make(chan struct{})}
		watcher := &monitor.Monitor{Handler: handler, SelectedExecutorID: "local"}
		updates := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan int, 1)
		go func() {
			result <- runMonitorLoopWithConfigUpdates(ctx, watcher, store, true, nil, log.New(io.Discard, "", 0), false, updates)
		}()
		<-handler.entered
		updates <- struct{}{}
		cancel()
		close(handler.release)
		synctest.Wait()
		select {
		case code := <-result:
			if code != 0 {
				t.Fatalf("cancellation with queued update returned %d, want clean shutdown", code)
			}
		default:
			t.Fatal("monitor did not stop after cancellation")
		}
	})
}

func TestRunMonitorLoopConfigUpdateWakesScheduledSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
		updates := make(chan struct{}, 1)
		result := make(chan int, 1)
		go func() {
			result <- runMonitorLoopWithConfigUpdates(t.Context(), &monitor.Monitor{}, store, true, nil, log.New(io.Discard, "", 0), false, updates)
		}()
		synctest.Wait()
		updates <- struct{}{}
		synctest.Wait()
		select {
		case code := <-result:
			if code != monitorConfigChanged || !time.Now().Before(store.Data.NextPollAt) {
				t.Fatalf("config update did not wake idle monitor: code=%d now=%s next=%s", code, time.Now(), store.Data.NextPollAt)
			}
		default:
			t.Fatal("config update did not wake idle monitor")
		}
	})
}

func TestRunMonitorLoopConfigUpdateWaitsForIteration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
		handler := &blockedConfigUpdateHandler{entered: make(chan struct{}), release: make(chan struct{})}
		watcher := &monitor.Monitor{Handler: handler, SelectedExecutorID: "local"}
		updates := make(chan struct{}, 1)
		result := make(chan int, 1)
		go func() {
			result <- runMonitorLoopWithConfigUpdates(t.Context(), watcher, store, true, nil, log.New(io.Discard, "", 0), false, updates)
		}()
		<-handler.entered
		updates <- struct{}{}
		synctest.Wait()
		select {
		case code := <-result:
			t.Fatalf("config update interrupted active iteration: code=%d", code)
		default:
		}
		close(handler.release)
		synctest.Wait()
		select {
		case code := <-result:
			if code != monitorConfigChanged {
				t.Fatalf("completed iteration returned %d, want config update", code)
			}
		default:
			t.Fatal("config update was not observed after iteration")
		}
	})
}

func TestRunMonitorLoopConfigUpdateWakesRemoteCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now})
		handler := &blockedConfigUpdateHandler{entered: make(chan struct{}), release: make(chan struct{})}
		watcher := &monitor.Monitor{
			Config:             config.Config{RemoteExecutor: &config.RemoteExecutorConfig{}},
			Handler:            handler,
			SelectedExecutorID: "remote",
		}
		updates := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan int, 1)
		go func() {
			result <- runMonitorLoopWithConfigUpdates(ctx, watcher, store, true, nil, log.New(io.Discard, "", 0), false, updates)
		}()
		<-handler.entered
		updates <- struct{}{}
		synctest.Wait()
		select {
		case code := <-result:
			if code != monitorConfigChanged || store.Data.Accepted != nil || !store.Data.NextPollAt.Equal(now) {
				t.Fatalf("remote capacity update returned %d with accepted=%v, next=%s", code, store.Data.Accepted, store.Data.NextPollAt)
			}
		default:
			t.Fatal("config update did not wake remote capacity wait")
		}
	})
}

func TestRunMonitorLoopConfigUpdateWaitsForAcceptedAction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		accepted := dashboardTestAccepted(now)
		accepted.RetryAt = now.Add(5 * time.Minute)
		store := serviceStore(t, state.Data{Accepted: accepted, NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
		updates := make(chan struct{}, 1)
		updates <- struct{}{}
		base, cancel := context.WithCancel(t.Context())
		defer cancel()
		retryReached, clear := make(chan struct{}), make(chan struct{}, 1)
		defer close(clear)
		ctx := clearAcceptedOnRetryContext{Context: base, store: store, retryAt: accepted.RetryAt, retryReached: retryReached, clear: clear}
		result := make(chan int, 1)
		go func() {
			result <- runMonitorLoopWithConfigUpdates(ctx, &monitor.Monitor{}, store, true, nil, log.New(io.Discard, "", 0), false, updates)
		}()
		select {
		case <-retryReached: // The accepted action survived the first monitor iteration.
		case code := <-result:
			t.Fatalf("config update interrupted accepted action: code=%d", code)
		}
		if store.Data.Accepted == nil {
			t.Fatal("accepted action cleared before its retry")
		}
		clear <- struct{}{}
		synctest.Wait()
		select {
		case code := <-result:
			if code != monitorConfigChanged {
				t.Fatalf("completed accepted action returned %d, want config update", code)
			}
		default:
			t.Fatal("pending config update was not applied after accepted action cleared")
		}
	})
}
