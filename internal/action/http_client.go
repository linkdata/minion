package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

var errHTTPWatchStopped = errors.New("HTTP cancellation watch stopped after worker return")
var errHTTPWorkerCleanup = errors.New("HTTP worker cleanup failed; worker is unavailable")
var errExecutorRuntimeCap = fmt.Errorf("executor runtime cap: %w", context.DeadlineExceeded)

type httpExecutionClient struct {
	client              *http.Client
	base                *url.URL
	outcomeSent         atomic.Bool
	outcomeAcknowledged bool
	outcomeDegraded     bool
}

type httpRunStatus uint8

const (
	httpRunFailed httpRunStatus = iota
	httpRunIdleShutdown
	httpRunAcknowledged
)

type httpPullBody struct {
	io.ReadCloser
	timer  *time.Timer
	done   <-chan struct{}
	cancel context.CancelCauseFunc
}

func (b *httpPullBody) Close() error {
	if !b.timer.Stop() {
		<-b.done
	}
	b.cancel(context.Canceled)
	return b.ReadCloser.Close()
}

type httpClientCheckpoints struct {
	transport     *httpExecutionClient
	control       context.Context
	action        context.Context
	inactivity    *inactivityTimer
	hardDeadline  time.Time
	ownRuntimeCap bool
	binding       httpBinding
	sequence      uint64
	failure       error
}

// RunHTTP pulls and executes one assignment. Returning does not prove process release.
func RunHTTP(ctx context.Context, client *http.Client, baseURL, executorID string, runner *Runner) error {
	return runHTTP(ctx, client, baseURL, executorID, runner, nil)
}

// RunHTTPLoop executes distinct assignments until the parent closes an idle pull.
func RunHTTPLoop(ctx context.Context, client *http.Client, baseURL, executorID string, runner *Runner) error {
	for {
		var status httpRunStatus
		err := runHTTP(ctx, client, baseURL, executorID, runner, &status)
		switch status {
		case httpRunIdleShutdown:
			return nil
		case httpRunAcknowledged:
			continue
		default:
			if err == nil {
				return errors.New("HTTP worker ended without an acknowledged outcome")
			}
			return err
		}
	}
}

