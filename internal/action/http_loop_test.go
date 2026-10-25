package action

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRunHTTPLoopStopsOnInvalidNext(t *testing.T) {
	var pulls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/next" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		pulls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{")
	}))
	defer server.Close()
	err := RunHTTPLoop(t.Context(), server.Client(), server.URL, "local", &Runner{runModel: forbidModel(t)})
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || pulls.Load() != 1 {
		t.Fatalf("invalid next response retried or became shutdown: pulls=%d error=%v", pulls.Load(), err)
	}
}

func TestRunHTTPLoopCanceledHeldPullIsCleanShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/next" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunHTTPLoop(ctx, server.Client(), server.URL, "local", &Runner{runModel: forbidModel(t)})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("worker did not hold a pull: %v", ctx.Err())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled idle pull became a worker failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled idle pull did not return")
	}
}

func TestRunHTTPLoopStopsAfterMissingOutcomeACK(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	job := testJob(command.Comment)
	parent := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	client := *parent.server.Client()
	underlying := client.Transport
	var pulls, calls atomic.Int32
	client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/next" {
			pulls.Add(1)
		}
		response, err := underlying.RoundTrip(r)
		if err == nil && r.URL.Path == "/outcome" {
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader("{"))
		}
		return response, err
	})
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, _ modelRun) (aicli.Result, error) {
		calls.Add(1)
		return aicli.Result{Answer: "Worker answer", UsageComplete: true}, nil
	}}
	err := RunHTTPLoop(t.Context(), &client, parent.server.URL, job.Identity.ExecutorID, runner)
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || pulls.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("missing outcome ACK retried model work: pulls=%d calls=%d error=%v", pulls.Load(), calls.Load(), err)
	}
	got := parent.result(t)
	if got.err != nil || got.result.Answer != "Worker answer" {
		t.Fatalf("parent received outcome was changed by missing child ACK: result=%+v error=%v", got.result, got.err)
	}
}
