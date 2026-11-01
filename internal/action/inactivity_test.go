package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
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

func TestDirectRunnerPreparedInputRenewsInactivity(t *testing.T) {
	for _, execute := range []bool{false, true} {
		t.Run(map[bool]string{false: "Run", true: "Execute"}[execute], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				job := testJob(command.Comment)
				job.RuntimeBudget = time.Minute
				job.InactivityTimeout = 5 * time.Second
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
					if request.onInput == nil {
						t.Fatal("direct model has no prepared-input progress callback")
					}
					time.Sleep(4 * time.Second)
					request.onInput(aicli.PreparedInput{Path: "/fixture/cli", Args: []string{"cli"}, Dir: request.dir, Stdin: request.prompt})
					time.Sleep(4 * time.Second)
					synctest.Wait()
					if ctx.Err() != nil {
						t.Fatalf("prepared input did not renew inactivity: %v", context.Cause(ctx))
					}
					return aicli.Result{Answer: "Completed."}, nil
				}}
				var result Result
				var err error
				if execute {
					result, err = runner.Execute(t.Context(), job, runnerExecutionCheckpoints{})
				} else {
					result, err = runner.Run(t.Context(), job)
				}
				if err != nil || result.Answer != "Completed." || len(result.Calls) != 1 {
					t.Fatalf("direct Runner prepared input: result=%+v error=%v", result, err)
				}
			})
		})
	}
}

func TestRunnerCreditsEachUnpackedSourceFileAndStopsOnCancellation(t *testing.T) {
	job := testJob(command.Comment)
	job.Source.Files = []workspace.File{
		{Path: "first.txt", Mode: workspace.Regular, Data: []byte("first\n")},
		{Path: "second.txt", Mode: workspace.Regular, Data: []byte("second\n")},
	}
	attachRepository(t, &job)
	workRoot := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstOnly := false
	runner := Runner{WorkRoot: workRoot,
		statusActivity: func() {
			entries, err := os.ReadDir(workRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), "minion-") {
					continue
				}
				source := filepath.Join(workRoot, entry.Name())
				_, firstErr := os.Stat(filepath.Join(source, "first.txt"))
				_, secondErr := os.Stat(filepath.Join(source, "second.txt"))
				if firstErr == nil && errors.Is(secondErr, os.ErrNotExist) {
					firstOnly = true
					cancel()
				}
			}
		},
		runModel: func(context.Context, modelRun) (aicli.Result, error) {
			return aicli.Result{}, errors.New("model started after unpack cancellation")
		},
	}
	_, err := runner.Run(ctx, job)
	if !firstOnly || !errors.Is(err, context.Canceled) {
		t.Fatalf("unpacking did not expose completed file and stop: firstOnly=%t error=%v", firstOnly, err)
	}
	assertEmptyDir(t, workRoot)
}

func TestContextCreditsEachUnpackedBaseFileAndStopsOnCancellation(t *testing.T) {
	job := testJob(command.Review)
	job.Source.BaseFiles = []workspace.File{
		{Path: "first.txt", Mode: workspace.Regular, Data: []byte("first\n")},
		{Path: "second.txt", Mode: workspace.Regular, Data: []byte("second\n")},
	}
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstOnly := false
	err := materializeContext(ctx, root, job.Source, func() {
		_, firstErr := os.Stat(filepath.Join(root, "base", "first.txt"))
		_, secondErr := os.Stat(filepath.Join(root, "base", "second.txt"))
		if firstErr == nil && errors.Is(secondErr, os.ErrNotExist) {
			firstOnly = true
			cancel()
		}
	})
	if !firstOnly || !errors.Is(err, context.Canceled) {
		t.Fatalf("base unpacking did not expose completed file and stop: firstOnly=%t error=%v", firstOnly, err)
	}
}
