package action

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/monitor"
)

func TestInactivityTimerUsesProgressAndStopsAtUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		watch := startInactivityTimer(time.Now(), 5*time.Second, cancel)
		t.Cleanup(watch.stop)
		time.Sleep(4 * time.Second)
		watch.progress()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("fresh execution progress did not renew inactivity: %v", context.Cause(ctx))
		}
		watch.stop() // Final upload uses transfer and hard-runtime limits.
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("upload was canceled by the stopped inactivity timer: %v", context.Cause(ctx))
		}
	})
}

func TestInactivityTimerExpiresFromAssignmentReceipt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		start := time.Now()
		time.Sleep(3 * time.Second) // Assignment admission consumed part of the idle budget.
		watch := startInactivityTimer(start, 5*time.Second, cancel)
		t.Cleanup(watch.stop)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !errors.Is(context.Cause(ctx), errExecutorInactivity) || !errors.Is(context.Cause(ctx), monitor.ErrTerminalJob) {
			t.Fatalf("assignment inactivity cause = %v", context.Cause(ctx))
		}
		watch.progress() // A late event cannot revive an expired action.
		if !errors.Is(context.Cause(ctx), errExecutorInactivity) {
			t.Fatalf("late progress changed the first stop cause: %v", context.Cause(ctx))
		}
	})
}
