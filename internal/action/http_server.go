package action

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

// HTTPExecutor owns one supplied-data transaction. It does not prove worker readiness or cleanup.
type HTTPExecutor struct {
	mu             sync.Mutex
	executorID     string
	credentials    WorkerCredentials
	credentialsSet bool
	active         *httpTransaction
	pull           bool
	pullDone       <-chan struct{}
	closed         bool
	unavailable    bool
	changed        chan struct{}
	outcomeGrace   time.Duration
	status         Status
	hasStatus      bool
}

type httpTransaction struct {
	mu          sync.Mutex
	mutation    sync.Mutex
	observation sync.Mutex
	handlers    sync.WaitGroup

	job         Job
	credentials WorkerCredentials
	started     time.Time
	binding     httpBinding
	sink        Checkpoints
	ctx         context.Context
	budget      context.Context
	cancel      context.CancelFunc
	stopBudget  context.CancelFunc

	ready              bool
	delivered          bool
	fenced             bool
	watch              bool
	sequence           uint64
	phaseCeiling       int
	uploadReady        bool
	stopping           bool
	stop               chan struct{}
	done               chan struct{}
	finished           chan struct{}
	transportErr       error
	requiredControlErr error
	checkpointErr      error
	outcome            bool
	result             Result
	runErr             error
	quarantine         bool
}

type httpExecutionState struct {
	admitted bool
	outcome  bool
}

func NewHTTPExecutor(executorID string) (*HTTPExecutor, error) {
	if !workspace.ValidIdentitySegment(executorID) {
		return nil, errors.New("invalid HTTP executor identity")
	}
	return &HTTPExecutor{executorID: executorID, changed: make(chan struct{}), outcomeGrace: httpControlTimeout}, nil
}

// SetWorkerCredentials configures assignment-only credentials before the worker pulls.
func (e *HTTPExecutor) SetWorkerCredentials(credentials WorkerCredentials) error {
	if e == nil {
		return errors.New("HTTP executor is missing")
	}
	if err := credentials.validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.credentialsSet || e.active != nil || e.pull || e.closed || e.unavailable {
		return errors.New("HTTP executor credentials cannot change after startup")
	}
	e.credentials, e.credentialsSet = credentials, true
	return nil
}

func (e *HTTPExecutor) notifyLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *HTTPExecutor) Execute(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
	return e.execute(ctx, job, sink, nil)
}

func (e *HTTPExecutor) execute(ctx context.Context, job Job, sink Checkpoints, state *httpExecutionState) (result Result, runErr error) {
	started := time.Now()
	result = Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}
	expires := started.Add(job.RuntimeBudget)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline
	}
	budgetCtx, stopBudget := context.WithDeadline(ctx, expires)
	defer stopBudget()
	actionCtx, cancel := context.WithCancel(budgetCtx)
	tx := &httpTransaction{job: job, started: started, binding: httpJobBinding(job), sink: sink, ctx: actionCtx, budget: budgetCtx, cancel: cancel, stopBudget: stopBudget,
		stop: make(chan struct{}), done: make(chan struct{}), finished: make(chan struct{})}
	e.mu.Lock()
	if e.closed || e.unavailable || e.active != nil {
		e.mu.Unlock()
		cancel()
		return result, errors.New("HTTP executor is closed, unavailable or busy")
	}
	tx.credentials = e.credentials
	e.active, e.status, e.hasStatus = tx, Status{}, false
	if state != nil {
		state.admitted = true
	}
	e.mu.Unlock()
	defer func() {
		tx.mu.Lock()
		tx.fenced = true
		tx.mu.Unlock()
		cancel()
		// Saves have no context. An entered fsync must drain even after control grace expires.
		tx.handlers.Wait()
		tx.mu.Lock()
		if tx.outcome {
			// A received outcome resolves watch ambiguity, even if its own receipt ACK was lost.
			if state != nil {
				state.outcome = true
			}
			result, runErr = tx.result, tx.runErr
		} else if tx.delivered {
			runErr = errors.Join(runErr, monitor.ErrExecutionOutcomeUnknown, tx.transportErr, tx.checkpointErr, actionCtx.Err())
			tx.quarantine = true
		}
		if tx.requiredControlErr != nil {
			runErr = errors.Join(runErr, monitor.ErrExecutionOutcomeUnknown, tx.requiredControlErr)
		}
		quarantine := tx.quarantine
		tx.mu.Unlock()
		e.mu.Lock()
		e.active = nil
		e.unavailable = e.unavailable || quarantine
		e.notifyLocked()
		e.mu.Unlock()
		close(tx.finished)
	}()
	if sink == nil {
		return result, fmt.Errorf("%w: execution checkpoints are required", monitor.ErrTerminalJob)
	}
	if err := httpExecutionContextError(actionCtx); err != nil {
		return result, err
	}
	if err := job.Validate(); err != nil {
		return result, fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
	}
	if job.Identity.ExecutorID != e.executorID {
		return result, fmt.Errorf("%w: HTTP executor identity differs from Job", monitor.ErrTerminalJob)
	}
	if err := tx.credentials.validateJob(job); err != nil {
		return result, fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
	}
	if httpAssignmentBound(job, tx.credentials) > httpJobBytes {
		return result, fmt.Errorf("%w: conservative Job encoding bound: %w", monitor.ErrTerminalJob, errHTTPBodyLimit)
	}
	if err := httpExecutionContextError(actionCtx); err != nil {
		return result, err
	}
	tx.mu.Lock()
	tx.ready = true
	tx.mu.Unlock()
	e.mu.Lock()
	e.notifyLocked()
	e.mu.Unlock()
	select {
	case <-tx.done:
		return result, nil
	case <-actionCtx.Done():
	case <-tx.stop:
	}
	tx.mu.Lock()
	delivered, transportErr := tx.delivered, tx.transportErr
	tx.mu.Unlock()
	if !delivered {
		return result, errors.Join(httpExecutionContextError(actionCtx), transportErr)
	}
	timer := time.NewTimer(e.outcomeGrace)
	defer timer.Stop()
	select {
	case <-tx.done:
	case <-timer.C:
	}
	return result, httpExecutionContextError(actionCtx)
}

