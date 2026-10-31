package action

import (
	"context"
	"errors"
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
	if !errors.As(got.err, &reported) || !reported.failure.Terminal || reported.failure.Deadline || len(got.result.Calls) != 1 {
		t.Fatalf("parent lost terminal inactivity outcome: error=%v result=%+v", got.err, got.result)
	}
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
		if !errors.As(got.err, &reported) || !reported.failure.Deadline || len(got.result.Calls) != 1 {
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
