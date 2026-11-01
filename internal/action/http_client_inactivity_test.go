package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type httpInactivityFixture struct {
	parent         *httpServerTest
	modelStarted   chan struct{}
	outcomeEntered chan struct{}
	releaseOutcome func()
	done           chan error
}

type delayedHTTPDeadlineContext struct {
	context.Context
	ready <-chan struct{}
	once  sync.Once
	delay time.Duration
}

func (c *delayedHTTPDeadlineContext) Deadline() (time.Time, bool) {
	select {
	case <-c.ready:
		c.once.Do(func() { time.Sleep(c.delay) })
	default:
	}
	return c.Context.Deadline()
}

type httpPullCloseSignal struct {
	io.ReadCloser
	once  sync.Once
	ready chan<- struct{}
}

func (b *httpPullCloseSignal) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { close(b.ready) })
	return err
}

func newHTTPInactivityFixture(t *testing.T, job Job, holdOutcome bool, model func(context.Context, modelRun) (aicli.Result, error)) *httpInactivityFixture {
	t.Helper()
	f := &httpInactivityFixture{
		modelStarted: make(chan struct{}, 1), outcomeEntered: make(chan struct{}, 1),
		done: make(chan error, 1),
	}
	releaseOutcome := make(chan struct{})
	f.releaseOutcome = sync.OnceFunc(func() { close(releaseOutcome) })
	f.parent = newHTTPServerTestWithServer(t, t.Context(), job, runnerExecutionCheckpoints{},
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/outcome" {
					select {
					case f.outcomeEntered <- struct{}{}:
					default:
					}
					if holdOutcome {
						<-releaseOutcome
					}
				}
				next.ServeHTTP(w, r)
			})
		}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) })
	if holdOutcome {
		t.Cleanup(f.releaseOutcome)
	}
	client := f.parent.server.Client()
	client.Timeout = 0
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
		f.modelStarted <- struct{}{}
		return model(ctx, request)
	}}
	go func() { f.done <- RunHTTP(t.Context(), client, f.parent.server.URL, job.Identity.ExecutorID, runner) }()
	return f
}

func (f *httpInactivityFixture) waitModel(t *testing.T) {
	t.Helper()
	select {
	case <-f.modelStarted:
	case err := <-f.done:
		t.Fatalf("worker ended before model admission: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not admit the model")
	}
}

func (f *httpInactivityFixture) assertInactivity(t *testing.T) {
	t.Helper()
	err := <-f.done
	if !errors.Is(err, errExecutorInactivity) || !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("worker inactivity error = %v", err)
	}
	got := f.parent.result(t)
	var reported *workerFailureError
	if !errors.As(got.err, &reported) || !reported.failure.Terminal || !reported.failure.Inactivity ||
		reported.failure.Deadline || len(got.result.Calls) != 1 {
		t.Fatalf("parent lost terminal inactivity outcome: error=%v result=%+v", got.err, got.result)
	}
}

func TestAdmissionFailureKeepsOverdueInactivity(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	inactivity := startInactivityTimer(time.Now().Add(-time.Minute), time.Second, cancel)
	err := admissionFailure(errors.Join(monitor.ErrRequestTransport, errors.New("watch failed")), ctx, inactivity)
	failure := newHTTPFailure(err)
	if !errors.Is(err, errExecutorInactivity) || !errors.Is(err, monitor.ErrTerminalJob) ||
		failure == nil || !failure.Terminal || !failure.Inactivity || failure.Deadline || !failure.RequestTransport {
		t.Fatalf("overdue admission failure lost terminal inactivity: error=%v failure=%+v", err, failure)
	}
}