func runHTTP(ctx context.Context, client *http.Client, baseURL, executorID string, runner *Runner, status *httpRunStatus) error {
	if client == nil || runner == nil || !workspace.ValidIdentitySegment(executorID) {
		return errors.New("HTTP worker requires a client, runner and valid executor identity")
	}
	if runner.Codex.APIKey != "" || runner.Codex.NativeAPIKey != "" || runner.Codex.NativeAccessToken != "" || runner.Claude.APIKey != "" {
		return errors.New("HTTP worker runner must not retain provider credentials")
	}
	base, err := url.Parse(baseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.RawFragment != "" || base.Opaque != "" {
		return errors.New("invalid HTTP execution base URL")
	}
	if err := ctx.Err(); err != nil {
		if status != nil && errors.Is(err, context.Canceled) {
			*status = httpRunIdleShutdown
		}
		return err
	}
	configured := *client
	configured.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := &httpExecutionClient{client: &configured, base: base}
	var checkpoints *httpClientCheckpoints
	protocolFailed := false
	cleanupFailed := false
	defer func() {
		if status != nil && transport.outcomeAcknowledged && !transport.outcomeDegraded && !protocolFailed && !cleanupFailed && checkpoints != nil && checkpoints.failure == nil {
			*status = httpRunAcknowledged
		}
	}()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: executorID}, httpControlBytes)
	if err != nil {
		return err
	}
	var assignment httpAssignment
	pullStarted := time.Now()
	if err := transport.post(ctx, "next", pull, httpJobBytes, &assignment, httpBinding{}, 0); err != nil {
		if status != nil && errors.Is(err, context.Canceled) && !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
			*status = httpRunIdleShutdown
		}
		return err
	}
	started := time.Now()
	job := assignment.Job
	if err := job.Validate(); err != nil {
		return httpClientUnknown("next", err)
	}
	if job.Identity.ExecutorID != executorID {
		return httpClientUnknown("next", errors.New("assignment executor differs from pull"))
	}
	if err := assignment.Credentials.validateJob(job); err != nil {
		return httpClientUnknown("next", err)
	}
	if assignment.StopGrace <= 0 || assignment.StopGrace > config.MaxActionRuntime {
		return httpClientUnknown("next", errors.New("invalid assignment stop grace"))
	}
	jobRunner := *runner
	jobRunner.Codex.APIKey = assignment.Credentials.CodexAPIKey
	jobRunner.Codex.NativeAPIKey = assignment.Credentials.CodexNativeAPIKey
	jobRunner.Codex.NativeAccessToken = assignment.Credentials.CodexAccessToken
	jobRunner.Claude.APIKey = assignment.Credentials.ClaudeAPIKey
	assignment.Credentials = WorkerCredentials{}
	assignedSecrets := jobRunner.secretValues()
	clearCredentials := func() {
		jobRunner.Codex.APIKey = ""
		jobRunner.Codex.NativeAPIKey = ""
		jobRunner.Codex.NativeAccessToken = ""
		jobRunner.Claude.APIKey = ""
	}
	defer clearCredentials()
	// Subtract transit and decoding time, but not the parent's idle long-poll.
	elapsed := started.Sub(pullStarted)
	if assignment.HeldFor < 0 || assignment.HeldFor > elapsed && assignment.HeldFor-elapsed > config.MaxActionRuntime {
		return httpClientUnknown("next", errors.New("invalid assignment pull wait"))
	}
	transit := elapsed - min(elapsed, assignment.HeldFor)
	workerExpires := started.Add(job.RuntimeBudget - transit)
	expires := workerExpires
	ownRuntimeCap := true
	if deadline, ok := ctx.Deadline(); ok && !workerExpires.Before(deadline) {
		expires = deadline
		ownRuntimeCap = false
	}
	var deadlineCause error = context.DeadlineExceeded
	if ownRuntimeCap {
		deadlineCause = errExecutorRuntimeCap
	}
	controlCtx, cancelControl := context.WithDeadline(context.WithoutCancel(ctx), expires.Add(assignment.StopGrace))
	defer cancelControl()
	shortenControl := func(remaining time.Duration) {
		if remaining <= 0 {
			cancelControl()
			return
		}
		timer := time.AfterFunc(remaining, cancelControl)
		context.AfterFunc(controlCtx, func() { timer.Stop() })
	}
	checkpoints = &httpClientCheckpoints{transport: transport, control: controlCtx, binding: httpJobBinding(job)}
	if !time.Now().Before(expires) {
		err := errors.Join(monitor.ErrTerminalJob, deadlineCause)
		if limit := selectWorkerLimit(nil, time.Now(), expires, started.Add(job.InactivityTimeout), ownRuntimeCap); limit != workerLimitNone {
			err = limit.err()
		}
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		clearCredentials()
		return redactActionError(errors.Join(err, checkpoints.outcome(result, err)), assignedSecrets...)
	}
	deadlineCtx, stopDeadline := context.WithDeadlineCause(ctx, expires, deadlineCause)
	defer stopDeadline()
	actionCtx, cancelAction := context.WithCancelCause(deadlineCtx)
	defer cancelAction(context.Canceled)
	inactivity := startInactivityTimer(started, job.InactivityTimeout, cancelAction)
	defer inactivity.stop()
	checkpoints.inactivity, checkpoints.hardDeadline, checkpoints.ownRuntimeCap = inactivity, expires, ownRuntimeCap
	startStop := sync.OnceFunc(func() { shortenControl(assignment.StopGrace) })
	stopOnAction := context.AfterFunc(actionCtx, startStop)
	defer stopOnAction()
	checkpoints.action = actionCtx
	if err := context.Cause(actionCtx); err != nil {
		startStop()
		if limit := selectWorkerLimit(err, time.Now(), expires, inactivity.deadline(), ownRuntimeCap); limit != workerLimitNone {
			err = limit.err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.Join(monitor.ErrTerminalJob, err)
		}
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		clearCredentials()
		return redactActionError(errors.Join(err, checkpoints.outcome(result, err)), assignedSecrets...)
	}
	jobRunner.executionProgress = inactivity.progress
	jobRunner.postCallContext = controlCtx
	watchCtx, stopWatch := context.WithCancelCause(ctx)
	watchBody, err := httpEncode(checkpoints.binding, httpControlBytes)
	if err != nil {
		protocolFailed = true
		stopWatch(errHTTPWatchStopped)
		return fmt.Errorf("encode cancellation admission: %w", err)
	}
	// Bound admission only; the held body must survive model expiry for paid receipts.
	headerDeadline, inactivityFirst := watchAdmissionDeadline(expires, started.Add(job.InactivityTimeout), time.Now().Add(httpControlTimeout))
	headerErr := fmt.Errorf("cancellation watch admission timed out: %w", context.DeadlineExceeded)
	if inactivityFirst {
		headerErr = fmt.Errorf("cancellation watch admission timed out: %w", errExecutorInactivity)
	} else if ownRuntimeCap && headerDeadline.Equal(expires) {
		headerErr = fmt.Errorf("cancellation watch admission timed out: %w", errExecutorRuntimeCap)
	}
	headerWait := max(0, time.Until(headerDeadline))
	admission := &watchAdmission{}
	headerTimer := time.AfterFunc(headerWait, func() {
		admission.expire(stopWatch, headerErr)
	})
	watchResponse, err := transport.openObserved(watchCtx, "cancel", watchBody, checkpoints.binding, 0, func() {
		admission.observe(actionCtx, watchCtx)
	})
	headerTimer.Stop()
	admissionObservedAt, admissionCause, watchAdmissionTimedOut := admission.finish(headerDeadline, stopWatch, headerErr)
	if watchAdmissionTimedOut {
		if watchResponse != nil {
			_ = watchResponse.Body.Close()
		}
		err = headerErr
		if inactivityFirst || headerDeadline.Equal(expires) {
			err = errors.Join(monitor.ErrTerminalJob, err)
		} else {
			err = errors.Join(monitor.ErrRequestTransport, err)
		}
	}
	if err != nil {
		protocolFailed = !watchAdmissionTimedOut || !errors.Is(err, monitor.ErrTerminalJob)
		stopWatch(errHTTPWatchStopped)
		startStop()
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		if watchAdmissionTimedOut {
			inactivity.stop() // The selected watch deadline wins later timer callbacks.
		} else {
			err = admissionFailure(err, admissionCause, inactivity, admissionObservedAt, expires, ownRuntimeCap)
		}
		err = redactActionError(err, assignedSecrets...)
		clearCredentials()
		return redactActionError(errors.Join(err, checkpoints.outcomeWithCause(result, err, err)), assignedSecrets...)
	}
	watchDone := make(chan error, 1)
	go func() {
		err := transport.watch(watchCtx, watchResponse, checkpoints.binding, shortenControl)
		// Only cancellation of the held request is an intentional stop. Do not
		// erase a parsed parent reply or protocol failure if stopping races it.
		if watchCtx.Err() != nil && err == watchCtx.Err() {
			err = context.Cause(watchCtx)
			if errors.Is(err, errHTTPWatchStopped) {
				watchDone <- nil
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				err = errors.Join(monitor.ErrTerminalJob, err)
			}
		}
		if err != nil {
			cancelAction(err)
		}
		watchDone <- err
	}()
	statusFeed := newWorkerStatus(job, started)
	statusCtx, cancelStatus := context.WithCancel(controlCtx)
	defer cancelStatus()
	eventCtx, cancelEvents := context.WithCancel(controlCtx)
	defer cancelEvents()
	jobRunner.phaseUpdate = statusFeed.phase
	jobRunner.statusActivity = statusFeed.activity
	events := newProviderEventQueue(func(event attemptRecord) error {
		return statusFeed.emit(eventCtx, transport, event, assignedSecrets...)
	})
	jobRunner.eventSink = events.add
	stopStatusOnAction := context.AfterFunc(actionCtx, statusFeed.stopping)
	defer stopStatusOnAction()
	go statusFeed.run(statusCtx, transport)
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
	var runErr error
	// Preparation consumes the accepted runtime.
	job.RuntimeBudget = time.Until(expires)
	inactivity.expire()
	if limit := selectWorkerLimit(context.Cause(actionCtx), time.Now(), expires, inactivity.deadline(), ownRuntimeCap); limit != workerLimitNone {
		runErr = limit.err()
	} else if actionCtx.Err() != nil {
		runErr = context.Cause(actionCtx)
	} else if job.RuntimeBudget <= 0 {
		runErr = errors.Join(monitor.ErrTerminalJob, deadlineCause)
	} else {
		result, runErr = jobRunner.Execute(actionCtx, job, checkpoints)
	}
	eventsDone := make(chan bool, 1)
	go func() { eventsDone <- events.finish() }()
	// A normal Runner return can still have a large Result to upload. Keep its
	// accepted runtime and transfer bound; only a stopped action or failed
	// required control exchange starts the short stop budget.
	if actionCtx.Err() != nil || checkpoints.failure != nil {
		startStop()
	}
	clearCredentials()
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "cleanup_failed" {
			cleanupFailed = true
			break
		}
	}
	if checkpoints.failure == nil && (job.Critic == nil && len(result.Calls) != 0 || job.Critic != nil && result.CriticStopReason != "") {
		jobRunner.phaseUpdate("upload", 0, "")
	}
	outcomeErr := statusFeed.finish(actionCtx, transport, func() error {
		inactivity.stop() // Result upload begins; Status drain and upload are not progress.
		stopCause := context.Cause(actionCtx)
		if runErr == nil && httpWatchUncertainty(stopCause) {
			// A completed result can resolve independent watch uncertainty through its receipt.
			stopCause = nil
		}
		if limit := selectWorkerLimit(stopCause, time.Now(), expires, inactivity.deadline(), ownRuntimeCap); limit != workerLimitNone {
			// Use deadline order even if a timer callback has not run yet.
			stopCause = limit.err()
		}
		// A watch can cancel the child first; retain independent caller and budget failures.
		cause := errors.Join(stopCause, context.Cause(ctx), httpExecutionContextError(deadlineCtx))
		if errors.Is(cause, context.DeadlineExceeded) {
			cause = errors.Join(cause, monitor.ErrTerminalJob)
		}
		runErr = redactActionError(errors.Join(runErr, cause), assignedSecrets...)
		err := checkpoints.outcomeWithCause(result, runErr, stopCause)
		cancelStatus() // Keep heartbeats available throughout the final Result upload.
		cancelEvents() // Optional logs cannot hold the worker after the required outcome.
		return err
	}, func() {
		if !<-eventsDone {
			statusFeed.markIncomplete()
			jobRunner.logf("provider event capture incomplete job=%s attempt=%s", job.Identity.JobID, job.Identity.AttemptID)
		}
	})
	stopWatch(errHTTPWatchStopped)
	watchErr := <-watchDone
	if watchErr != nil {
		var network *url.Error
		protocolFailed = errors.Is(watchErr, monitor.ErrExecutionOutcomeUnknown) || errors.Is(watchErr, monitor.ErrRequestTransport) || errors.As(watchErr, &network)
	}
	if status == nil && outcomeErr == nil && context.Cause(watchCtx) == errHTTPWatchStopped && httpWatchUncertainty(watchErr) {
		// Parent receipt is known. A watch-only protocol failure cannot revoke it;
		// errors already captured in runErr still propagate.
		runner.logf("HTTP cancellation watch: %q", httpFailureDiagnostic(redactActionError(watchErr, assignedSecrets...).Error()))
		watchErr = nil
	}
	callerErr := context.Cause(ctx)
	if errors.Is(callerErr, context.DeadlineExceeded) {
		callerErr = errors.Join(callerErr, monitor.ErrTerminalJob)
	}
	if cleanupFailed {
		runErr = errors.Join(runErr, errHTTPWorkerCleanup)
	}
	return redactActionError(errors.Join(runErr, outcomeErr, watchErr, callerErr), assignedSecrets...)
}