func httpExecutionContextError(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			err = context.DeadlineExceeded
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = errors.Join(monitor.ErrTerminalJob, err)
	}
	return err
}

func (e *HTTPExecutor) Close() error {
	e.mu.Lock()
	e.closed = true
	tx := e.active
	e.notifyLocked()
	e.mu.Unlock()
	if tx != nil {
		tx.stopBudget()
		<-tx.finished
	}
	return nil
}

func (e *HTTPExecutor) Status() (Status, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, e.hasStatus
}

func (e *HTTPExecutor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpServerReject(w, "error", http.StatusMethodNotAllowed, httpBinding{}, 0, errors.New("HTTP execution requires POST"))
		return
	}
	switch r.URL.Path {
	case "/next":
		e.serveNext(w, r)
	case "/cancel":
		e.serveCancel(w, r)
	case "/status":
		e.serveStatus(w, r)
	case "/before", "/after", "/review", "/outcome":
		e.serveMutation(w, r)
	default:
		httpServerReject(w, "error", http.StatusNotFound, httpBinding{}, 0, errors.New("unknown HTTP execution route"))
	}
}

func (e *HTTPExecutor) serveNext(w http.ResponseWriter, r *http.Request) {
	var pull httpPull
	if err := httpServerRead(w, r, httpControlBytes, &pull); err != nil {
		httpServerReject(w, "next", httpInputStatus(err), httpBinding{}, 0, err)
		return
	}
	pullAt := time.Now()
	if pull.Version != ProtocolVersion || pull.ExecutorID != e.executorID {
		httpServerReject(w, "next", http.StatusConflict, httpBinding{}, 0, errors.New("HTTP pull version or executor differs"))
		return
	}
	e.mu.Lock()
	if e.pull || e.closed || e.unavailable {
		rejection := errors.New("HTTP executor has no pull slot")
		if e.closed && !e.pull {
			rejection = context.Canceled
		}
		e.mu.Unlock()
		httpServerReject(w, "next", http.StatusConflict, httpBinding{}, 0, rejection)
		return
	}
	if r.Context().Err() != nil {
		e.mu.Unlock()
		return
	}
	e.pull = true
	e.pullDone = r.Context().Done()
	e.notifyLocked()
	e.mu.Unlock()
	var delivery *httpTransaction
	defer func() {
		if delivery == nil {
			e.mu.Lock()
			e.pull = false
			e.pullDone = nil
			e.notifyLocked()
			e.mu.Unlock()
		} else {
			delivery.handlers.Done()
		}
	}()
poll:
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			httpServerReject(w, "next", http.StatusConflict, httpBinding{}, 0, context.Canceled)
			return
		}
		if e.unavailable {
			e.mu.Unlock()
			httpServerReject(w, "next", http.StatusConflict, httpBinding{}, 0, errors.New("HTTP executor is unavailable"))
			return
		}
		tx, changed := e.active, e.changed
		if tx != nil {
			tx.mu.Lock()
			if tx.delivered || tx.fenced {
				completed := tx.outcome
				tx.mu.Unlock()
				e.mu.Unlock()
				if completed {
					for {
						select {
						case <-tx.finished:
							continue poll
						case <-r.Context().Done():
							return
						case <-changed:
							e.mu.Lock()
							closed := e.closed
							changed = e.changed
							e.mu.Unlock()
							if closed {
								continue poll
							}
						}
					}
				}
				httpServerReject(w, "next", http.StatusConflict, httpBinding{}, 0, errors.New("HTTP assignment is already claimed"))
				return
			}
			if tx.ready && httpExecutionContextError(tx.ctx) == nil && r.Context().Err() == nil {
				deadline, _ := tx.budget.Deadline()
				dispatchAt := time.Now()
				job := tx.job
				job.RuntimeBudget = max(deadline.Sub(dispatchAt), time.Nanosecond)
				body, err := httpEncode(httpAssignment{Job: job, Credentials: tx.credentials, HeldFor: dispatchAt.Sub(pullAt)}, httpJobBytes)
				if err != nil {
					tx.mu.Unlock()
					e.mu.Unlock()
					tx.stopExecution(fmt.Errorf("%w: encode HTTP assignment: %w", monitor.ErrTerminalJob, err), true)
					httpServerReject(w, "next", http.StatusInternalServerError, httpBinding{}, 0, err)
					return
				}
				if r.Context().Err() != nil {
					tx.mu.Unlock()
					e.mu.Unlock()
					return
				}
				if err := httpExecutionContextError(tx.ctx); err != nil {
					tx.mu.Unlock()
					e.mu.Unlock()
					httpServerReject(w, "next", http.StatusConflict, httpBinding{}, 0, err)
					return
				}
				tx.delivered = true
				tx.handlers.Add(1)
				delivery = tx
				tx.mu.Unlock()
				// The assignment ACK can start another pull before this handler returns.
				e.pull = false
				e.pullDone = nil
				e.notifyLocked()
				e.mu.Unlock()
				if err := httpServerWriteBytes(w, http.StatusOK, body); err != nil {
					tx.stopRequiredDelivery(err)
				}
				return
			}
			tx.mu.Unlock()
		}
		e.mu.Unlock()
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

