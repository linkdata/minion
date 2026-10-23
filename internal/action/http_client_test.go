package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workroot"
)

func TestRunHTTPReportsActualObservationsAndRawPaidReceipts(t *testing.T) {
	for _, test := range []struct {
		name            string
		invalidCounters bool
		holdWatch       bool
		lostStatus      string
		malformedStatus bool
	}{
		{name: "complete"},
		{name: "invalid signed counters", invalidCounters: true},
		{name: "intentional held watch stop", holdWatch: true},
		{name: "lost prepare observation ACK", lostStatus: "prepare"},
		{name: "malformed upload observation ACK", lostStatus: "upload", malformedStatus: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			expires := time.Now().Add(job.RuntimeBudget)
			tokens := usage.Tokens{Input: 17, CachedInput: 3, Output: 5}
			if test.invalidCounters {
				tokens.Input = -1
			}
			var next, sequence, native atomic.Uint64
			var watchAdmitted atomic.Bool
			sealed := make(chan struct{})
			events := make(chan string, 8)
			after := make(chan CallUsage, 1)
			outcome := make(chan httpOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("client did not use explicit uncompressed JSON POST")
				}
				var requestSequence uint64
				kind := strings.TrimPrefix(r.URL.Path, "/")
				switch kind {
				case "next":
					var pull httpPull
					if err := httpDecode(r.Body, httpControlBytes, &pull); err != nil || pull.Version != ProtocolVersion || pull.ExecutorID != job.Identity.ExecutorID || next.Add(1) != 1 {
						t.Errorf("invalid or repeated pull: %+v error=%v", pull, err)
					}
					events <- "next"
					httpClientTestJSON(t, w, httpAssignment{Job: job, Expires: expires.UnixNano()})
					return
				case "cancel":
					var value httpBinding
					if err := httpDecode(r.Body, httpControlBytes, &value); err != nil || value != binding {
						t.Errorf("foreign watch: %+v error=%v", value, err)
					}
					watchAdmitted.Store(true)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					select {
					case <-sealed:
						if test.holdWatch {
							<-r.Context().Done()
						} else {
							_ = json.NewEncoder(w).Encode(httpCancellation{httpBinding: binding, Reason: "completed"})
						}
					case <-r.Context().Done():
					}
					return
				case "status":
					var value httpObservation
					if err := httpDecode(r.Body, httpControlBytes, &value); err != nil || value.Status.Validate(job) != nil || value.Status.WorkerState != "busy" {
						t.Errorf("invalid observation: %+v error=%v", value, err)
					}
					want := uint64(1)
					if value.Status.Phase == "upload" {
						want = 2
					}
					if value.Status.Sequence != want {
						t.Errorf("status sequence reused transport sequence: %+v", value)
					}
					requestSequence = value.Status.Sequence
					events <- "status/" + value.Status.Phase
					if value.Status.Phase == test.lostStatus {
						if test.malformedStatus {
							w.Header().Set("Content-Type", "application/json")
							_, _ = fmt.Fprint(w, "{")
						} else {
							connection, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
							} else {
								_ = connection.Close()
							}
						}
						return
					}
				case "before", "after":
					var value httpCall
					if err := httpDecode(r.Body, httpControlBytes, &value); err != nil {
						t.Error(err)
					}
					requestSequence = value.Sequence
					if kind == "after" {
						after <- value.Checkpoint.Call
					}
					events <- kind
				case "outcome":
					var value httpOutcome
					if err := httpDecode(r.Body, httpOutcomeBytes, &value); err != nil {
						t.Error(err)
					}
					requestSequence = value.Sequence
					outcome <- value
					close(sealed)
					events <- "outcome"
				default:
					t.Errorf("unexpected worker request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if kind != "status" && requestSequence != sequence.Add(1) {
					t.Errorf("transport sequence skipped or repeated: %d", requestSequence)
				}
				httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Sequence: requestSequence, Kind: kind})
			}))
			defer server.Close()
			runner := &Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
				if native.Add(1) != 1 || request.role != roleMain || !watchAdmitted.Load() {
					t.Error("worker repeated native execution or ran before watch admission")
					return aicli.Result{}, errors.New("unexpected native admission")
				}
				if deadline, ok := ctx.Deadline(); !ok || deadline.After(expires) {
					t.Errorf("worker renewed the accepted budget: %v %t", deadline, ok)
					return aicli.Result{}, errors.New("renewed native runtime")
				}
				return aicli.Result{Answer: "Complete answer", Tokens: tokens, UsageComplete: true}, nil
			}}
			err := RunHTTP(context.Background(), server.Client(), server.URL, job.Identity.ExecutorID, runner)
			if test.invalidCounters {
				if !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("known invalid usage changed failure policy: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantEvents := []string{"next", "status/prepare", "before", "after", "status/upload", "outcome"}
			if len(events) != len(wantEvents) || len(after) != 1 || len(outcome) != 1 || next.Load() != 1 || native.Load() != 1 {
				t.Fatalf("missing transaction observations: events=%d after=%d outcome=%d pulls=%d native=%d", len(events), len(after), len(outcome), next.Load(), native.Load())
			}
			var gotEvents []string
			for range wantEvents {
				gotEvents = append(gotEvents, <-events)
			}
			paid, returned := <-after, <-outcome
			if !slices.Equal(gotEvents, wantEvents) || paid.Tokens != tokens || !paid.Complete || len(returned.Result.Calls) != 1 || returned.Result.Calls[0] != paid {
				t.Fatalf("raw paid evidence or order changed: events=%v paid=%+v returned=%+v", gotEvents, paid, returned)
			}
			if test.invalidCounters {
				if returned.Failure == nil || !returned.Failure.Terminal || returned.Failure.OutcomeUnknown || returned.Result.Answer != "" {
					t.Fatalf("invalid raw receipt was discarded or turned into success: %+v", returned)
				}
			} else if returned.Failure != nil || returned.Result.Validate(job) != nil || returned.Result.Answer != "Complete answer" {
				t.Fatalf("valid one-shot execution did not return its actual result: %+v", returned)
			}
		})
	}
}