func watchAdmissionDeadline(hard, idle, control time.Time) (time.Time, bool) {
	deadline := hard
	if control.Before(deadline) {
		deadline = control
	}
	if idle.Before(deadline) {
		return idle, true
	}
	return deadline, false
}

// watchAdmission settles the first observed watch result against the header timer.
// The timer cannot cancel a watch whose headers or error were already observed.
type watchAdmission struct {
	mu         sync.Mutex
	observedAt time.Time
	cause      error
	watchCause error
}

func (a *watchAdmission) observe(action, watch context.Context) {
	a.mu.Lock()
	a.cause = context.Cause(action)
	a.watchCause = context.Cause(watch)
	a.observedAt = time.Now()
	a.mu.Unlock()
}

func (a *watchAdmission) expire(cancel context.CancelCauseFunc, err error) {
	a.mu.Lock()
	expired := a.observedAt.IsZero()
	a.mu.Unlock()
	if expired {
		cancel(err)
	}
}

func (a *watchAdmission) finish(deadline time.Time, cancel context.CancelCauseFunc, err error) (time.Time, error, bool) {
	a.mu.Lock()
	observedAt, cause, watchCause := a.observedAt, a.cause, a.watchCause
	a.mu.Unlock()
	// A non-admission watch stop already selected by context keeps its cause.
	timedOut := (watchCause == nil || watchCause == err) && !observedAt.Before(deadline)
	if timedOut {
		cancel(err) // The absolute deadline wins even if its callback was delayed.
	}
	return observedAt, cause, timedOut
}

