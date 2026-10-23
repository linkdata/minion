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
	"sync/atomic"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

var errHTTPWatchStopped = errors.New("HTTP cancellation watch stopped after worker return")

type httpExecutionClient struct {
	client      *http.Client
	base        *url.URL
	outcomeSent atomic.Bool
}

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
	transport *httpExecutionClient
	control   context.Context
	action    context.Context
	job       Job
	binding   httpBinding
	sequence  uint64
	failure   error
}

// RunHTTP pulls and executes one assignment. Returning does not prove process release.
func RunHTTP(ctx context.Context, client *http.Client, baseURL, executorID string, runner *Runner) error {
	if client == nil || runner == nil || !workspace.ValidIdentitySegment(executorID) {
		return errors.New("HTTP worker requires a client, runner and valid executor identity")
	}
	base, err := url.Parse(baseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.RawFragment != "" || base.Opaque != "" {
		return errors.New("invalid HTTP execution base URL")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	configured := *client
	configured.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := &httpExecutionClient{client: &configured, base: base}
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: executorID}, httpControlBytes)
	if err != nil {
		return err
	}
	var assignment httpAssignment
	if err := transport.post(ctx, "next", pull, httpJobBytes, &assignment, httpBinding{}, 0); err != nil {
		return err
	}
	job := assignment.Job
	if err := job.Validate(); err != nil {
		return httpClientUnknown("next", err)
	}
	if job.Identity.ExecutorID != executorID {
		return httpClientUnknown("next", errors.New("assignment executor differs from pull"))
	}
	expires := time.Unix(0, assignment.Expires)
	if supplied := time.Now().Add(job.RuntimeBudget); supplied.Before(expires) {
		expires = supplied
	}
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline
	}
	if assignment.Expires <= 0 {
		return httpClientUnknown("next", errors.New("invalid assignment expiry"))
	}
	checkpoints := &httpClientCheckpoints{transport: transport, control: context.WithoutCancel(ctx), job: job, binding: httpJobBinding(job)}
	if !time.Now().Before(expires) {
		err := errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		return errors.Join(err, checkpoints.outcome(result, err))
	}
	deadlineCtx, stopDeadline := context.WithDeadline(ctx, expires)
	defer stopDeadline()
	actionCtx, cancelAction := context.WithCancelCause(deadlineCtx)
	defer cancelAction(context.Canceled)
	checkpoints.action = actionCtx
	watchCtx, stopWatch := context.WithCancelCause(ctx)
	watchBody, err := httpEncode(checkpoints.binding, httpControlBytes)
	if err != nil {
		stopWatch(errHTTPWatchStopped)
		return fmt.Errorf("encode cancellation admission: %w", err)
	}
	// Bound admission only; the held body must survive model expiry for paid receipts.
	headerErr := fmt.Errorf("cancellation watch admission timed out: %w", context.DeadlineExceeded)
	headerDone := make(chan struct{})
	headerTimer := time.AfterFunc(min(httpControlTimeout, time.Until(expires)), func() {
		stopWatch(headerErr)
		close(headerDone)
	})
	watchResponse, err := transport.open(watchCtx, "cancel", watchBody, checkpoints.binding, 0, true)
	if !headerTimer.Stop() {
		<-headerDone
	}
	if context.Cause(watchCtx) == headerErr {
		if watchResponse != nil {
			_ = watchResponse.Body.Close()
		}
		err = headerErr
		if !time.Now().Before(expires) {
			err = errors.Join(monitor.ErrTerminalJob, err)
		}
	}
	if err != nil {
		stopWatch(errHTTPWatchStopped)
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		return errors.Join(err, checkpoints.outcome(result, err))
	}
	watchDone := make(chan error, 1)
	go func() {
		err := transport.watch(watchCtx, watchResponse, checkpoints.binding)
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
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
	if err := checkpoints.status("prepare", 1); err != nil {
		runner.logf("HTTP prepare observation: %v", err)
	}
	var runErr error
	// Preparation and status transfer consume the same accepted runtime.
	job.RuntimeBudget = time.Until(expires)
	if job.RuntimeBudget <= 0 {
		runErr = errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
	} else {
		result, runErr = runner.Execute(actionCtx, job, checkpoints)
	}
	if runErr == nil {
		if err := checkpoints.status("upload", 2); err != nil {
			runner.logf("HTTP upload observation: %v", err)
		}
	}
	if cause := context.Cause(actionCtx); errors.Is(cause, context.DeadlineExceeded) {
		// A parent deadline reply can cancel the native call just before its local timer.
		runErr = errors.Join(runErr, monitor.ErrTerminalJob, context.DeadlineExceeded)
	} else if cause != nil {
		runErr = errors.Join(runErr, cause)
	}
	outcomeErr := checkpoints.outcome(result, runErr)
	stopWatch(errHTTPWatchStopped)
	return errors.Join(runErr, outcomeErr, <-watchDone)
}