func TestHTTPClientRequiresExactAcknowledgementAndNeverRetries(t *testing.T) {
	for _, name := range []string{"known failure", "empty", "malformed", "trailing", "unknown field", "foreign binding", "wrong kind", "wrong sequence", "success with failure", "content type", "encoding", "multiple encodings", "oversized"} {
		t.Run(name, func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				ack := httpAcknowledgement{httpBinding: binding, Sequence: 1, Kind: "before"}
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "known failure":
					ack.Failure = newHTTPFailure(workroot.ErrUnsafe)
					w.WriteHeader(http.StatusConflict)
				case "empty":
					return
				case "malformed":
					_, _ = fmt.Fprint(w, "{")
					return
				case "trailing":
					httpClientTestJSON(t, w, ack)
					_, _ = fmt.Fprint(w, "{}")
					return
				case "unknown field":
					_, _ = fmt.Fprint(w, `{"unexpected":true}`)
					return
				case "foreign binding":
					ack.Identity.AttemptID = "foreign"
				case "wrong kind":
					ack.Kind = "after"
				case "wrong sequence":
					ack.Sequence++
				case "success with failure":
					ack.Failure = newHTTPFailure(workroot.ErrUnsafe)
				case "content type":
					w.Header().Set("Content-Type", "text/plain")
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "multiple encodings":
					w.Header().Set("Content-Encoding", "identity")
					w.Header().Add("Content-Encoding", "gzip")
				case "oversized":
					_, _ = fmt.Fprint(w, strings.Repeat(" ", httpControlBytes+1))
					return
				}
				if err := json.NewEncoder(w).Encode(ack); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints := &httpClientCheckpoints{transport: &httpExecutionClient{client: server.Client(), base: base}, control: context.Background(), action: context.Background(), job: job, binding: binding}
			call := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1}}
			err = checkpoints.BeforeCall(call)
			if name == "known failure" {
				if !errors.Is(err, workroot.ErrUnsafe) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("valid finite negative ACK lost its known failure: %v", err)
				}
			} else if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("missing or invalid ACK was treated as known: %v", err)
			}
			if second := checkpoints.BeforeCall(call); second != err || requests.Load() != 1 {
				t.Fatalf("failed callback was retried: first=%v second=%v requests=%d", err, second, requests.Load())
			}
		})
	}
}