type workerLimit uint8

const (
	workerLimitNone workerLimit = iota
	workerLimitInactivity
	workerLimitRuntimeCap
	workerLimitCallerDeadline
)

func (limit workerLimit) err() error {
	switch limit {
	case workerLimitInactivity:
		return errors.Join(monitor.ErrTerminalJob, errExecutorInactivity)
	case workerLimitRuntimeCap:
		return errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap)
	case workerLimitCallerDeadline:
		return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
	default:
		return nil
	}
}

func selectWorkerLimit(cause error, at, hard, idle time.Time, ownRuntimeCap bool) workerLimit {
	if cause != nil && !errors.Is(cause, errExecutorInactivity) && !errors.Is(cause, errExecutorRuntimeCap) {
		if ownRuntimeCap || !errors.Is(cause, context.DeadlineExceeded) ||
			errors.Is(cause, monitor.ErrRequestTransport) || errors.Is(cause, monitor.ErrExecutionOutcomeUnknown) {
			return workerLimitNone // An earlier parent or transport stop keeps its cause.
		}
	}
	if !idle.IsZero() && !at.Before(idle) && idle.Before(hard) {
		return workerLimitInactivity
	}
	if !at.Before(hard) {
		if ownRuntimeCap {
			return workerLimitRuntimeCap // The hard cap wins a tie.
		}
		return workerLimitCallerDeadline // The caller deadline wins a tie.
	}
	return workerLimitNone
}

