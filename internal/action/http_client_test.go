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
	"github.com/linkdata/minion/internal/config"
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
		afterFailure    bool
		watchPipe       bool
	}{
		{name: "complete"},
		{name: "invalid signed counters", invalidCounters: true},
		{name: "intentional held watch stop", holdWatch: true},
		{name: "intentional held watch pipe close", holdWatch: true, watchPipe: true},
		{name: "lost prepare observation ACK", lostStatus: "prepare"},
		{name: "malformed upload observation ACK", lostStatus: "upload", malformedStatus: true},
		{name: "paid checkpoint refusal skips upload", afterFailure: true},
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
			var pipeReads atomic.Int32
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
				ack := httpAcknowledgement{httpBinding: binding, Sequence: requestSequence, Kind: kind}
				if kind == "after" && test.afterFailure {
					ack.Failure = newHTTPFailure(errors.Join(monitor.ErrTerminalJob, workroot.ErrUnsafe))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
				}
				httpClientTestJSON(t, w, ack)
			}))
			defer server.Close()
			client := *server.Client()
			transport := client.Transport
			client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err == nil && test.watchPipe && r.URL.Path == "/cancel" {
					response.Body = httpClientPipeBody{ReadCloser: response.Body, trigger: r.Context().Done(), reads: &pipeReads}
				}
				return response, err
			})
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
			err := RunHTTP(context.Background(), &client, server.URL, job.Identity.ExecutorID, runner)
			if test.invalidCounters || test.afterFailure {
				if !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("known invalid usage changed failure policy: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantEvents := []string{"next", "status/prepare", "before", "after", "status/upload", "outcome"}
			if test.invalidCounters || test.afterFailure {
				wantEvents = []string{"next", "status/prepare", "before", "after", "outcome"}
			}
			if test.watchPipe && pipeReads.Load() != 1 {
				t.Fatalf("held watch did not return the injected teardown pipe error: %d", pipeReads.Load())
			}
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
			if test.invalidCounters || test.afterFailure {
				if returned.Failure == nil || !returned.Failure.Terminal || returned.Failure.OutcomeUnknown || returned.Result.Answer != "" {
					t.Fatalf("invalid raw receipt was discarded or turned into success: %+v", returned)
				}
				if test.afterFailure && (!errors.Is(err, workroot.ErrUnsafe) || !returned.Failure.Unsafe) {
					t.Fatalf("checkpoint refusal lost its finite failure facts: error=%v outcome=%+v", err, returned)
				}
			} else if returned.Failure != nil || returned.Result.Validate(job) != nil || returned.Result.Answer != "Complete answer" {
				t.Fatalf("valid one-shot execution did not return its actual result: %+v", returned)
			}
		})
	}
}