func TestRunHTTPExpiredAssignmentSkipsWatchAndModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = time.Minute
		job.InactivityTimeout = 5 * time.Second
		binding := httpJobBinding(job)
		pullClosed := make(chan struct{})
		ctx := &delayedHTTPDeadlineContext{Context: t.Context(), ready: pullClosed, delay: 6 * time.Second}
		var watchCalls, modelCalls int
		var outcome httpOutcome
		respond := func(value any) (*http.Response, error) {
			data, err := httpEncode(value, httpJobBytes)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/next":
				response, err := respond(httpAssignment{Job: job, StopGrace: httpControlTimeout})
				if err == nil {
					response.Body = &httpPullCloseSignal{ReadCloser: response.Body, ready: pullClosed}
				}
				return response, err
			case "/cancel":
				watchCalls++
				return nil, errors.New("overdue watch opened")
			case "/outcome":
				if err := httpDecode(r.Body, httpOutcomeBytes, &outcome); err != nil {
					return nil, err
				}
				return respond(httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: outcome.Sequence})
			default:
				return nil, errors.New("unexpected worker request")
			}
		})}
		runner := &Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
			modelCalls++
			return aicli.Result{}, errors.New("overdue model started")
		}}
		err := RunHTTP(ctx, client, "http://worker.test", job.Identity.ExecutorID, runner)
		if watchCalls != 0 || modelCalls != 0 {
			t.Fatalf("overdue assignment entered watch or model: watch=%d model=%d error=%v", watchCalls, modelCalls, err)
		}
		if !errors.Is(err, errExecutorInactivity) || !errors.Is(err, monitor.ErrTerminalJob) ||
			errors.Is(err, context.DeadlineExceeded) || errors.Is(err, monitor.ErrRequestTransport) ||
			outcome.Failure == nil || !outcome.Failure.Terminal || !outcome.Failure.Inactivity || outcome.Failure.Deadline ||
			outcome.Failure.RequestTransport || outcome.Failure.OutcomeUnknown {
			t.Fatalf("overdue assignment lost terminal inactivity: error=%v outcome=%+v", err, outcome)
		}
	})
}

func TestRunHTTPWatchAdmissionRespectsInactivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = time.Minute
		job.InactivityTimeout = 5 * time.Second
		binding := httpJobBinding(job)
		watchEntered := make(chan struct{}, 1)
		releaseWatch := make(chan struct{})
		defer close(releaseWatch)
		outcomes := make(chan httpOutcome, 1)
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/next":
				httpClientTestJSON(t, w, httpAssignment{Job: job, StopGrace: httpControlTimeout})
			case "/cancel":
				watchEntered <- struct{}{}
				<-releaseWatch // Never admit the watch with response headers.
			case "/outcome":
				var outcome httpOutcome
				if err := httpDecode(r.Body, httpOutcomeBytes, &outcome); err != nil {
					t.Error(err)
				}
				outcomes <- outcome
				httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: outcome.Sequence})
			default:
				t.Errorf("worker advanced before watch admission: %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		done := make(chan error, 1)
		go func() {
			done <- RunHTTP(t.Context(), server.Client(), server.URL, job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
		}()
		<-watchEntered
		time.Sleep(job.InactivityTimeout)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, errExecutorInactivity) || !errors.Is(err, monitor.ErrTerminalJob) ||
				errors.Is(err, monitor.ErrRequestTransport) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("watch admission lost inactivity cause: %v", err)
			}
		default:
			t.Fatal("stalled watch outlived the inactivity deadline")
		}
		select {
		case outcome := <-outcomes:
			if outcome.Failure == nil || !outcome.Failure.Terminal || outcome.Failure.Deadline ||
				outcome.Failure.RequestTransport || outcome.Failure.OutcomeUnknown ||
				len(outcome.Result.Calls) != 0 || outcome.Result.Answer != "" {
				t.Fatalf("watch admission reported wrong outcome: %+v", outcome)
			}
		default:
			t.Fatal("stalled watch did not send a terminal outcome")
		}
	})
}

func TestRunHTTPStatusContactDoesNotRenewInactivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.InactivityTimeout = 20 * time.Second
		f := newHTTPInactivityFixture(t, job, false, func(ctx context.Context, _ modelRun) (aicli.Result, error) {
			<-ctx.Done()
			return aicli.Result{}, ctx.Err()
		})
		f.waitModel(t)
		synctest.Wait()
		first, _, ok := f.parent.executor.Status()
		if !ok || first.Phase != "main" {
			t.Fatalf("initial model Status = %+v, present=%t", first, ok)
		}
		time.Sleep(statusHeartbeatInterval)
		synctest.Wait()
		second, _, ok := f.parent.executor.Status()
		if !ok || second.Sequence <= first.Sequence || second.ProgressSequence != first.ProgressSequence {
			t.Fatalf("Status heartbeat changed execution progress: first=%+v second=%+v", first, second)
		}
		for range 3 {
			if _, _, ok := f.parent.executor.Status(); !ok {
				t.Fatal("parent Status poll lost live worker contact")
			}
		}
		time.Sleep(job.InactivityTimeout - statusHeartbeatInterval)
		f.assertInactivity(t)
	})
}

func TestRunHTTPModelProgressRenewsInactivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.InactivityTimeout = 20 * time.Second
		progress := make(chan func(), 1)
		f := newHTTPInactivityFixture(t, job, false, func(ctx context.Context, request modelRun) (aicli.Result, error) {
			progress <- request.onProgress
			<-ctx.Done()
			return aicli.Result{}, ctx.Err()
		})
		f.waitModel(t)
		renew := <-progress
		if renew == nil {
			t.Fatal("model did not receive a progress callback")
		}
		for range 2 {
			time.Sleep(18 * time.Second)
			renew()
			synctest.Wait()
			select {
			case err := <-f.done:
				t.Fatalf("worker stopped despite model progress: %v", err)
			default:
			}
		}
		time.Sleep(19 * time.Second)
		synctest.Wait()
		select {
		case err := <-f.done:
			t.Fatalf("worker stopped before renewed inactivity deadline: %v", err)
		default:
		}
		time.Sleep(2 * time.Second)
		f.assertInactivity(t)
	})
}

func TestRunHTTPHardRuntimeCapsChattyModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = 25 * time.Second
		job.InactivityTimeout = 20 * time.Second
		progress := make(chan func(), 1)
		f := newHTTPInactivityFixture(t, job, false, func(ctx context.Context, request modelRun) (aicli.Result, error) {
			progress <- request.onProgress
			<-ctx.Done()
			return aicli.Result{}, ctx.Err()
		})
		f.waitModel(t)
		renew := <-progress
		time.Sleep(15 * time.Second)
		renew()
		time.Sleep(8 * time.Second)
		renew()
		time.Sleep(3 * time.Second)
		err := <-f.done
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, errExecutorInactivity) {
			t.Fatalf("hard runtime did not cap chatty model: %v", err)
		}
		got := f.parent.result(t)
		var reported *workerFailureError
		if !errors.As(got.err, &reported) || !reported.failure.Deadline || reported.failure.Inactivity || len(got.result.Calls) != 1 {
			t.Fatalf("parent lost hard-runtime outcome: error=%v result=%+v", got.err, got.result)
		}
	})
}

func TestRunHTTPStopsInactivityDuringOutcomeUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = time.Minute
		job.InactivityTimeout = 10 * time.Second
		f := newHTTPInactivityFixture(t, job, true, func(context.Context, modelRun) (aicli.Result, error) {
			return aicli.Result{Answer: "Complete", Tokens: usage.Tokens{Input: 1}, UsageComplete: true}, nil
		})
		select {
		case <-f.outcomeEntered:
		case err := <-f.done:
			t.Fatalf("worker ended before outcome upload: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not begin outcome upload")
		}
		synctest.Wait()
		first, _, ok := f.parent.executor.Status()
		if !ok || first.Phase != "upload" {
			t.Fatalf("upload Status = %+v, present=%t", first, ok)
		}
		time.Sleep(statusHeartbeatInterval)
		synctest.Wait()
		second, _, ok := f.parent.executor.Status()
		if !ok || second.Sequence <= first.Sequence || second.ProgressSequence != first.ProgressSequence {
			t.Fatalf("outcome upload lost Status heartbeat: first=%+v second=%+v", first, second)
		}
		select {
		case err := <-f.done:
			t.Fatalf("inactivity canceled outcome upload: %v", err)
		default:
		}
		f.releaseOutcome()
		if err := <-f.done; err != nil {
			t.Fatal(err)
		}
		if got := f.parent.result(t); got.err != nil || got.result.Answer != "Complete" {
			t.Fatalf("outcome upload changed successful result: %+v", got)
		}
	})
}