func admissionFailure(err, cause error, inactivity *inactivityTimer, observedAt, hard time.Time, ownRuntimeCap bool) error {
	// Select at the moment the transport failure was observed. Stopping the
	// timer may invoke a later inactivity callback, which cannot replace it.
	limit := selectWorkerLimit(cause, observedAt, hard, inactivity.deadline(), ownRuntimeCap)
	inactivity.stop()
	if limit != workerLimitNone {
		cause = limit.err()
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		cause = errors.Join(monitor.ErrTerminalJob, cause)
	}
	return errors.Join(err, cause)
}

func httpClientUnknown(kind string, err error) error {
	return fmt.Errorf("%w: HTTP %s: %w", monitor.ErrExecutionOutcomeUnknown, kind, err)
}

func (c *httpExecutionClient) post(ctx context.Context, kind string, data []byte, limit int, result any, binding httpBinding, sequence uint64) error {
	response, err := c.open(ctx, kind, data, binding, sequence)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := httpDecode(response.Body, limit, result); err != nil {
		return httpClientUnknown(kind, err)
	}
	return nil
}

// open returns only after success headers. The held cancellation body is read
// separately, so a registered watch is an admission barrier before Runner starts.
func (c *httpExecutionClient) open(ctx context.Context, kind string, data []byte, binding httpBinding, sequence uint64) (*http.Response, error) {
	return c.openObserved(ctx, kind, data, binding, sequence, nil)
}

