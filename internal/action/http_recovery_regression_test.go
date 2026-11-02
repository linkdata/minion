package action

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

type heldRecoveryPullBody struct {
	data    []byte
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *heldRecoveryPullBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (*heldRecoveryPullBody) Close() error { return nil }

func TestAuthenticatedPullRetiredWhenCleanupCompletesBeforeExecutorDispatch(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := &httpRepullRecorder{httptest.NewRecorder()}
	authenticated, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	handler := authenticatedExecutionHandlerWithServe(executor, token, func(w http.ResponseWriter, r *http.Request) {
		close(authenticated)
		<-release
		executor.ServeHTTP(w, r)
	})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-authenticated:
	case <-time.After(5 * time.Second):
		t.Fatal("authenticated pull did not reach executor dispatch")
	}
	(&RemoteEndpoint{executor: executor}).RequireFreshPull()
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retired authenticated pull did not return")
	}
	executor.mu.Lock()
	held := executor.pull
	executor.mu.Unlock()
	if response.Code != http.StatusTooEarly || held {
		t.Fatalf("precleanup authenticated pull became ready: status=%d held=%t", response.Code, held)
	}
}

func TestHTTPExecutorRejectsPrecleanupInflightPull(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &RemoteEndpoint{executor: executor, serveDone: make(chan struct{})}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &heldRecoveryPullBody{data: pull, entered: entered, release: release}
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/next", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := &httpRepullRecorder{httptest.NewRecorder()}
	go func() {
		authenticatedExecutionHandler(executor, token).ServeHTTP(response, request)
		close(done)
	}()
	guard, stopGuard := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopGuard()
	select {
	case <-entered:
	case <-guard.Done():
		t.Fatalf("authenticated pull did not begin reading: %v", guard.Err())
	}
	endpoint.RequireFreshPull()
	executor.mu.Lock()
	changed := executor.changed
	executor.mu.Unlock()
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-changed:
		executor.mu.Lock()
		held := executor.pull
		executor.mu.Unlock()
		if held {
			cancel()
			<-done
			t.Fatal("pull started before cleanup advertised readiness after cleanup")
		}
		select {
		case <-done:
		case <-guard.Done():
			t.Fatalf("retired in-flight pull did not return: %v", guard.Err())
		}
	case <-guard.Done():
		t.Fatalf("retired in-flight pull stalled: %v", guard.Err())
	}
	executor.mu.Lock()
	held := executor.pull
	executor.mu.Unlock()
	if held || response.Code != http.StatusTooEarly {
		t.Fatalf("precleanup pull became ready or lacked retirement ACK: held=%t status=%d body=%s", held, response.Code, response.Body.String())
	}
}

func TestHTTPExecutorRetiresPullAfterCleanupDuringEncoding(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	response := &httpRepullRecorder{httptest.NewRecorder()}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	go func() {
		f.executor.serveNextEncoded(response, request, func(assignment httpAssignment) ([]byte, error) {
			close(entered)
			<-release
			return httpEncode(assignment, httpJobBytes)
		})
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment encoding did not start")
	}
	cancel()
	if got := f.result(t); !errors.Is(got.err, context.Canceled) || f.tx.delivered {
		t.Fatalf("undelivered assignment did not stop: %+v delivered=%t", got, f.tx.delivered)
	}
	(&RemoteEndpoint{executor: f.executor}).RequireFreshPull()
	unblock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("retired pull handler did not finish")
	}
	f.executor.mu.Lock()
	held := f.executor.pull
	f.executor.mu.Unlock()
	if response.Code != http.StatusTooEarly || held {
		t.Fatalf("precleanup encoded pull was not retired: status=%d held=%t", response.Code, held)
	}
}

