package main

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

type blockedConfigUpdateHandler struct {
	entered chan struct{}
	release chan struct{}
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