func (c *httpExecutionClient) openObserved(ctx context.Context, kind string, data []byte, binding httpBinding, sequence uint64, observed func()) (*http.Response, error) {
	if observed != nil {
		defer observed() // Record completion before the caller can be descheduled.
	}
	var pullCancel context.CancelCauseFunc
	if kind == "next" {
		ctx, pullCancel = context.WithCancelCause(ctx)
	}
	defer func() {
		if pullCancel != nil {
			pullCancel(context.Canceled)
		}
	}()
	failure := func(err error) error {
		if kind == "cancel" {
			return fmt.Errorf("HTTP %s admission: %w", kind, err)
		}
		return httpClientUnknown(kind, err)
	}
	endpoint := *c.base
	endpoint.Path, endpoint.RawPath = strings.TrimRight(endpoint.Path, "/")+"/"+kind, ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return nil, failure(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	if kind == "outcome" {
		c.outcomeSent.Store(true)
	}
	response, err := c.client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if kind == "cancel" && ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))) {
			return nil, ctx.Err()
		}
		return nil, failure(err)
	}
	if pullCancel != nil {
		// The assignment wait is unbounded; size bounds only its received body.
		cancel, done := pullCancel, make(chan struct{})
		size := response.ContentLength
		if size < 0 {
			size = httpJobBytes
		}
		if response.StatusCode != http.StatusOK {
			size = 0 // A rejection carries only a control acknowledgement.
		}
		timer := time.AfterFunc(httpTransferTimeout(min(size, httpJobBytes)), func() {
			cancel(context.DeadlineExceeded)
			close(done)
		})
		response.Body = &httpPullBody{ReadCloser: response.Body, timer: timer, done: done, cancel: cancel}
		pullCancel = nil // Response-body cleanup now owns cancellation and the timer.
	}
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	encodings := response.Header.Values("Content-Encoding")
	invalidEncoding := len(encodings) > 1 || len(encodings) == 1 && encodings[0] != "" && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity")
	if mediaErr != nil || media != "application/json" || response.Uncompressed || invalidEncoding {
		_ = response.Body.Close()
		return nil, failure(errors.New("response must be uncompressed JSON"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		var ack httpAcknowledgement
		if err := httpDecode(response.Body, httpControlBytes, &ack); err != nil {
			return nil, failure(err)
		}
		if ack.httpBinding != binding || ack.Sequence != sequence || ack.Kind != kind || ack.Failure == nil {
			return nil, failure(errors.New("invalid failure acknowledgement"))
		}
		if err := ack.Failure.validate(); err != nil {
			return nil, failure(err)
		}
		return nil, ack.Failure.err()
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, failure(errors.New("positive response requires HTTP 200"))
	}
	return response, nil
}

func (c *httpExecutionClient) watch(ctx context.Context, response *http.Response, binding httpBinding, shortenControl func(time.Duration)) error {
	defer response.Body.Close()
	cancellation := httpCancellation{StopRemaining: -1}
	if err := httpDecode(response.Body, httpControlBytes, &cancellation); err != nil {
		intentionalPipe := context.Cause(ctx) == errHTTPWatchStopped && errors.Is(err, io.ErrClosedPipe)
		if ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx)) || intentionalPipe) {
			return ctx.Err()
		}
		return httpClientUnknown("cancel", err)
	}
	if cancellation.httpBinding != binding {
		return httpClientUnknown("cancel", errors.New("cancellation binding differs from assignment"))
	}
	switch cancellation.Reason {
	case "canceled", "deadline":
		if cancellation.StopRemaining < 0 || cancellation.StopRemaining > config.MaxActionRuntime {
			return httpClientUnknown("cancel", errors.New("invalid cancellation stop remaining"))
		}
		shortenControl(cancellation.StopRemaining)
		if cancellation.Reason == "canceled" {
			return fmt.Errorf("parent action canceled: %w", context.Canceled)
		}
		return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
	case "completed":
		if c.outcomeSent.Load() {
			return nil
		}
		return httpClientUnknown("cancel", errors.New("transaction completed before this worker returned an outcome"))
	default:
		return httpClientUnknown("cancel", errors.New("invalid cancellation reason"))
	}
}