func TestRunHTTPRejectsAssignmentBeforeWatchOrRunner(t *testing.T) {
	for _, name := range []string{"version", "executor", "expired", "expired outcome ACK lost", "missing expiry", "trailing JSON"} {
		t.Run(name, func(t *testing.T) {
			job := testJob(command.Comment)
			assignment := httpAssignment{Job: job, Expires: time.Now().Add(job.RuntimeBudget).UnixNano()}
			switch name {
			case "version":
				assignment.Job.Version++
			case "executor":
				assignment.Job.Identity.ExecutorID = "foreign"
			case "expired", "expired outcome ACK lost":
				assignment.Expires = time.Now().Add(-time.Minute).UnixNano()
			case "missing expiry":
				assignment.Expires = 0
			}
			var requests atomic.Int32
			outcomes := make(chan httpOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/next":
					httpClientTestJSON(t, w, assignment)
					if name == "trailing JSON" {
						_, _ = fmt.Fprint(w, "{}")
					}
				case "/outcome":
					var outcome httpOutcome
					if err := httpDecode(r.Body, httpOutcomeBytes, &outcome); err != nil {
						t.Error(err)
					}
					outcomes <- outcome
					w.Header().Set("Content-Type", "application/json")
					if name != "expired outcome ACK lost" {
						httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "outcome", Sequence: outcome.Sequence})
					}
				default:
					t.Errorf("invalid or expired assignment advanced to %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			err := RunHTTP(context.Background(), server.Client(), server.URL, job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
			if name == "expired" || name == "expired outcome ACK lost" {
				if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != (name == "expired outcome ACK lost") || requests.Load() != 2 || len(outcomes) != 1 {
					t.Fatalf("expired known assignment lost its bound no-work outcome: error=%v requests=%d outcomes=%d", err, requests.Load(), len(outcomes))
				}
				outcome := <-outcomes
				if outcome.Sequence != 1 || !outcome.Result.matchesJob(job) || outcome.Result.Answer != "" || len(outcome.Result.Calls) != 0 || outcome.Failure == nil || !outcome.Failure.Terminal || !outcome.Failure.Deadline || outcome.Failure.OutcomeUnknown {
					t.Fatalf("expired assignment fabricated work or unknown execution: %+v", outcome)
				}
			} else if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || requests.Load() != 1 || len(outcomes) != 0 {
				t.Fatalf("invalid assignment ran or became repeatable: error=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestRunHTTPRequiresWatchAdmissionBeforeRunner(t *testing.T) {
	for _, name := range []string{"known negative ACK", "parent unknown failure", "invalid headers"} {
		t.Run(name, func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			outcomes := make(chan httpOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/next":
					httpClientTestJSON(t, w, httpAssignment{Job: job, Expires: time.Now().Add(job.RuntimeBudget).UnixNano()})
				case "/cancel":
					if name != "invalid headers" {
						failure := workroot.ErrUnsafe
						if name == "parent unknown failure" {
							failure = errors.Join(monitor.ErrExecutionOutcomeUnknown, processgroup.ErrUnconfirmed, processgroup.ErrScopeUnconfirmed)
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusConflict)
						httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Kind: "cancel", Failure: newHTTPFailure(failure)})
					} else {
						w.Header().Set("Content-Type", "text/plain")
					}
				case "/outcome":
					var value httpOutcome
					if err := httpDecode(r.Body, httpOutcomeBytes, &value); err != nil {
						t.Error(err)
					}
					outcomes <- value
					httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Sequence: value.Sequence, Kind: "outcome"})
				default:
					t.Errorf("worker advanced before watch admission: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			err := RunHTTP(context.Background(), server.Client(), server.URL, job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
			unknown := name == "parent unknown failure"
			if err == nil || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != unknown || name == "known negative ACK" && !errors.Is(err, workroot.ErrUnsafe) || len(outcomes) != 1 {
				t.Fatalf("watch admission failure lost its cause or failed outcome: error=%v outcomes=%d", err, len(outcomes))
			}
			outcome := <-outcomes
			if outcome.Sequence != 1 || !outcome.Result.matchesJob(job) || outcome.Failure == nil || outcome.Failure.OutcomeUnknown != unknown || len(outcome.Result.Calls) != 0 || outcome.Result.Answer != "" {
				t.Fatalf("watch startup failure fabricated execution: %+v", outcome)
			}
			if unknown && (!errors.Is(err, processgroup.ErrUnconfirmed) || !errors.Is(err, processgroup.ErrScopeUnconfirmed) || !outcome.Failure.Unconfirmed || !outcome.Failure.ScopeUnconfirmed) {
				t.Fatalf("known no-work transport mode erased supplied parent failure flags: error=%v outcome=%+v", err, outcome)
			}
		})
	}
}

func TestRunHTTPBoundsStalledWatchHeadersByAcceptedExpiry(t *testing.T) {
	job := testJob(command.Comment)
	binding := httpJobBinding(job)
	watchEntered := make(chan struct{}, 1)
	outcomes := make(chan httpOutcome, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/next":
			httpClientTestJSON(t, w, httpAssignment{Job: job, Expires: time.Now().Add(time.Second).UnixNano()})
		case "/cancel":
			var value httpBinding
			if err := httpDecode(r.Body, httpControlBytes, &value); err != nil || value != binding {
				t.Errorf("invalid stalled watch: binding=%+v error=%v", value, err)
			}
			watchEntered <- struct{}{}
			<-r.Context().Done() // Deliberately never admit the watch with response headers.
		case "/outcome":
			var outcome httpOutcome
			if err := httpDecode(r.Body, httpOutcomeBytes, &outcome); err != nil {
				t.Error(err)
			}
			outcomes <- outcome
			httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: outcome.Sequence})
		default:
			t.Errorf("worker continued while watch headers were stalled: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunHTTP(ctx, server.Client(), server.URL, job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
	if errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || ctx.Err() != nil || len(watchEntered) != 1 || len(outcomes) != 1 {
		t.Fatalf("stalled watch outlived the accepted runtime or ran models: error=%v caller=%v watch=%d outcome=%d", err, ctx.Err(), len(watchEntered), len(outcomes))
	}
	outcome := <-outcomes
	if outcome.Failure == nil || outcome.Failure.OutcomeUnknown || !outcome.Failure.Deadline || !outcome.Failure.Terminal || len(outcome.Result.Calls) != 0 || outcome.Result.Answer != "" {
		t.Fatalf("stalled watch fabricated execution or lost its admission deadline: %+v", outcome)
	}
}

func TestRunHTTPKnownNoWorkOutcomeReleasesParentCapacity(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired assignment=%t", expired), func(t *testing.T) {
			job := testJob(command.Comment)
			if expired {
				job.RuntimeBudget = time.Second
			}
			var callbacks atomic.Int32
			sink := runnerExecutionCheckpoints{
				before: func(CallCheckpoint) error { callbacks.Add(1); return nil },
				after:  func(CallCheckpoint) error { callbacks.Add(1); return nil },
				review: func(ReviewCheckpoint) error { callbacks.Add(1); return nil },
			}
			f := newHTTPServerTest(t, context.Background(), job, sink, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !expired && r.URL.Path == "/cancel" {
						w = httpClientFailedWatchHeaders{w}
					}
					next.ServeHTTP(w, r)
				})
			})
			client := *f.server.Client()
			transport := client.Transport
			client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if expired && r.URL.Path == "/next" && err == nil {
					<-f.tx.budget.Done() // Deliver the valid assignment only after its fixed expiry.
				}
				return response, err
			})
			err := RunHTTP(context.Background(), &client, f.server.URL, job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
			got := f.result(t)
			if err == nil || got.err == nil || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || callbacks.Load() != 0 || !got.result.matchesJob(job) || len(got.result.Calls) != 0 || got.result.Answer != "" {
				t.Fatalf("conclusive no-work outcome became unknown or fabricated work: worker=%v parent=%v result=%+v callbacks=%d", err, got.err, got.result, callbacks.Load())
			}
			if expired && (!errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(got.err, monitor.ErrTerminalJob) || !errors.Is(got.err, context.DeadlineExceeded)) {
				t.Fatalf("expired assignment lost its known deadline cause: worker=%v parent=%v", err, got.err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := f.executor.Execute(ctx, job, sink); !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("known no-work outcome quarantined reusable parent capacity: %v", err)
			}
		})
	}
}