func httpClientUnknown(kind string, err error) error {
	return fmt.Errorf("%w: HTTP %s: %w", monitor.ErrExecutionOutcomeUnknown, kind, err)
}

func (c *httpExecutionClient) post(ctx context.Context, kind string, data []byte, limit int, result any, binding httpBinding, sequence uint64) error {
	response, err := c.open(ctx, kind, data, binding, sequence, false)
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
func (c *httpExecutionClient) open(ctx context.Context, kind string, data []byte, binding httpBinding, sequence uint64, knownNoWork bool) (*http.Response, error) {
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
		if knownNoWork {
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
		// The assignment wait is unbounded; only its received body uses control time.
		cancel, done := pullCancel, make(chan struct{})
		timer := time.AfterFunc(httpControlTimeout, func() {
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

func (c *httpExecutionClient) watch(ctx context.Context, response *http.Response, binding httpBinding) error {
	defer response.Body.Close()
	var cancellation httpCancellation
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
	case "canceled":
		return fmt.Errorf("parent action canceled: %w", context.Canceled)
	case "deadline":
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

func (c *httpClientCheckpoints) acknowledge(kind string, data []byte, sequence uint64) error {
	parent := c.control
	if kind == "status" {
		parent = c.action
	}
	ctx, cancel := context.WithTimeout(parent, httpControlTimeout)
	defer cancel()
	if kind != "status" {
		c.sequence = sequence
	}
	var ack httpAcknowledgement
	if err := c.transport.post(ctx, kind, data, httpControlBytes, &ack, c.binding, sequence); err != nil {
		return err
	}
	if ack.httpBinding != c.binding || ack.Sequence != sequence || ack.Kind != kind || ack.Failure != nil {
		return httpClientUnknown(kind, errors.New("missing or mismatched successful acknowledgement"))
	}
	return nil
}

func (c *httpClientCheckpoints) checkpoint(kind string, value CallCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if kind == "before" && c.action.Err() != nil {
		return context.Cause(c.action)
	}
	if (httpBinding{Version: value.Version, Identity: value.Identity, SourceRef: value.SourceRef}) != c.binding {
		c.failure = httpClientUnknown(kind, errors.New("checkpoint binding differs from assignment"))
		return c.failure
	}
	if err := httpCallShape(value.Call); err != nil {
		c.failure = httpClientUnknown(kind, err)
		return c.failure
	}
	sequence := c.sequence + 1
	data, err := httpEncode(httpCall{Sequence: sequence, Checkpoint: value}, httpControlBytes)
	if err == nil {
		err = c.acknowledge(kind, data, sequence)
	} else {
		err = httpClientUnknown(kind, err)
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
		err = c.acknowledge("review", data, sequence)
	} else {
		err = httpClientUnknown("review", err)
	}
	c.failure = err
	return err
}

func (c *httpClientCheckpoints) status(phase string, observation uint64) error {
	status := Status{Version: c.binding.Version, Identity: c.binding.Identity, SourceRef: c.binding.SourceRef, Sequence: observation, WorkerState: "busy", Phase: phase}
	data, err := httpEncode(httpObservation{Status: status}, httpControlBytes)
	if err != nil {
		return httpClientUnknown("status", err)
	}
	return c.acknowledge("status", data, observation)
}

func (c *httpClientCheckpoints) outcome(result Result, runErr error) error {
	sequence := c.sequence + 1
	outcome := httpOutcome{Sequence: sequence, Result: result, Failure: newHTTPFailure(runErr)}
	shapeErr := httpResultShape(result)
	if !result.matchesJob(c.job) {
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
		// Encode the one fallback before sending anything. Already acknowledged
		// receipts remain the parent's evidence; no After is inferred from Result.
		fallback := Result{Version: c.binding.Version, Identity: c.binding.Identity, SourceRef: c.binding.SourceRef}
		if result.matchesJob(c.job) {
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
		outcome.Failure = newHTTPFailure(errors.Join(runErr, omitted))
		data, err = httpEncode(outcome, httpOutcomeBytes)
		if err != nil {
			return httpClientUnknown("outcome", err)
		}
	}
	if outcome.Failure != nil && outcome.Failure.OutcomeUnknown && !errors.Is(runErr, monitor.ErrExecutionOutcomeUnknown) {
		omitted = errors.Join(omitted, monitor.ErrExecutionOutcomeUnknown)
	}
	return errors.Join(omitted, c.acknowledge("outcome", data, sequence))
}
