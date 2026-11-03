package action

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRemoteAttachRejectsRoleSettingsDriftBeforeListening(t *testing.T) {
	cfg := config.Config{Main: "codex", Adversarial: "claude", MainSettings: config.RoleModelConfig{Model: "main-model", Effort: "high"},
		CriticSettings: config.RoleModelConfig{Model: "critic-model", Effort: "low"}, RemoteExecutor: &config.RemoteExecutorConfig{}}
	e := Orchestrator{Main: cfg.Main, Adversarial: cfg.Adversarial, MainModel: "main-model", MainEffort: "high", CriticModel: "wrong", CriticEffort: "low"}
	if _, _, err := e.AttachRemoteWorker(cfg, tls.Certificate{}); err == nil || !strings.Contains(err.Error(), "critic model settings differ") {
		t.Fatalf("remote role drift reached listener: %v", err)
	}
	cfg.Main, e.Main = "claude", "claude"
	cfg.Adversarial, e.Adversarial = "claude", "claude"
	if _, _, err := e.AttachRemoteWorker(cfg, tls.Certificate{}); err == nil || !strings.Contains(err.Error(), nativeRolePairUnverified) {
		t.Fatalf("unverified native pair reached remote listener: %v", err)
	}
}

func TestRemoteWorkerCapacityRequiresLivePull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor, err := NewHTTPExecutor("remote")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := &RemoteEndpoint{serveDone: make(chan struct{})}
		worker := &remoteWorker{executor: executor, endpoint: endpoint}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- worker.WaitCapacity(ctx) }()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("capacity without a pull: %v", err)
		default:
		}

		stale := make(chan struct{})
		close(stale)
		executor.mu.Lock()
		executor.pull, executor.pullDone = true, stale
		executor.notifyLocked()
		executor.mu.Unlock()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("stale pull advertised capacity: %v", err)
		default:
		}

		live := make(chan struct{})
		executor.mu.Lock()
		executor.pullDone = live
		executor.notifyLocked()
		executor.mu.Unlock()
		if err := <-result; err != nil {
			t.Fatalf("live pull did not advertise capacity: %v", err)
		}
		close(live)
		if worker.heldPull() != nil {
			t.Fatal("closed pull remained available for assignment")
		}
		close(endpoint.serveDone)
		if err := worker.WaitCapacity(t.Context()); !errors.Is(err, ErrRemoteWorkerUnavailable) {
			t.Fatalf("stopped endpoint advertised capacity: %v", err)
		}
	})
}

type remoteAssignmentProbe struct {
	runnerExecutionCheckpoints
	undelivered int
	before      func(Job) error
}

func (*remoteAssignmentProbe) AssignmentRequired() bool { return true }
func (p *remoteAssignmentProbe) BeforeAssignment(job Job) error {
	if p.before != nil {
		return p.before(job)
	}
	return nil
}
func (p *remoteAssignmentProbe) AssignmentNotDelivered() error {
	p.undelivered++
	return nil
}

func TestRemoteWorkerRejectsAssignmentAfterPullDisconnect(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	worker := &remoteWorker{executor: executor, endpoint: &RemoteEndpoint{serveDone: make(chan struct{})}}
	stale := make(chan struct{})
	close(stale)
	executor.mu.Lock()
	executor.pull, executor.pullDone = true, stale
	executor.mu.Unlock()
	probe := &remoteAssignmentProbe{}
	result, err := worker.Execute(t.Context(), testJob(command.Comment), probe)
	if !errors.Is(err, monitor.ErrRequestTransport) || probe.undelivered != 1 || result.Version != ProtocolVersion {
		t.Fatalf("disconnected pull reached assignment: result=%+v error=%v undelivered=%d", result, err, probe.undelivered)
	}
}