type httpClientTestTransport func(*http.Request) (*http.Response, error)

func (f httpClientTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type httpClientFailedWatchHeaders struct{ http.ResponseWriter }

func (w httpClientFailedWatchHeaders) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w httpClientFailedWatchHeaders) FlushError() error           { return io.ErrClosedPipe }
func (w httpClientFailedWatchHeaders) WriteHeader(status int) {
	w.Header().Set("Content-Type", "text/plain")
	w.ResponseWriter.WriteHeader(status)
}

func TestHTTPClientWatchDistinguishesLocalStopFromParsedReplies(t *testing.T) {
	binding := httpJobBinding(testJob(command.Comment))
	for _, test := range []struct {
		name    string
		reason  string
		readErr error
		want    error
		unknown bool
	}{
		{name: "local explicit cause", readErr: errHTTPWatchStopped, want: context.Canceled},
		{name: "local context sentinel", readErr: context.Canceled, want: context.Canceled},
		{name: "malformed body", unknown: true},
		{name: "parent deadline", reason: "deadline", want: context.DeadlineExceeded},
		{name: "parent cancellation", reason: "canceled", want: context.Canceled},
		{name: "premature completed", reason: "completed", unknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, stop := context.WithCancelCause(context.Background())
			stop(errHTTPWatchStopped)
			var reader io.Reader = strings.NewReader("{")
			if test.readErr != nil {
				reader = iotest.ErrReader(test.readErr)
			} else if test.reason != "" {
				body, err := httpEncode(httpCancellation{httpBinding: binding, Reason: test.reason}, httpControlBytes)
				if err != nil {
					t.Fatal(err)
				}
				reader = strings.NewReader(string(body))
			}
			transport := &httpExecutionClient{}
			err := transport.watch(ctx, &http.Response{Body: io.NopCloser(reader)}, binding)
			if errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != test.unknown || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("intentional stop erased a real reply or became unknown: %v", err)
			}
			if test.readErr != nil && err != context.Canceled || test.reason == "canceled" && err == context.Canceled {
				t.Fatalf("local request stop and parsed parent cancellation became indistinguishable: %v", err)
			}
		})
	}
}

