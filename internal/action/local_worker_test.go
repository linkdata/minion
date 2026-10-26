package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func startHTTPSWorkerTest(t *testing.T, mode string) *localWorker {
	t.Helper()
	w, err := startLocalWorker("local", os.Args[0], []string{"-test.run=^TestLocalWorkerHTTPSHelper$"}, 5*time.Second, WorkerCredentials{}, func(cmd *exec.Cmd, access LocalEndpointAccess) error {
		input, err := json.Marshal(access)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = append(os.Environ(), "MINION_HTTPS_WORKER_TEST="+mode)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestLocalWorkerRequiresAuthenticatedHeldPull(t *testing.T) {
	w := startHTTPSWorkerTest(t, "idle")
	job := testJob(command.Comment)
	if result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{}); err == nil || !strings.Contains(err.Error(), "not ready") || !result.matchesJob(job) {
		t.Fatalf("assignment bypassed capacity proof: result=%+v error=%v", result, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatalf("live held pull lost ready capacity: %v", err)
	}
	if err := w.Close(); err != nil || w.cmd.ProcessState == nil || !w.cmd.ProcessState.Success() || w.stopSent.Load() {
		t.Fatalf("idle worker failed graceful close: error=%v state=%v direct-stop=%v", err, w.cmd.ProcessState, w.stopSent.Load())
	}
}

func TestLocalWorkerDirectStopAndCrashAreDistinct(t *testing.T) {
	t.Run("direct stop", func(t *testing.T) {
		w := startHTTPSWorkerTest(t, "ignore-idle-cancel")
		w.stopGrace = 25 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := w.WaitCapacity(ctx); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil || !w.stopSent.Load() || w.cmd.ProcessState == nil {
			t.Fatalf("direct stop failed: error=%v sent=%v state=%v", err, w.stopSent.Load(), w.cmd.ProcessState)
		}
	})
	t.Run("crash before pull", func(t *testing.T) {
		w := startHTTPSWorkerTest(t, "crash-before-pull")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := w.WaitCapacity(ctx); err == nil || w.cmd.ProcessState == nil || w.cmd.ProcessState.Success() {
			t.Fatalf("crash became capacity or a clean stop: error=%v state=%v", err, w.cmd.ProcessState)
		}
	})
}

func TestLocalWorkerCrashAndCleanupFailureRetireCapacity(t *testing.T) {
	for _, mode := range []string{"crash-after-job", "cleanup-failed"} {
		t.Run(mode, func(t *testing.T) {
			w := startHTTPSWorkerTest(t, mode)
			w.executor.outcomeGrace = 25 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := w.WaitCapacity(ctx); err != nil {
				t.Fatal(err)
			}
			job := testJob(command.Comment)
			result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{})
			if !result.matchesJob(job) || w.reuse.Load() != workerRetired || w.cmd.ProcessState == nil {
				t.Fatalf("worker retained capacity: result=%+v error=%v state=%d process=%v", result, err, w.reuse.Load(), w.cmd.ProcessState)
			}
			if mode == "crash-after-job" && (!errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !strings.Contains(err.Error(), "exited")) {
				t.Fatalf("missing outcome became known: %v", err)
			}
			if mode == "cleanup-failed" && (err != nil || !w.unclean.Load() || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "cleanup_failed") {
				t.Fatalf("known cleanup failure was lost: result=%+v error=%v unclean=%v", result, err, w.unclean.Load())
			}
		})
	}
}

func TestLocalWorkerKnownOutcomeSurvivesChildExit(t *testing.T) {
	w := startHTTPSWorkerTest(t, "known-then-exit")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{})
	if err != nil || result.Answer != "known" || !result.matchesJob(job) {
		t.Fatalf("accepted result changed after child exit: result=%+v error=%v", result, err)
	}
	select {
	case <-w.exited:
	case <-ctx.Done():
		t.Fatal("known-result child was not reaped")
	}
	if w.waitErr == nil || w.cmd.ProcessState == nil {
		t.Fatalf("unexpected exit was not observed: wait=%v state=%v", w.waitErr, w.cmd.ProcessState)
	}
	if err := w.WaitCapacity(ctx); err == nil {
		t.Fatal("exited child gained another ready slot")
	}
	var exit *exec.ExitError
	if err := w.Close(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("known result hid separate close error: %v", err)
	}
}

func TestLocalWorkerListenerLossCannotSupplyCapacity(t *testing.T) {
	w := startHTTPSWorkerTest(t, "idle")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.endpoint.serveDone:
	case <-ctx.Done():
		t.Fatal("listener loss did not stop serving")
	}
	if err := w.WaitCapacity(ctx); err == nil || !errors.Is(err, ErrLocalWorkerUnavailable) {
		t.Fatalf("lost listener supplied capacity: %v", err)
	}
}

func TestLocalWorkerStartupFailureClosesListener(t *testing.T) {
	var address string
	configurationFailure := errors.New("fixture configuration failure")
	if worker, err := startLocalWorker("local", os.Args[0], nil, time.Second, WorkerCredentials{}, func(_ *exec.Cmd, access LocalEndpointAccess) error {
		address = strings.TrimPrefix(access.BaseURL, "https://")
		return configurationFailure
	}); worker != nil || !errors.Is(err, configurationFailure) {
		t.Fatalf("invalid startup continued: worker=%v error=%v", worker, err)
	}
	if connection, err := net.DialTimeout("tcp4", address, 100*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("failed startup retained its listener")
	}
}

func TestLocalWorkerHTTPSHelper(t *testing.T) {
	mode := os.Getenv("MINION_HTTPS_WORKER_TEST")
	if mode == "" {
		return
	}
	var access LocalEndpointAccess
	if err := json.NewDecoder(os.Stdin).Decode(&access); err != nil {
		t.Fatal(err)
	}
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if mode == "crash-before-pull" {
		os.Exit(7)
	}
	if mode == "idle" || mode == "ignore-idle-cancel" {
		if err := RunHTTPLoop(t.Context(), client, access.BaseURL, "local", &Runner{}); err != nil {
			t.Fatal(err)
		}
		if mode == "ignore-idle-cancel" {
			select {}
		}
		return
	}
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "local"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, access.BaseURL+"/next", bytes.NewReader(pull))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("worker pull: response=%v error=%v", response, err)
	}
	var assignment httpAssignment
	if err := httpDecode(response.Body, httpJobBytes, &assignment); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if mode == "crash-after-job" {
		os.Exit(7)
	}
	result := Result{Version: ProtocolVersion, Identity: assignment.Job.Identity, SourceRef: assignment.Job.Source.Head, Answer: "known"}
	if mode == "cleanup-failed" {
		result.Diagnostics = []Diagnostic{{Code: "cleanup_failed", Message: "fixture cleanup failed"}}
	}
	outcome, err := httpEncode(httpOutcome{Sequence: 1, Result: result}, httpOutcomeBytes)
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, access.BaseURL+"/outcome", bytes.NewReader(outcome))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("worker outcome: response=%v error=%v", response, err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if mode == "known-then-exit" {
		os.Exit(7)
	}
	select {}
}