func TestRemoteWorkerLostPullStopsBeforeAssignmentClaim(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	worker := &remoteWorker{executor: executor, endpoint: &RemoteEndpoint{serveDone: make(chan struct{})}}
	job := testJob(command.Comment)
	job.Identity.ExecutorID = "remote"
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	pullCtx, cancelPull := context.WithCancel(t.Context())
	defer cancelPull()
	request := httptest.NewRequestWithContext(pullCtx, http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	response := &httpRepullRecorder{httptest.NewRecorder()}
	encoding, release, pullFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	go func() {
		executor.serveNextEncoded(response, request, func(assignment httpAssignment) ([]byte, error) {
			close(encoding)
			<-release
			return httpEncode(assignment, httpJobBytes)
		})
		close(pullFinished)
	}()
	readyCtx, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelReady()
	if err := worker.WaitCapacity(readyCtx); err != nil {
		t.Fatal(err)
	}
	probe := &remoteAssignmentProbe{}
	executed := make(chan error, 1)
	go func() { _, err := worker.Execute(t.Context(), job, probe); executed <- err }()
	select {
	case <-encoding:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment encoding did not begin")
	}
	executor.mu.Lock()
	tx := executor.active
	executor.mu.Unlock()
	cancelPull()
	select {
	case err := <-executed:
		if !errors.Is(err, monitor.ErrRequestTransport) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || probe.undelivered != 1 {
			t.Fatalf("lost pull did not refund the unclaimed assignment: error=%v undelivered=%d", err, probe.undelivered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lost pull waited for encoding or action expiry")
	}
	if tx == nil || !tx.fenced || tx.delivered {
		t.Fatalf("lost pull did not fence the unclaimed transaction: transaction=%+v", tx)
	}
	unblock()
	select {
	case <-pullFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled pull handler did not drain")
	}
	if response.Body.Len() != 0 {
		t.Fatal("fenced transaction emitted assignment bytes")
	}
}

func TestRemoteWorkerLostPullDrainsPreDeliverySaveFailure(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	worker := &remoteWorker{executor: executor, endpoint: &RemoteEndpoint{serveDone: make(chan struct{})}}
	job := testJob(command.Comment)
	job.Identity.ExecutorID = "remote"
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	pullCtx, cancelPull := context.WithCancel(t.Context())
	defer cancelPull()
	request := httptest.NewRequestWithContext(pullCtx, http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	response := &httpRepullRecorder{httptest.NewRecorder()}
	pullFinished := make(chan struct{})
	go func() {
		executor.serveNext(response, request)
		close(pullFinished)
	}()
	readyCtx, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelReady()
	if err := worker.WaitCapacity(readyCtx); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	saveErr := errors.New("assignment save failed")
	probe := &remoteAssignmentProbe{before: func(Job) error {
		close(entered)
		<-release
		return saveErr
	}}
	executed := make(chan error, 1)
	go func() { _, err := worker.Execute(t.Context(), job, probe); executed <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pre-delivery save did not begin")
	}
	executor.mu.Lock()
	tx := executor.active
	executor.mu.Unlock()
	cancelPull()
	select {
	case <-tx.stop:
	case <-time.After(5 * time.Second):
		t.Fatal("lost pull did not stop while assignment save was entered")
	}
	select {
	case err := <-executed:
		t.Fatalf("transaction returned before the entered save drained: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-executed:
		if !errors.Is(err, monitor.ErrRequestTransport) || !errors.Is(err, saveErr) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || probe.undelivered != 1 {
			t.Fatalf("lost pull erased save failure or refunded a claimed assignment: error=%v undelivered=%d", err, probe.undelivered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lost pull did not stop after the save drained")
	}
	select {
	case <-pullFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled pull handler did not finish after save failure")
	}
	if response.Body.Len() != 0 {
		t.Fatal("failed pre-delivery save emitted assignment bytes")
	}
}

func TestHTTPExecutorClosedSelectedPullAfterClaimIsUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pullDone := make(chan struct{})
		state := &httpExecutionState{pullDone: pullDone}
		result := make(chan error, 1)
		go func() {
			_, err := executor.execute(ctx, time.Millisecond, job, runnerExecutionCheckpoints{}, state)
			result <- err
		}()
		for {
			executor.mu.Lock()
			tx, changed := executor.active, executor.changed
			ready := false
			if tx != nil {
				tx.mu.Lock()
				ready = tx.ready
				if ready {
					tx.delivered = true // Assignment claimed before its response is known to the worker.
				}
				tx.mu.Unlock()
			}
			executor.mu.Unlock()
			if ready {
				break
			}
			<-changed
		}
		close(pullDone)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("claimed assignment was refunded after pull closure: %v", err)
		default:
		}
		cancel()
		if err := <-result; !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !state.delivered {
			t.Fatalf("claimed assignment lost uncertainty: error=%v delivered=%t", err, state.delivered)
		}
	})
}

func TestRemoteWorkerEndpointClosureBeforeDeliveryIsTransport(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &RemoteEndpoint{executor: executor, server: &http.Server{}, serveDone: make(chan struct{})}
	worker := &remoteWorker{executor: executor, endpoint: endpoint}
	executor.mu.Lock()
	executor.pull, executor.pullDone = true, make(chan struct{})
	executor.mu.Unlock()
	job := testJob(command.Comment)
	job.Identity.ExecutorID = "remote"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &remoteAssignmentProbe{}
	executed := make(chan error, 1)
	go func() { _, err := worker.Execute(ctx, job, probe); executed <- err }()
	for {
		executor.mu.Lock()
		tx, changed := executor.active, executor.changed
		ready := false
		if tx != nil {
			tx.mu.Lock()
			ready = tx.ready
			tx.mu.Unlock()
		}
		executor.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatal("remote transaction did not become ready")
		}
	}
	executor.mu.Lock()
	executor.closed = true
	executor.mu.Unlock()
	close(endpoint.serveDone)
	cancel()
	select {
	case err := <-executed:
		if !errors.Is(err, monitor.ErrRequestTransport) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || probe.undelivered != 1 {
			t.Fatalf("undelivered endpoint closure was not a transport failure: error=%v undelivered=%d", err, probe.undelivered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint closure did not return")
	}
}

func TestAttachRemoteWorkerSelectsConfiguredIdentityWithoutHostSkills(t *testing.T) {
	certificate, _ := testRemoteCertificate(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	const missingSkill = "https://github.com/example/skills/tree/main/skills/not-installed"
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{missingSkill}}}
	cfg := config.Config{Main: "codex", Commands: commands,
		Repositories:   []config.Repository{{Name: "owner/repo", Actions: []string{"inspect"}}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:0", Token: token}}
	orchestrator := &Orchestrator{Main: "codex", Commands: commands}
	selected, endpoint, err := orchestrator.AttachRemoteWorker(cfg, certificate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	if !selected["inspect"] || len(selected) != 1 || orchestrator.ExecutorID != "remote" || !orchestrator.assignmentRequired || !orchestrator.remoteSelected {
		t.Fatalf("wrong remote selection: commands=%v executor=%q", selected, orchestrator.ExecutorID)
	}
	job := monitor.Job{Repository: cfg.Repositories[0], Command: command.Command{Name: "inspect", Action: command.Comment}}
	if err := orchestrator.validateCommand(job); err != nil {
		t.Fatalf("orchestrator required worker-local skill: %v", err)
	}
	job.Command.Name = "other"
	if err := orchestrator.validateCommand(job); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("remote mode skipped command validation: %v", err)
	}
}

func TestRemoteWorkerRunsOneAuthenticatedAssignment(t *testing.T) {
	certificate, leaf := testRemoteCertificate(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenRemoteExecution(executor, "127.0.0.1:0", certificate, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	origin := "https://" + endpoint.Addr().String()
	client, err := NewRemoteExecutionClient(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client.Transport.(*authenticatedExecutionTransport).TLSClientConfig.RootCAs = roots
	job := testJob(command.Comment)
	job.Identity.ExecutorID = "remote"
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		return aicli.Result{Answer: "remote answer", UsageComplete: true}, nil
	}}
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunHTTP(t.Context(), client, origin, "remote", runner) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	remote := &remoteWorker{executor: executor, endpoint: endpoint}
	if err := remote.WaitCapacity(ctx); err != nil {
		t.Fatalf("authenticated remote pull was not ready: %v", err)
	}
	result, err := remote.Execute(ctx, job, runnerExecutionCheckpoints{})
	if err != nil || result.Answer != "remote answer" || !result.matchesJob(job) {
		t.Fatalf("remote assignment failed: result=%+v error=%v", result, err)
	}
	if err := <-workerDone; err != nil {
		t.Fatalf("remote worker failed after outcome: %v", err)
	}
}

func TestRemoteWorkerCleanupFailureClosesEndpoint(t *testing.T) {
	certificate, leaf := testRemoteCertificate(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenRemoteExecution(executor, "127.0.0.1:0", certificate, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	origin := "https://" + endpoint.Addr().String()
	client, err := NewRemoteExecutionClient(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client.Transport.(*authenticatedExecutionTransport).TLSClientConfig.RootCAs = roots
	base, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	transport := &httpExecutionClient{client: client, base: base}
	job := testJob(command.Comment)
	job.Identity.ExecutorID = "remote"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	type pullResult struct {
		assignment httpAssignment
		err        error
	}
	pulled := make(chan pullResult, 1)
	go func() {
		var assignment httpAssignment
		err := transport.post(ctx, "next", pull, httpJobBytes, &assignment, httpBinding{}, 0)
		pulled <- pullResult{assignment, err}
	}()
	worker := &remoteWorker{executor: executor, endpoint: endpoint}
	if err := worker.WaitCapacity(ctx); err != nil {
		t.Fatalf("authenticated held pull was unavailable: %v", err)
	}
	type executionResult struct {
		result Result
		err    error
	}
	executed := make(chan executionResult, 1)
	go func() {
		result, err := worker.Execute(ctx, job, runnerExecutionCheckpoints{})
		executed <- executionResult{result, err}
	}()
	if got := <-pulled; got.err != nil || !sameAssignmentJob(job, got.assignment.Job) {
		t.Fatalf("remote assignment was not delivered: %+v", got)
	}
	outcome := httpOutcome{Sequence: 1, Result: Result{Version: ProtocolVersion, Identity: job.Identity,
		SourceRef: job.Source.Head, Diagnostics: []Diagnostic{{Code: "cleanup_failed", Message: "cleanup failed"}}}}
	body, err := httpEncode(outcome, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	var ack httpAcknowledgement
	if err := transport.post(ctx, "outcome", body, httpControlBytes, &ack, httpJobBinding(job), 1); err != nil || ack.Kind != "outcome" || ack.Failure != nil {
		t.Fatalf("cleanup outcome was not acknowledged: ack=%+v error=%v", ack, err)
	}
	var got executionResult
	select {
	case got = <-executed:
	case <-ctx.Done():
		t.Fatalf("remote cleanup-failure execution did not finish: %v", ctx.Err())
	}
	if got.err != nil || len(got.result.Diagnostics) != 1 || got.result.Diagnostics[0].Code != "cleanup_failed" {
		t.Fatalf("cleanup failure was not retained: %+v", got)
	}
	if observed := worker.dashboardObservation(); observed.readiness != "cleanup failed" {
		t.Fatalf("remote cleanup failure was not visible: %+v", observed)
	}
	select {
	case <-endpoint.Done():
	default:
		t.Fatal("cleanup failure left the remote listener open")
	}
	if err := worker.WaitCapacity(ctx); !errors.Is(err, ErrRemoteWorkerUnavailable) {
		t.Fatalf("closed endpoint advertised capacity: %v", err)
	}
	for _, request := range []struct {
		path string
		body any
	}{
		{"/next", httpPull{Version: ProtocolVersion, ExecutorID: "remote"}},
		{"/status", httpObservation{Status: Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1}}},
	} {
		body, err := httpEncode(request.body, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		recorder := &httpRepullRecorder{httptest.NewRecorder()}
		httpRequest := httptest.NewRequest(http.MethodPost, request.path, bytes.NewReader(body))
		httpRequest.Header.Set("Content-Type", "application/json")
		executor.ServeHTTP(recorder, httpRequest)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("stale %s was accepted after cleanup: status=%d body=%s", request.path, recorder.Code, recorder.Body.String())
		}
	}
}