func (e *HTTPExecutor) transaction() *httpTransaction {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active
}

func (tx *httpTransaction) enter(binding httpBinding, sequence uint64, kind string, failedOutcome bool) (bool, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	gap := sequence > tx.sequence+1
	if !tx.delivered || binding != tx.binding || sequence == 0 || sequence <= tx.sequence || (gap && !failedOutcome) {
		return false, errors.New("HTTP checkpoint binding, sequence or admission differs")
	}
	// A bound request consumes its sequence even when model admission is refused.
	tx.sequence = sequence
	if kind != "outcome" && tx.checkpointErr != nil {
		return false, tx.checkpointErr
	}
	if kind == "before" {
		err := httpExecutionContextError(tx.ctx)
		if err == nil && tx.stopping {
			err = errors.New("HTTP execution is stopping; no new call is admitted")
		}
		if err != nil {
			if tx.transportErr != nil && httpExecutionContextError(tx.budget) == nil {
				if tx.requiredControlErr == nil {
					tx.requiredControlErr = errors.Join(fmt.Errorf("HTTP before admission failed: %w", err), tx.transportErr)
				}
				tx.quarantine = true
			}
			return false, err
		}
	}
	return gap, nil
}

func (e *HTTPExecutor) serveStatus(w http.ResponseWriter, r *http.Request) {
	tx := e.transaction()
	if tx == nil || !tx.observation.TryLock() {
		httpServerReject(w, "status", http.StatusConflict, httpBinding{}, 0, errors.New("HTTP execution has no observation slot"))
		return
	}
	tx.mu.Lock()
	if tx.fenced || !tx.delivered {
		tx.mu.Unlock()
		tx.observation.Unlock()
		httpServerReject(w, "status", http.StatusConflict, httpBinding{}, 0, errors.New("HTTP execution has closed observation admission"))
		return
	}
	tx.mu.Unlock()
	var observation httpObservation
	err := httpServerRead(w, r, httpControlBytes, &observation)
	status := observation.Status
	binding := httpBinding{status.Version, status.Identity, status.SourceRef}
	code := httpInputStatus(err)
	if err == nil {
		err = httpBindingShape(binding)
	}
	if err != nil {
		binding = httpBinding{}
	} else {
		// Execute validates the Job before an assignment can be delivered.
		err = status.validate(tx.job)
		if err == nil {
			e.mu.Lock()
			tx.mu.Lock()
			if tx.fenced || !tx.delivered {
				err = errors.New("HTTP execution has closed observation admission")
			} else if status.Phase == "upload" && !tx.uploadReady {
				err = errors.New("HTTP Status upload exceeds accepted callback facts")
			} else if status.Phase != "upload" && status.Phase != "cleanup" && status.order(tx.job.rounds()) > tx.phaseCeiling {
				err = errors.New("HTTP Status phase exceeds accepted callback facts")
			} else if status.ProgressAge > time.Since(tx.started) {
				err = errors.New("HTTP Status progress age exceeds elapsed action time")
			} else if e.hasStatus {
				err = status.follows(e.status, tx.job.rounds())
			}
			if err == nil {
				e.status, e.hasStatus = status, true
			}
			tx.mu.Unlock()
			e.mu.Unlock()
		}
		if err != nil {
			code = http.StatusConflict
		}
	}
	if err == nil {
		code = http.StatusOK
	}
	tx.observation.Unlock()
	// Observation delivery never admits paid work or changes its sequence or outcome.
	if err != nil {
		httpServerReject(w, "status", code, binding, status.Sequence, err)
		return
	}
	_ = httpServerWrite(w, code, httpAcknowledgement{httpBinding: binding, Kind: "status", Sequence: status.Sequence})
}

