package action

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
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

func TestInactivityTimerStopEnforcesElapsedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		watch := &inactivityTimer{last: time.Now().Add(-6 * time.Second), timeout: 5 * time.Second, cancel: cancel}
		watch.timer = time.AfterFunc(time.Hour, watch.expire) // Simulate a delayed expiry callback.
		watch.stop()
		if !errors.Is(context.Cause(ctx), errExecutorInactivity) || !errors.Is(context.Cause(ctx), monitor.ErrTerminalJob) {
			t.Fatalf("stopped overdue timer lost inactivity cause: %v", context.Cause(ctx))
		}
	})
}

func TestRunnerRunEnforcesInactivityAfterLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = time.Minute
		job.InactivityTimeout = 5 * time.Second
		root := t.TempDir()
		runner := Runner{WorkRoot: root, runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
			if request.onProgress == nil {
				t.Fatal("direct model has no inactivity progress callback")
			}
			<-ctx.Done()
			return aicli.Result{Answer: proposalJSON("Late success.", "")}, nil
		}}
		result, err := runner.Run(t.Context(), job)
		if !errors.Is(err, errExecutorInactivity) || !errors.Is(err, monitor.ErrTerminalJob) ||
			errors.Is(err, context.DeadlineExceeded) || len(result.Calls) != 1 || result.Answer != "" || runner.executionProgress != nil {
			t.Fatalf("direct Run inactivity: result=%+v error=%v", result, err)
		}
		assertEmptyDir(t, root)
	})
}

func TestRunnerExecuteProgressRenewsInactivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = time.Minute
		job.InactivityTimeout = 5 * time.Second
		runner := Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
			if request.onProgress == nil {
				t.Fatal("direct model has no inactivity progress callback")
			}
			time.Sleep(4 * time.Second)
			request.onProgress()
			time.Sleep(4 * time.Second)
			synctest.Wait()
			if ctx.Err() != nil {
				t.Fatalf("direct progress did not renew inactivity: %v", context.Cause(ctx))
			}
			return aicli.Result{Answer: "Completed."}, nil
		}}
		result, err := runner.Execute(t.Context(), job, runnerExecutionCheckpoints{})
		if err != nil || result.Answer != "Completed." || len(result.Calls) != 1 || runner.executionProgress != nil {
			t.Fatalf("direct Execute after progress: result=%+v error=%v", result, err)
		}
	})
}