func (c *httpClientCheckpoints) acknowledge(parent context.Context, kind string, data []byte, sequence uint64) error {
	ctx, cancel := context.WithTimeout(parent, httpTransferTimeout(int64(len(data)))+httpControlTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return httpClientUnknown(kind, err)
	}
	c.sequence = sequence
	var ack httpAcknowledgement
	if err := c.transport.post(ctx, kind, data, httpControlBytes, &ack, c.binding, sequence); err != nil {
		return err
	}
	if ack.httpBinding != c.binding || ack.Sequence != sequence || ack.Kind != kind || ack.Failure != nil {
		return httpClientUnknown(kind, errors.New("missing or mismatched successful acknowledgement"))
	}
	if kind == "outcome" {
		c.transport.outcomeAcknowledged = true
	}
	return nil
}

func (c *httpClientCheckpoints) beforeCallStop() error {
	cause := context.Cause(c.action)
	if !c.hardDeadline.IsZero() {
		var idle time.Time
		if c.inactivity != nil {
			idle = c.inactivity.deadline()
		}
		if limit := selectWorkerLimit(cause, time.Now(), c.hardDeadline, idle, c.ownRuntimeCap); limit != workerLimitNone {
			return limit.err()
		}
	}
	return cause
}

func (c *httpClientCheckpoints) checkpoint(kind string, value CallCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if kind == "before" {
		if err := c.beforeCallStop(); err != nil {
			return err
		}
	}
	if (httpBinding{Version: value.Version, Identity: value.Identity, SourceRef: value.SourceRef}) != c.binding {
		c.failure = httpClientUnknown(kind, errors.New("checkpoint binding differs from assignment"))
		return c.failure
	}
	if err := httpCallCheckpointShape(value); err != nil {
		c.failure = httpClientUnknown(kind, err)
		return c.failure
	}
	sequence := c.sequence + 1
	data, err := httpEncode(httpCall{Sequence: sequence, Checkpoint: value}, httpControlBytes)
	if err == nil {
		err = c.acknowledge(c.control, kind, data, sequence)
	} else {
		err = httpClientUnknown(kind, err)
	}
	if err == nil && kind == "before" {
		err = c.beforeCallStop()
	}
	c.failure = err
	return err
}

func (c *httpClientCheckpoints) BeforeCall(value CallCheckpoint) error {
	return c.checkpoint("before", value)
}

func (c *httpClientCheckpoints) AfterCall(value CallCheckpoint) error {
	return c.checkpoint("after", value)
}

func (c *httpClientCheckpoints) Review(value ReviewCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if (httpBinding{Version: value.Version, Identity: value.Identity, SourceRef: value.SourceRef}) != c.binding {
		c.failure = httpClientUnknown("review", errors.New("checkpoint binding differs from assignment"))
		return c.failure
	}
	if err := httpReviewShape(value); err != nil {
		c.failure = httpClientUnknown("review", err)
		return c.failure
	}
	sequence := c.sequence + 1
	data, err := httpEncode(httpReview{Sequence: sequence, Checkpoint: value}, httpReviewBytes)
	if err == nil {
		err = c.acknowledge(c.control, "review", data, sequence)
	} else {
		err = httpClientUnknown("review", err)
	}
	c.failure = err
	return err
}