// stopExecution begins control grace; a known store refusal need not cancel the worker before its error returns.
func (tx *httpTransaction) stopExecution(err error, cancel bool) {
	tx.mu.Lock()
	if err != nil && tx.transportErr == nil {
		tx.transportErr = err
	}
	if !tx.stopping {
		tx.stopping = true
		close(tx.stop)
	}
	tx.mu.Unlock()
	if cancel {
		tx.cancel()
	}
}

func (tx *httpTransaction) stopRequiredDelivery(err error) {
	tx.mu.Lock()
	if tx.requiredControlErr == nil {
		tx.requiredControlErr = err
	}
	tx.quarantine = true
	tx.mu.Unlock()
	tx.stopExecution(nil, true)
}

func (e *HTTPExecutor) serveMutation(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimPrefix(r.URL.Path, "/")
	tx := e.transaction()
	if tx == nil {
		httpServerReject(w, kind, http.StatusConflict, httpBinding{}, 0, errors.New("HTTP execution has no mutation slot"))
		return
	}
	tx.mutation.Lock()
	tx.mu.Lock()
	if tx.fenced || !tx.delivered {
		tx.mu.Unlock()
		tx.mutation.Unlock()
		httpServerReject(w, kind, http.StatusConflict, httpBinding{}, 0, errors.New("HTTP execution has closed callback admission"))
		return
	}
	tx.handlers.Add(1)
	tx.mu.Unlock()
	defer tx.handlers.Done()
	locked := true
	defer func() {
		if locked {
			tx.mutation.Unlock()
		}
	}()
	var binding httpBinding
	var sequence uint64
	var failedOutcome, sequenceGap bool
	var phaseProof int
	var allowUpload bool
	var apply func() error
	var err error
	switch kind {
	case "before", "after":
		var call httpCall
		err = httpServerRead(w, r, httpControlBytes, &call)
		binding, sequence = httpBinding{call.Checkpoint.Version, call.Checkpoint.Identity, call.Checkpoint.SourceRef}, call.Sequence
		if err == nil {
			err = httpCallShape(call.Checkpoint.Call)
		}
		if kind == "before" {
			fact := call.Checkpoint.Call
			phase := Status{Version: ProtocolVersion, Identity: tx.job.Identity, SourceRef: tx.job.Source.Head, Sequence: 1,
				WorkerState: "busy", Phase: fact.Role, Round: fact.Round, ReviewPhase: fact.ReviewPhase}
			if phase.validate(tx.job) == nil {
				phaseProof = phase.order(tx.job.rounds())
			}
		} else if tx.job.Critic == nil && call.Checkpoint.Call.Role == roleMain && call.Checkpoint.Call.Round == 1 && call.Checkpoint.Call.ReviewPhase == "" {
			allowUpload = true
		}
		apply = func() error {
			if kind == "before" {
				return tx.sink.BeforeCall(call.Checkpoint)
			}
			return tx.sink.AfterCall(call.Checkpoint)
		}
	case "review":
		var review httpReview
		err = httpServerRead(w, r, httpReviewBytes, &review)
		binding, sequence = httpBinding{review.Checkpoint.Version, review.Checkpoint.Identity, review.Checkpoint.SourceRef}, review.Sequence
		if err == nil {
			err = httpReviewShape(review.Checkpoint)
		}
		if tx.job.Critic != nil && review.Checkpoint.CriticStopReason != "" {
			allowUpload = true
		}
		apply = func() error { return tx.sink.Review(review.Checkpoint) }
	case "outcome":
		var outcome httpOutcome
		err = httpServerRead(w, r, httpOutcomeBytes, &outcome)
		binding, sequence = httpBinding{outcome.Result.Version, outcome.Result.Identity, outcome.Result.SourceRef}, outcome.Sequence
		if err == nil {
			err = httpResultShape(outcome.Result)
		}
		if err == nil {
			err = outcome.Failure.validate()
		}
		failedOutcome = outcome.Failure != nil
		apply = func() error {
			tx.mu.Lock()
			tx.result, tx.runErr = outcome.Result, errors.Join(outcome.Failure.workerErr(), tx.checkpointErr)
			for _, diagnostic := range outcome.Result.Diagnostics {
				if diagnostic.Code == "cleanup_failed" {
					tx.quarantine = true
					break
				}
			}
			// A missing callback cannot be acknowledged from raw outcome claims.
			if sequenceGap {
				tx.runErr = errors.Join(tx.runErr, monitor.ErrExecutionOutcomeUnknown)
				tx.quarantine = true
			}
			// Preserve late paid facts without accepting a success beyond the fixed expiry.
			tx.runErr = errors.Join(tx.runErr, httpExecutionContextError(tx.budget))
			tx.outcome, tx.fenced = true, true
			close(tx.done)
			tx.mu.Unlock()
			return nil
		}
	}
	if err == nil {
		err = httpBindingShape(binding)
	}
	if err != nil {
		tx.mutation.Unlock()
		locked = false
		httpServerReject(w, kind, httpInputStatus(err), httpBinding{}, sequence, err)
		return
	}
	sequenceGap, err = tx.enter(binding, sequence, kind, failedOutcome)
	if err != nil {
		tx.mutation.Unlock()
		locked = false
		httpServerReject(w, kind, http.StatusConflict, binding, sequence, err)
		return
	}
	callbackErr := apply()
	if callbackErr != nil {
		tx.mu.Lock()
		if tx.checkpointErr == nil {
			tx.checkpointErr = callbackErr
		}
		tx.mu.Unlock()
		tx.stopExecution(nil, false)
	} else if phaseProof > 0 || allowUpload {
		tx.mu.Lock()
		tx.phaseCeiling = max(tx.phaseCeiling, phaseProof)
		tx.uploadReady = tx.uploadReady || allowUpload
		tx.mu.Unlock()
	}
	// Release mutation ownership before ACK bytes can let a sequential client send its next request.
	tx.mutation.Unlock()
	locked = false
	status := http.StatusOK
	if callbackErr != nil {
		status = http.StatusInternalServerError
	}
	ack := httpAcknowledgement{httpBinding: binding, Sequence: sequence, Kind: kind, Failure: newHTTPFailure(callbackErr)}
	if err := httpServerWrite(w, status, ack); err != nil {
		if kind == "outcome" {
			tx.mu.Lock()
			tx.quarantine = true
			tx.mu.Unlock()
		} else {
			tx.stopRequiredDelivery(err)
		}
	}
}

