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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pulls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/next" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		if pulls.Add(1) > 1 {
			cancel()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{")
	}))
	defer server.Close()
	err := RunHTTPLoop(ctx, server.Client(), server.URL, "local", &Runner{runModel: forbidModel(t)})
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || pulls.Load() != 1 {
		t.Fatalf("invalid next response retried or became shutdown: pulls=%d error=%v", pulls.Load(), err)
	}
}

func TestRunHTTPLoopCanceledBeforePullIsCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var sent atomic.Bool
	client := &http.Client{Transport: httpClientTestTransport(func(*http.Request) (*http.Response, error) {
		sent.Store(true)
		return nil, errors.New("unexpected pull")
	})}
	runner := &Runner{runModel: forbidModel(t)}
	if err := RunHTTPLoop(ctx, client, "http://minion", "local", runner); err != nil || sent.Load() {
		t.Fatalf("canceled pre-pull worker failed or sent a request: error=%v sent=%t", err, sent.Load())
	}
	if err := RunHTTP(ctx, client, "http://minion", "local", runner); !errors.Is(err, context.Canceled) || sent.Load() {
		t.Fatalf("one-attempt API changed cancellation: error=%v sent=%t", err, sent.Load())
	}
}

func TestRunHTTPLoopCanceledHeldPullWithoutACKIsUncertain(t *testing.T) {
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
		if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled pull without a parent reply was treated as idle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled idle pull did not return")
	}
}

type partialAssignmentBody struct {
	ctx     context.Context
	reading chan struct{}
	reads   int
}

func (b *partialAssignmentBody) Read(p []byte) (int, error) {
	if b.reads == 0 {
		b.reads++
		return copy(p, "{"), nil
	}
	if b.reads == 1 {
		b.reads++
		close(b.reading)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*partialAssignmentBody) Close() error { return nil }

func TestRunHTTPLoopDoesNotHidePartialAssignmentOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &partialAssignmentBody{ctx: ctx, reading: make(chan struct{})}
	client := &http.Client{Transport: httpClientTestTransport(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body}, nil
	})}
	done := make(chan error, 1)
	go func() { done <- RunHTTPLoop(ctx, client, "http://minion", "local", &Runner{runModel: forbidModel(t)}) }()
	select {
	case <-body.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not read the partial assignment")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(err, context.Canceled) {
			t.Fatalf("partial assignment became clean idle shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled partial assignment did not return")
	}
}

func TestRunHTTPLoopStopsAfterMissingOutcomeACK(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job := testJob(command.Comment)
	parent := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	client := *parent.server.Client()
	underlying := client.Transport
	var pulls, calls atomic.Int32
	client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/next" && pulls.Add(1) > 1 {
			cancel()
			return nil, context.Canceled
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
	err := RunHTTPLoop(ctx, &client, parent.server.URL, job.Identity.ExecutorID, runner)
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || pulls.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("missing outcome ACK retried model work: pulls=%d calls=%d error=%v", pulls.Load(), calls.Load(), err)
	}
	got := parent.result(t)
	if got.err != nil || got.result.Answer != "Worker answer" {
		t.Fatalf("parent received outcome was changed by missing child ACK: result=%+v error=%v", got.result, got.err)
	}
}

func TestRunHTTPLoopReportsWatchFailureAfterOutcomeACK(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job := testJob(command.Comment)
	parent := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	client := *parent.server.Client()
	underlying := client.Transport
	triggerCtx, trigger := context.WithCancelCause(t.Context())
	var pulls, watchReads atomic.Int32
	client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/next" && pulls.Add(1) > 1 {
			cancel()
			return nil, context.Canceled
		}
		response, err := underlying.RoundTrip(r)
		if err != nil {
			return response, err
		}
		switch r.URL.Path {
		case "/cancel":
			response.Body = httpClientGatedWatchBody{ReadCloser: response.Body, reader: strings.NewReader("{"), trigger: triggerCtx.Done(), reads: &watchReads}
		case "/outcome":
			response.Body = httpClientCancelOnCloseBody{ReadCloser: response.Body, cancel: trigger, cause: context.Canceled}
		}
		return response, nil
	})
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		return aicli.Result{Answer: "Worker answer", UsageComplete: true}, nil
	}}
	err := RunHTTPLoop(ctx, &client, parent.server.URL, job.Identity.ExecutorID, runner)
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !strings.Contains(err.Error(), "HTTP cancel") || pulls.Load() != 1 || watchReads.Load() == 0 {
		t.Fatalf("watch failure after outcome ACK was hidden: pulls=%d reads=%d error=%v", pulls.Load(), watchReads.Load(), err)
	}
	if got := parent.result(t); got.err != nil || got.result.Answer != "Worker answer" {
		t.Fatalf("watch failure altered acknowledged result: %+v", got)
	}
}

func TestRunHTTPLoopStopsAfterMalformedRequiredACK(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job := testJob(command.Comment)
	parent := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	client := *parent.server.Client()
	underlying := client.Transport
	var pulls, calls atomic.Int32
	client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/next" && pulls.Add(1) > 1 {
			cancel()
			return nil, context.Canceled
		}
		response, err := underlying.RoundTrip(r)
		if err == nil && r.URL.Path == "/before" {
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader("{"))
		}
		return response, err
	})
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, _ modelRun) (aicli.Result, error) {
		calls.Add(1)
		return aicli.Result{Answer: "Worker answer", UsageComplete: true}, nil
	}}
	err := RunHTTPLoop(ctx, &client, parent.server.URL, job.Identity.ExecutorID, runner)
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || pulls.Load() != 1 || calls.Load() != 0 {
		t.Fatalf("malformed required ACK allowed another pull or model call: pulls=%d calls=%d error=%v", pulls.Load(), calls.Load(), err)
	}
	got := parent.result(t)
	if got.err == nil || !got.result.matchesJob(job) {
		t.Fatalf("parent lost acknowledged failure outcome: result=%+v error=%v", got.result, got.err)
	}
}