func TestRunHTTPDeliversPaidFactsAfterCancellationWatchStopsAction(t *testing.T) {
	for _, name := range []string{"deadline", "canceled", "completed", "malformed", "local parent cancellation", "local action deadline"} {
		t.Run(name, func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			sealed := make(chan struct{})
			watchContexts := make(chan context.Context, 1)
			after := make(chan CallUsage, 1)
			outcome := make(chan httpOutcome, 1)
			var native, requests, mutationSequence atomic.Uint64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				kind := strings.TrimPrefix(r.URL.Path, "/")
				var sequence uint64
				switch kind {
				case "next":
					runtime := job.RuntimeBudget
					if name == "local action deadline" {
						runtime = time.Second
					}
					httpClientTestJSON(t, w, httpAssignment{Job: job, Expires: time.Now().Add(runtime).UnixNano()})
					return
				case "cancel":
					w.Header().Set("Content-Type", "application/json")
					watchContexts <- r.Context()
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					select {
					case <-started:
					case <-r.Context().Done():
						return
					}
					if name == "local parent cancellation" {
						<-r.Context().Done()
						return
					}
					if name == "local action deadline" {
						select {
						case <-sealed:
							_ = json.NewEncoder(w).Encode(httpCancellation{httpBinding: binding, Reason: "completed"})
						case <-r.Context().Done():
						}
						return
					}
					if name == "malformed" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprint(w, "{")
						return
					}
					httpClientTestJSON(t, w, httpCancellation{httpBinding: binding, Reason: name})
					return
				case "status":
					var value httpObservation
					if err := httpDecode(r.Body, httpControlBytes, &value); err != nil {
						t.Error(err)
					}
					sequence = value.Status.Sequence
				case "before", "after":
					var value httpCall
					if err := httpDecode(r.Body, httpControlBytes, &value); err != nil {
						t.Error(err)
					}
					sequence = value.Sequence
					if kind == "after" {
						if r.Context().Err() != nil {
							t.Error("paid After inherited the canceled action context")
						}
						if name == "local action deadline" && (<-watchContexts).Err() != nil {
							t.Error("header admission timeout also bounded the held watch body")
						}
						after <- value.Checkpoint.Call
					}
				case "outcome":
					var value httpOutcome
					if err := httpDecode(r.Body, httpOutcomeBytes, &value); err != nil {
						t.Error(err)
					}
					sequence = value.Sequence
					outcome <- value
					close(sealed)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				requests.Add(1)
				if kind != "status" && sequence != mutationSequence.Add(1) {
					t.Errorf("request sequence changed after cancellation: %d", sequence)
				}
				httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Sequence: sequence, Kind: kind})
			}))
			defer server.Close()
			tokens := usage.Tokens{Input: 19, CachedInput: 2, Output: 3}
			runner := &Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, _ modelRun) (aicli.Result, error) {
				if native.Add(1) != 1 {
					t.Error("canceled execution repeated native work")
					return aicli.Result{}, errors.New("repeated canceled native call")
				}
				close(started)
				if name == "local parent cancellation" {
					cancel()
				}
				<-ctx.Done()
				return aicli.Result{Tokens: tokens}, ctx.Err()
			}}
			err := RunHTTP(ctx, server.Client(), server.URL, job.Identity.ExecutorID, runner)
			unknown := name == "completed" || name == "malformed"
			wantCause := context.Canceled
			if name == "local action deadline" {
				wantCause = context.DeadlineExceeded
			}
			if !errors.Is(err, wantCause) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != unknown || native.Load() != 1 || requests.Load() != 5 || len(after) != 1 || len(outcome) != 1 {
				t.Fatalf("watch cancellation lost paid callbacks or changed policy: error=%v native=%d requests=%d after=%d outcome=%d", err, native.Load(), requests.Load(), len(after), len(outcome))
			}
			paid, returned := <-after, <-outcome
			if paid.Tokens != tokens || paid.Complete || len(returned.Result.Calls) != 1 || returned.Result.Calls[0] != paid || returned.Result.Answer != "" || returned.Failure == nil || returned.Failure.OutcomeUnknown != unknown {
				t.Fatalf("cancellation discarded raw paid receipt: paid=%+v outcome=%+v", paid, returned)
			}
			if (name == "deadline" || name == "local action deadline") && (!errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, context.DeadlineExceeded) || !returned.Failure.Deadline || !returned.Failure.Terminal) {
				t.Fatalf("parent deadline became ordinary canceled execution: error=%v outcome=%+v", err, returned)
			}
		})
	}
}