func (e *HTTPExecutor) serveCancel(w http.ResponseWriter, r *http.Request) {
	var binding httpBinding
	if err := httpServerRead(w, r, httpControlBytes, &binding); err != nil {
		httpServerReject(w, "cancel", httpInputStatus(err), httpBinding{}, 0, err)
		return
	}
	if err := httpBindingShape(binding); err != nil {
		httpServerReject(w, "cancel", http.StatusBadRequest, httpBinding{}, 0, err)
		return
	}
	tx := e.transaction()
	if tx == nil {
		httpServerReject(w, "cancel", http.StatusConflict, binding, 0, errors.New("HTTP execution is absent"))
		return
	}
	tx.mu.Lock()
	if tx.fenced || !tx.delivered || tx.watch || binding != tx.binding {
		tx.mu.Unlock()
		httpServerReject(w, "cancel", http.StatusConflict, binding, 0, errors.New("HTTP cancellation binding or admission differs"))
		return
	}
	tx.watch = true
	tx.handlers.Add(1)
	tx.mu.Unlock()
	defer tx.handlers.Done()
	if err := httpServerWatchHeaders(w); err != nil {
		tx.stopExecution(err, true)
		return
	}
	select {
	case <-tx.ctx.Done():
	case <-tx.done:
	case <-r.Context().Done():
		tx.mu.Lock()
		sealed := tx.outcome
		tx.mu.Unlock()
		if !sealed {
			tx.stopExecution(r.Context().Err(), true)
		}
		return
	}
	tx.mu.Lock()
	reason := "canceled"
	if tx.outcome {
		reason = "completed"
	} else if errors.Is(tx.ctx.Err(), context.DeadlineExceeded) {
		reason = "deadline"
	}
	tx.mu.Unlock()
	if err := httpServerWatchReason(w, httpCancellation{httpBinding: binding, Reason: reason}); err != nil {
		tx.mu.Lock()
		sealed := tx.outcome
		tx.mu.Unlock()
		if !sealed {
			tx.stopExecution(err, true)
		}
	}
}