func TestFreshAuthenticatedPullWaitsForRetiringSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	endpoint := &RemoteEndpoint{executor: f.executor, serveDone: make(chan struct{})}
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
	oldRequest.Header.Set("Content-Type", "application/json")
	oldResponse := &httpRepullRecorder{httptest.NewRecorder()}
	encoding, release, oldDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	go func() {
		f.executor.serveNextEncoded(oldResponse, oldRequest, func(assignment httpAssignment) ([]byte, error) {
			close(encoding)
			<-release
			return httpEncode(assignment, httpJobBytes)
		})
		close(oldDone)
	}()
	select {
	case <-encoding:
	case <-time.After(5 * time.Second):
		t.Fatal("old pull did not reach encoding")
	}
	cancel()
	if got := f.result(t); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("undelivered transaction did not finish: %+v", got)
	}
	endpoint.RequireFreshPull()
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	freshCtx, stopFresh := context.WithCancel(t.Context())
	defer stopFresh()
	freshRequest := httptest.NewRequestWithContext(freshCtx, http.MethodPost, "/next", bytes.NewReader(pull))
	freshRequest.Header.Set("Content-Type", "application/json")
	freshRequest.Header.Set("Authorization", "Bearer "+token)
	freshResponse := &httpRepullRecorder{httptest.NewRecorder()}
	freshEntered, freshDone := make(chan struct{}), make(chan struct{})
	authenticated := authenticatedExecutionHandlerWithServe(f.executor, token, func(w http.ResponseWriter, r *http.Request) {
		close(freshEntered)
		f.executor.ServeHTTP(w, r)
	})
	go func() {
		authenticated.ServeHTTP(freshResponse, freshRequest)
		close(freshDone)
	}()
	select {
	case <-freshEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh pull did not authenticate")
	}
	beforeRelease, stopBeforeRelease := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stopBeforeRelease()
	if err := (&remoteWorker{executor: f.executor, endpoint: endpoint}).WaitCapacity(beforeRelease); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retiring slot advertised capacity: %v", err)
	}
	select {
	case <-freshDone:
		t.Fatalf("fresh pull was rejected before old slot retired: status=%d", freshResponse.Code)
	default:
	}
	unblock()
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		t.Fatal("retired old pull did not release its slot")
	}
	if oldResponse.Code != http.StatusTooEarly {
		t.Fatalf("old pull retirement status = %d", oldResponse.Code)
	}
	readyCtx, stopReady := context.WithTimeout(t.Context(), time.Second)
	defer stopReady()
	if err := (&remoteWorker{executor: f.executor, endpoint: endpoint}).WaitCapacity(readyCtx); err != nil {
		t.Fatalf("fresh pull did not restore capacity after old slot retired: %v", err)
	}
	stopFresh()
	select {
	case <-freshDone:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh held pull did not stop")
	}
}

func TestRunHTTPLoopRepullsAfterDiscardedPreDeliveryAssignment(t *testing.T) {
	secondPull := make(chan struct{})
	var pulls atomic.Int32
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/next" && pulls.Add(1) == 2 {
				close(secondPull)
			}
			next.ServeHTTP(w, r)
		})
	})
	endpoint := &RemoteEndpoint{executor: f.executor, serveDone: make(chan struct{})}
	f.tx.mu.Lock()
	f.tx.beforeDelivery = func(Job) error {
		endpoint.RequireFreshPull()
		return monitor.ErrAcceptedDiscarded
	}
	f.tx.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunHTTPLoop(ctx, f.server.Client(), f.server.URL, f.job.Identity.ExecutorID,
			&Runner{runModel: forbidModel(t)})
	}()
	select {
	case <-secondPull:
	case err := <-workerDone:
		t.Fatalf("discarded assignment stopped the worker instead of repulling: %v", err)
	case <-ctx.Done():
		t.Fatalf("discarded assignment did not produce a fresh pull: %v", ctx.Err())
	}
	if got := f.result(t); !errors.Is(got.err, monitor.ErrAcceptedDiscarded) || f.tx.delivered {
		t.Fatalf("discarded assignment changed parent result: %+v delivered=%t", got, f.tx.delivered)
	}
	readyCtx, stopReady := context.WithTimeout(ctx, time.Second)
	defer stopReady()
	if err := (&remoteWorker{executor: f.executor, endpoint: endpoint}).WaitCapacity(readyCtx); err != nil {
		t.Fatalf("fresh pull did not restore readiness after discard: %v", err)
	}
	if err := f.executor.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("worker did not exit on acknowledged idle shutdown: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not finish after endpoint shutdown: %v", ctx.Err())
	}
}