func (c *httpClientCheckpoints) outcome(result Result, runErr error) error {
	var cause error
	if c.action != nil {
		cause = context.Cause(c.action)
	}
	return c.outcomeWithCause(result, runErr, cause)
}

func (c *httpClientCheckpoints) outcomeFailure(runErr, cause error) *httpFailure {
	limit := workerLimitNone
	if c.inactivity != nil {
		limit = selectWorkerLimit(cause, time.Now(), c.hardDeadline, c.inactivity.deadline(), c.ownRuntimeCap)
	}
	if limit == workerLimitNone {
		return newHTTPFailure(runErr)
	}
	failure := newHTTPFailure(errors.Join(runErr, limit.err()))
	failure.Terminal = true
	failure.Canceled = false // A selected limit outranks the model's later ctx.Err().
	failure.Inactivity = limit == workerLimitInactivity
	failure.RuntimeCap = limit == workerLimitRuntimeCap
	failure.Deadline = limit != workerLimitInactivity
	failure.ObservedMessage = failure.Message
	failure.Message = httpFailureDiagnostic(limit.err().Error())
	return failure
}

func (c *httpClientCheckpoints) outcomeWithCause(result Result, runErr, cause error) error {
	if err := c.control.Err(); err != nil {
		return httpClientUnknown("outcome", err)
	}
	sequence := c.sequence + 1
	outcome := httpOutcome{Sequence: sequence, Result: result, Failure: c.outcomeFailure(runErr, cause)}
	shapeErr := httpResultShape(result)
	matchingBinding := (httpBinding{Version: result.Version, Identity: result.Identity, SourceRef: result.SourceRef}) == c.binding
	if !matchingBinding {
		shapeErr = errors.Join(shapeErr, errors.New("outcome binding differs from assignment"))
	}
	var data []byte
	var err, omitted error
	if shapeErr == nil {
		data, err = httpEncode(outcome, httpOutcomeBytes)
	} else {
		err = shapeErr
	}
	if err != nil {
		c.transport.outcomeDegraded = true
		// Encode the one fallback before sending anything. Already acknowledged
		// receipts remain the parent's evidence; no After is inferred from Result.
		fallback := Result{Version: c.binding.Version, Identity: c.binding.Identity, SourceRef: c.binding.SourceRef}
		for _, diagnostic := range result.Diagnostics {
			if diagnostic.Code == "cleanup_failed" {
				fallback.Diagnostics = []Diagnostic{{Code: "cleanup_failed", Message: "Execution workspace cleanup failed; operator attention is required."}}
				break
			}
		}
		if matchingBinding {
			core := ReviewCheckpoint{Version: c.binding.Version, Identity: c.binding.Identity, SourceRef: c.binding.SourceRef, Calls: result.Calls}
			if httpReviewShape(core) == nil {
				fallback.Calls = result.Calls
			}
			core.Calls, core.Refutations = nil, result.Refutations
			if httpReviewShape(core) == nil {
				fallback.Refutations = result.Refutations
			}
			if boundedText(result.CriticStopReason, 32) {
				fallback.CriticStopReason = result.CriticStopReason
			}
		}
		outcome.Result = fallback
		omitted = httpClientUnknown("outcome", fmt.Errorf("primary HTTP outcome omitted: %w", err))
		outcome.Failure = c.outcomeFailure(errors.Join(runErr, omitted), cause)
		data, err = httpEncode(outcome, httpOutcomeBytes)
		if err != nil {
			return httpClientUnknown("outcome", err)
		}
	}
	if outcome.Failure != nil && outcome.Failure.OutcomeUnknown && !errors.Is(runErr, monitor.ErrExecutionOutcomeUnknown) {
		omitted = errors.Join(omitted, monitor.ErrExecutionOutcomeUnknown)
	}
	return errors.Join(omitted, c.acknowledge(c.control, "outcome", data, sequence))
}