func httpServerRead(w http.ResponseWriter, r *http.Request, limit int, value any) (readErr error) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errors.New("HTTP execution requires JSON content type")
	}
	encodings := r.Header.Values("Content-Encoding")
	if len(encodings) > 1 || len(encodings) == 1 && encodings[0] != "" && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity") {
		return errors.New("HTTP execution does not accept content encoding")
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(httpControlTimeout)); err != nil {
		return err
	}
	defer func() { readErr = errors.Join(readErr, controller.SetReadDeadline(time.Time{})) }()
	return httpDecode(r.Body, limit, value)
}

func httpInputStatus(err error) int {
	if errors.Is(err, errHTTPBodyLimit) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func httpServerReject(w http.ResponseWriter, kind string, status int, binding httpBinding, sequence uint64, err error) {
	// Do not let net/http drain an unfinished rejected body before or after the ACK.
	w.Header().Set("Connection", "close")
	err = errors.Join(err, http.NewResponseController(w).SetReadDeadline(time.Now()))
	_ = httpServerWrite(w, status, httpAcknowledgement{httpBinding: binding, Kind: kind, Sequence: sequence, Failure: newHTTPFailure(err)})
}

func httpServerWrite(w http.ResponseWriter, status int, value any) error {
	body, err := httpEncode(value, httpControlBytes)
	if err != nil {
		return err
	}
	return httpServerWriteBytes(w, status, body)
}

// Flushed headers prove watch admission before the worker starts Runner; the reason body comes later.
func httpServerWatchHeaders(w http.ResponseWriter) (writeErr error) {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(httpControlTimeout)); err != nil {
		return err
	}
	defer func() { writeErr = errors.Join(writeErr, controller.SetWriteDeadline(time.Time{})) }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return controller.Flush()
}

func httpServerWatchReason(w http.ResponseWriter, cancellation httpCancellation) error {
	body, err := httpEncode(cancellation, httpControlBytes)
	if err != nil {
		return err
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(httpControlTimeout)); err != nil {
		return err
	}
	n, err := w.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	// Keep this deadline through net/http's final framing of the already-started response.
	return controller.Flush()
}

func httpServerWriteBytes(w http.ResponseWriter, status int, body []byte) (writeErr error) {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(httpControlTimeout)); err != nil {
		return err
	}
	defer func() { writeErr = errors.Join(writeErr, controller.SetWriteDeadline(time.Time{})) }()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	// Flush required ACKs while their handlers are counted, before Execute returns.
	return controller.Flush()
}