func TestHTTPClientRequiresExactAcknowledgementAndNeverRetries(t *testing.T) {
	for _, name := range []string{"known failure", "HTTP 202", "HTTP 206", "empty", "malformed", "trailing", "unknown field", "foreign binding", "wrong kind", "wrong sequence", "success with failure", "content type", "encoding", "multiple encodings", "oversized"} {
		t.Run(name, func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				ack := httpAcknowledgement{httpBinding: binding, Sequence: 1, Kind: "before"}
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "HTTP 202":
					w.WriteHeader(http.StatusAccepted)
				case "HTTP 206":
					w.WriteHeader(http.StatusPartialContent)
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
			checkpoints := &httpClientCheckpoints{transport: &httpExecutionClient{client: server.Client(), base: base}, control: context.Background(), action: context.Background(), binding: binding}
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
	for _, name := range []string{"version", "executor", "HTTP 202", "HTTP 206", "expired", "expired outcome ACK lost", "missing expiry", "trailing JSON"} {
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
					w.Header().Set("Content-Type", "application/json")
					if name == "HTTP 202" {
						w.WriteHeader(http.StatusAccepted)
					} else if name == "HTTP 206" {
						w.WriteHeader(http.StatusPartialContent)
					}
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

func TestRunHTTPBoundsAssignmentBodyAfterHeadersAndAllowsLongPoll(t *testing.T) {
	for _, negative := range []bool{false, true} {
		t.Run(fmt.Sprintf("negative=%t", negative), func(t *testing.T) {
			t.Parallel()
			headers, drained := make(chan struct{}, 1), make(chan struct{})
			var requests, native atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) != 1 || r.URL.Path != "/next" {
					t.Errorf("incomplete assignment advanced or retried: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				defer close(drained)
				if !negative {
					// Waiting for assignment headers must outlive the body-only timeout.
					timer := time.NewTimer(httpControlTimeout + time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if negative {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_, _ = fmt.Fprint(w, `{"incomplete":`)
				w.(http.Flusher).Flush()
				headers <- struct{}{}
				<-r.Context().Done() // No EOF and no caller deadline can end the body read.
			}))
			client := *server.Client()
			client.Timeout = 0
			transport := client.Transport
			var requestDeadline atomic.Bool
			client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				if _, ok := r.Context().Deadline(); ok {
					requestDeadline.Store(true)
				}
				return transport.RoundTrip(r)
			})
			done := make(chan error, 1)
			finished := false
			defer func() {
				server.CloseClientConnections()
				server.Close()
				if !finished {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("worker did not drain after fixture cleanup")
					}
				}
			}()
			runner := &Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
				native.Add(1)
				return aicli.Result{}, errors.New("incomplete assignment reached native work")
			}}
			go func() { done <- RunHTTP(context.Background(), &client, server.URL, "local", runner) }()
			select {
			case <-headers:
			case err := <-done:
				finished = true
				t.Fatalf("worker bounded the intentional pre-header assignment wait: %v", err)
			case <-time.After(httpControlTimeout + 5*time.Second):
				t.Fatal("assignment headers did not arrive after the controlled long poll")
			}
			var err error
			select {
			case err = <-done:
				finished = true
			case <-time.After(httpControlTimeout + 5*time.Second):
				t.Fatal("assignment body outlived its 30-second transfer bound")
			}
			if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 1 || native.Load() != 0 || requestDeadline.Load() || client.Timeout != 0 {
				t.Fatalf("body timeout changed failure, long-poll context, or caller client: error=%v requests=%d native=%d deadline=%t timeout=%s", err, requests.Load(), native.Load(), requestDeadline.Load(), client.Timeout)
			}
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("timed-out assignment response did not drain")
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
	if errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(err, monitor.ErrRequestTransport) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || ctx.Err() != nil || len(watchEntered) != 1 || len(outcomes) != 1 {
		t.Fatalf("stalled watch outlived the accepted runtime or ran models: error=%v caller=%v watch=%d outcome=%d", err, ctx.Err(), len(watchEntered), len(outcomes))
	}
	outcome := <-outcomes
	if outcome.Failure == nil || outcome.Failure.OutcomeUnknown || outcome.Failure.RequestTransport || !outcome.Failure.Deadline || !outcome.Failure.Terminal || len(outcome.Result.Calls) != 0 || outcome.Result.Answer != "" {
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

func TestRunHTTPStalledStatusCannotConsumeOutcomeGrace(t *testing.T) {
	for _, phase := range []string{"prepare", "upload"} {
		t.Run(phase, func(t *testing.T) {
			job := testJob(command.Comment)
			job.RuntimeBudget = 2 * time.Second
			paid := make(chan CallUsage, 1)
			f := newHTTPServerTest(t, context.Background(), job, runnerExecutionCheckpoints{
				after: func(value CallCheckpoint) error { paid <- value.Call; return nil },
			}, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			stopped := make(chan error, 1)
			var observations, native atomic.Int32
			client := *f.server.Client()
			client.Timeout = 0 // The action context must end the stalled observation.
			transport := client.Transport
			client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/status" {
					sequence := observations.Add(1)
					if phase == "prepare" && sequence == 1 || phase == "upload" && sequence == 2 {
						close(entered)
						select {
						case <-r.Context().Done():
							stopped <- context.Cause(r.Context())
							return nil, r.Context().Err()
						case <-release:
							return nil, errors.New("fixture released a stuck Status request")
						}
					}
				}
				return transport.RoundTrip(r)
			})
			tokens := usage.Tokens{Input: 17, CachedInput: 3, Output: 5}
			runner := &Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				native.Add(1)
				return aicli.Result{Answer: "Completed before upload stalled", Tokens: tokens, UsageComplete: true}, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- RunHTTP(ctx, &client, f.server.URL, job.Identity.ExecutorID, runner) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("optional observation did not enter its stalled request")
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("optional Status consumed required outcome grace after action expiry")
			}
			got := f.result(t)
			wantCalls := 0
			if phase == "upload" {
				wantCalls = 1
			}
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || native.Load() != int32(wantCalls) || len(got.result.Calls) != wantCalls || len(paid) != wantCalls || len(stopped) != 1 {
				t.Fatalf("stalled observation lost known outcome or paid facts: worker=%v parent=%v result=%+v native=%d paid=%d stopped=%d", err, got.err, got.result, native.Load(), len(paid), len(stopped))
			}
			if cause := <-stopped; !errors.Is(cause, context.DeadlineExceeded) {
				t.Fatalf("observation did not inherit the accepted action expiry: %v", cause)
			}
			if wantCalls == 1 {
				if receipt := <-paid; receipt != got.result.Calls[0] || receipt.Tokens != tokens || !receipt.Complete || got.result.Answer != "Completed before upload stalled" {
					t.Fatalf("stalled upload discarded acknowledged paid facts: receipt=%+v result=%+v", receipt, got.result)
				}
			} else if got.result.Answer != "" {
				t.Fatalf("stalled preparation fabricated an answer: %+v", got.result)
			}
		})
	}
}

func TestRunHTTPOutcomeACKPreservesKnownReceiptAfterWatchFailure(t *testing.T) {
	for _, test := range []struct {
		name                string
		early               bool
		truncated           bool
		malformedACK        bool
		externalCause       bool
		duringOutcome       bool
		duringUpload        bool
		callerBeforeOutcome bool
		expiredAfterWatch   bool
		processFailure      bool
	}{
		{name: "malformed watch after ACK"},
		{name: "truncated watch after ACK", truncated: true},
		{name: "malformed watch during outcome", duringOutcome: true},
		{name: "watch failure stops Runner", early: true},
		{name: "malformed final ACK stays unknown", malformedACK: true},
		{name: "external caller cause after ACK", externalCause: true},
		{name: "malformed watch during upload Status", duringUpload: true},
		{name: "upload watch and malformed final ACK", duringUpload: true, malformedACK: true},
		{name: "watch first then caller cause before outcome", duringUpload: true, callerBeforeOutcome: true},
		{name: "watch first then caller cause after ACK", duringUpload: true, externalCause: true},
		{name: "accepted expiry after upload watch", duringUpload: true, expiredAfterWatch: true},
		{name: "process failure during upload watch", duringUpload: true, processFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Comment)
			if test.expiredAfterWatch {
				job.RuntimeBudget = 2 * time.Second
			}
			f := newExecutionAdapterFixture(t, job)
			store := f.startUsage()
			parent := newHTTPServerTest(t, context.Background(), f.job, f.sink, nil)
			started := make(chan struct{})
			outcomeEntered, watchClosed := make(chan struct{}), make(chan struct{})
			uploadEntered := make(chan struct{})
			ctx, cancel := context.WithCancelCause(context.Background())
			externalCause := errors.Join(processgroup.ErrScopeUnconfirmed, monitor.ErrExecutionOutcomeUnknown)
			var native, outcomes, faultReads, observations atomic.Int32
			client := *parent.server.Client()
			client.Timeout = 0
			underlying := client.Transport
			client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				if test.duringUpload && r.URL.Path == "/status" && observations.Add(1) == 2 {
					close(uploadEntered)
					<-r.Context().Done() // The watch must cancel the optional request before its cause is sampled.
					if test.callerBeforeOutcome {
						cancel(externalCause)
					}
					if test.expiredAfterWatch {
						<-parent.tx.budget.Done() // The real accepted expiry follows the watch's child cancellation.
					}
					return nil, r.Context().Err()
				}
				if r.URL.Path == "/outcome" {
					outcomes.Add(1)
					if test.duringOutcome {
						close(outcomeEntered)
						select {
						case <-watchClosed: // Decode and close the malformed watch before actual outcome delivery.
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
				}
				response, err := underlying.RoundTrip(r)
				if err == nil && r.URL.Path == "/cancel" {
					trigger := r.Context().Done() // RunHTTP stops the watch after its outcome exchange.
					if test.early {
						trigger = started
					} else if test.duringOutcome {
						trigger = outcomeEntered
					} else if test.duringUpload {
						trigger = uploadEntered
					}
					var reader io.Reader = strings.NewReader("{")
					if test.truncated {
						reader = iotest.ErrReader(io.ErrUnexpectedEOF)
					} else if test.processFailure {
						reader = iotest.ErrReader(externalCause)
					} else if test.externalCause && !test.duringUpload {
						reader = iotest.ErrReader(context.Canceled)
					}
					var closed chan struct{}
					if test.duringOutcome {
						closed = watchClosed
					}
					response.Body = httpClientGatedWatchBody{ReadCloser: response.Body, reader: reader, trigger: trigger, reads: &faultReads, closed: closed}
				}
				if err == nil && r.URL.Path == "/outcome" && test.externalCause {
					// post closes the actual ACK after decoding it, before RunHTTP stops its watch.
					response.Body = httpClientCancelOnCloseBody{ReadCloser: response.Body, cancel: cancel, cause: externalCause}
				}
				if err == nil && r.URL.Path == "/outcome" && test.malformedACK {
					_ = response.Body.Close()
					response.Body = io.NopCloser(strings.NewReader("{"))
				}
				return response, err
			})
			tokens := usage.Tokens{Input: 19, CachedInput: 2, Output: 3}
			runner := &Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, _ modelRun) (aicli.Result, error) {
				if native.Add(1) != 1 {
					return aicli.Result{}, errors.New("watch failure repeated native work")
				}
				close(started)
				if test.early {
					<-ctx.Done()
					return aicli.Result{Tokens: tokens}, ctx.Err()
				}
				return aicli.Result{Answer: "Known candidate", Tokens: tokens, UsageComplete: true}, nil
			}}
			done := make(chan error, 1)
			finished := false
			t.Cleanup(func() {
				cancel(context.Canceled)
				if !finished {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("worker did not drain after watch fixture cancellation")
					}
				}
			})
			go func() { done <- RunHTTP(ctx, &client, parent.server.URL, f.job.Identity.ExecutorID, runner) }()
			var workerErr error
			select {
			case workerErr = <-done:
				finished = true
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not drain its outcome and gated watch")
			}
			got := parent.result(t)
			callerFailure := test.externalCause || test.callerBeforeOutcome
			workerUnknown := test.early || test.malformedACK || callerFailure || test.processFailure
			parentUnknown := test.early || test.callerBeforeOutcome || test.processFailure
			workerFailed, parentFailed := workerUnknown || test.expiredAfterWatch, parentUnknown || test.expiredAfterWatch
			if errors.Is(workerErr, monitor.ErrExecutionOutcomeUnknown) != workerUnknown || (workerErr != nil) != workerFailed || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) != parentUnknown || (got.err != nil) != parentFailed || native.Load() != 1 || outcomes.Load() != 1 || faultReads.Load() == 0 || len(got.result.Calls) != 1 || test.duringUpload && observations.Load() != 2 {
				t.Fatalf("watch drain changed known receipt or erased earlier failure: worker=%v parent=%v result=%+v native=%d outcomes=%d reads=%d", workerErr, got.err, got.result, native.Load(), outcomes.Load(), faultReads.Load())
			}
			if callerFailure && (!errors.Is(workerErr, externalCause) || context.Cause(ctx) != externalCause) {
				t.Fatalf("known receipt erased original caller cause: worker=%v caller=%v", workerErr, context.Cause(ctx))
			}
			if errors.Is(workerErr, processgroup.ErrScopeUnconfirmed) != (callerFailure || test.processFailure) || errors.Is(got.err, processgroup.ErrScopeUnconfirmed) != (test.callerBeforeOutcome || test.processFailure) || test.processFailure && !errors.Is(workerErr, externalCause) {
				t.Fatalf("watch handling erased or invented process uncertainty: worker=%v parent=%v", workerErr, got.err)
			}
			if test.expiredAfterWatch && (!errors.Is(workerErr, monitor.ErrTerminalJob) || !errors.Is(workerErr, context.DeadlineExceeded) || !errors.Is(got.err, monitor.ErrTerminalJob) || !errors.Is(got.err, context.DeadlineExceeded)) {
				t.Fatalf("watch child cancellation masked accepted expiry: worker=%v parent=%v", workerErr, got.err)
			}
			call := got.result.Calls[0]
			if call.Tokens != tokens || call.Complete == test.early || !slices.Equal(f.sink.calls, got.result.Calls) || f.sink.pending != nil {
				t.Fatalf("watch fault lost acknowledged paid facts: call=%+v saved=%+v pending=%+v", call, f.sink.calls, f.sink.pending)
			}
			if test.early {
				if !errors.Is(workerErr, context.Canceled) || got.result.Answer != "" || !errors.Is(f.sink.finish(got.result, got.err), monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("watch error that stopped Runner became success: worker=%v parent=%v result=%+v", workerErr, got.err, got.result)
				}
			} else if got.result.Answer != "Known candidate" || got.result.Validate(f.job) != nil || (f.sink.finish(got.result, got.err) != nil) != parentFailed {
				t.Fatalf("known parent receipt was altered by independent watch failure: result=%+v parent=%v", got.result, got.err)
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 || records[0].Tokens != tokens || records[0].CodexCalls != 1 || records[0].ClaudeCalls != 0 || records[0].UsageComplete == test.early {
				t.Fatalf("watch failure lost exact durable accounting: records=%+v error=%v", records, err)
			}
		})
	}
}

type httpClientCancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
	cause  error
}

func (b httpClientCancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel(b.cause)
	return err
}

type httpClientTestTransport func(*http.Request) (*http.Response, error)

func (f httpClientTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type httpClientPipeBody struct {
	io.ReadCloser
	trigger <-chan struct{}
	reads   *atomic.Int32
}

func (b httpClientPipeBody) Read([]byte) (int, error) {
	<-b.trigger
	b.reads.Add(1)
	return 0, io.ErrClosedPipe
}

type httpClientGatedWatchBody struct {
	io.ReadCloser
	reader  io.Reader
	trigger <-chan struct{}
	reads   *atomic.Int32
	closed  chan struct{}
}

func (b httpClientGatedWatchBody) Read(p []byte) (int, error) {
	<-b.trigger
	b.reads.Add(1)
	return b.reader.Read(p)
}

func (b httpClientGatedWatchBody) Close() error {
	err := b.ReadCloser.Close()
	if b.closed != nil {
		close(b.closed)
	}
	return err
}

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
		{name: "local pipe close", readErr: io.ErrClosedPipe, want: context.Canceled},
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
	for _, name := range []string{"deadline", "canceled", "completed", "malformed", "pipe before outcome", "local parent cancellation", "local action deadline"} {
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
			var pipeReads atomic.Int32
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
					if name == "local parent cancellation" || name == "pipe before outcome" {
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
			client := *server.Client()
			transport := client.Transport
			client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err == nil && name == "pipe before outcome" && r.URL.Path == "/cancel" {
					response.Body = httpClientPipeBody{ReadCloser: response.Body, trigger: started, reads: &pipeReads}
				}
				return response, err
			})
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
			err := RunHTTP(ctx, &client, server.URL, job.Identity.ExecutorID, runner)
			unknown := name == "completed" || name == "malformed" || name == "pipe before outcome"
			wantCause := context.Canceled
			if name == "local action deadline" {
				wantCause = context.DeadlineExceeded
			}
			if !errors.Is(err, wantCause) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != unknown || native.Load() != 1 || requests.Load() != 4 || len(after) != 1 || len(outcome) != 1 {
				t.Fatalf("watch cancellation lost paid callbacks or changed policy: error=%v native=%d requests=%d after=%d outcome=%d", err, native.Load(), requests.Load(), len(after), len(outcome))
			}
			paid, returned := <-after, <-outcome
			if name == "pipe before outcome" && (!errors.Is(err, io.ErrClosedPipe) || pipeReads.Load() != 1) {
				t.Fatalf("watch failure observed before teardown was erased: error=%v pipe reads=%d", err, pipeReads.Load())
			}
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
	for _, test := range []struct {
		name       string
		edit       func(*Result)
		wantCalls  bool
		wantRefs   bool
		wantReason bool
	}{
		{name: "oversized answer", wantCalls: true, wantRefs: true, wantReason: true},
		{name: "unrepresentable calls", edit: func(r *Result) { r.Calls[0].Model = strings.Repeat("x", config.MaxModelSettingBytes+1) }, wantRefs: true, wantReason: true},
		{name: "unrepresentable refutations", edit: func(r *Result) { r.Refutations[0].Text = strings.Repeat("x", maxOutputBytes+1) }, wantCalls: true, wantReason: true},
		{name: "unrepresentable reason", edit: func(r *Result) { r.CriticStopReason = strings.Repeat("x", 33) }, wantCalls: true, wantRefs: true},
		{name: "foreign claims", edit: func(r *Result) { r.Identity.AttemptID = "foreign" }},
	} {
		for _, loseACK := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lose ACK=%t", test.name, loseACK), func(t *testing.T) {
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
				checkpoints := &httpClientCheckpoints{transport: &httpExecutionClient{client: server.Client(), base: base}, control: context.Background(), binding: binding}
				// Craft the invalid primary field at the wire boundary, not as a Runner response.
				result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: strings.Repeat("x", maxOutputBytes+1),
					Calls:       []CallUsage{{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 13, CachedInput: -2}, Approved: true, HasBlockers: true}},
					Refutations: []Refutation{{Round: 2, Text: "Private raw refutation\x00\nkept exactly."}}, CriticStopReason: "round_cap"}
				if test.edit != nil {
					test.edit(&result)
				}
				originalCalls, originalRefs := slices.Clone(result.Calls), slices.Clone(result.Refutations)
				originalIdentity, originalReason := result.Identity, result.CriticStopReason
				err = checkpoints.outcome(result, errors.Join(monitor.ErrTerminalJob, processgroup.ErrScopeUnconfirmed))
				if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || requests.Load() != 1 || len(outcomes) != 1 || len(result.Answer) != maxOutputBytes+1 || result.Identity != originalIdentity || result.CriticStopReason != originalReason || !slices.Equal(result.Calls, originalCalls) || !slices.Equal(result.Refutations, originalRefs) {
					t.Fatalf("fallback hid unknown outcome, retried, or mutated raw result: error=%v requests=%d", err, requests.Load())
				}
				got := <-outcomes
				var wantCalls []CallUsage
				var wantRefs []Refutation
				var wantReason string
				if test.wantCalls {
					wantCalls = originalCalls
				}
				if test.wantRefs {
					wantRefs = originalRefs
				}
				if test.wantReason {
					wantReason = originalReason
				}
				if !got.Result.matchesJob(job) || got.Result.Answer != "" || len(got.Result.Changes)+len(got.Result.Artifacts)+len(got.Result.Diagnostics) != 0 || !slices.Equal(got.Result.Calls, wantCalls) || !slices.Equal(got.Result.Refutations, wantRefs) || got.Result.CriticStopReason != wantReason || got.Failure == nil || !got.Failure.Terminal || !got.Failure.ScopeUnconfirmed || !got.Failure.OutcomeUnknown {
					t.Fatalf("bounded fallback lost actual flags or invented success/receipts: %+v", got)
				}
			})
		}
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