func TestHTTPClientSendsOneBoundFailureForUnrepresentableOutcome(t *testing.T) {
	for _, loseACK := range []bool{false, true} {
		t.Run(fmt.Sprintf("lose ACK=%t", loseACK), func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			var requests atomic.Int32
			outcomes := make(chan httpOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) != 1 || r.URL.Path != "/outcome" {
					t.Error("fallback resent an outcome or synthesized a callback")
				}
				var value httpOutcome
				if err := httpDecode(r.Body, httpOutcomeBytes, &value); err != nil {
					t.Error(err)
				}
				outcomes <- value
				w.Header().Set("Content-Type", "application/json")
				if !loseACK {
					httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Sequence: value.Sequence, Kind: "outcome"})
				}
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints := &httpClientCheckpoints{transport: &httpExecutionClient{client: server.Client(), base: base}, control: context.Background(), job: job, binding: binding}
			result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: strings.Repeat("x", maxOutputBytes+1), Calls: []CallUsage{{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 13}}}}
			err = checkpoints.outcome(result, errors.Join(monitor.ErrTerminalJob, processgroup.ErrScopeUnconfirmed))
			if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || requests.Load() != 1 || len(outcomes) != 1 || len(result.Answer) != maxOutputBytes+1 {
				t.Fatalf("fallback hid unknown outcome, retried, or mutated raw result: error=%v requests=%d", err, requests.Load())
			}
			got := <-outcomes
			if !got.Result.matchesJob(job) || got.Result.Answer != "" || len(got.Result.Calls) != 0 || got.Failure == nil || !got.Failure.Terminal || !got.Failure.ScopeUnconfirmed || !got.Failure.OutcomeUnknown {
				t.Fatalf("bounded fallback lost actual flags or invented success/receipts: %+v", got)
			}
		})
	}
}

func TestRunHTTPRejectsRedirectsWithoutMutatingClient(t *testing.T) {
	var redirects, originalPolicy atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Minute
	client.CheckRedirect = func(*http.Request, []*http.Request) error { originalPolicy.Add(1); return nil }
	err := RunHTTP(context.Background(), client, server.URL, "local", &Runner{runModel: forbidModel(t)})
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || redirects.Load() != 0 || originalPolicy.Load() != 0 || client.Timeout != time.Minute {
		t.Fatalf("redirect was followed or caller client was changed: error=%v redirects=%d policy=%d", err, redirects.Load(), originalPolicy.Load())
	}
	if err := client.CheckRedirect(nil, nil); err != nil || originalPolicy.Load() != 1 {
		t.Fatal("caller redirect policy was replaced")
	}
}

func httpClientTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
