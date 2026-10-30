package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

type httpDeadlineRecorder struct {
	*httptest.ResponseRecorder
	reads  []time.Time
	writes []time.Time
}

func (w *httpDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.reads = append(w.reads, deadline)
	return nil
}

func (w *httpDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.writes = append(w.writes, deadline)
	return nil
}

func checkHTTPDeadline(t *testing.T, deadline time.Time, before, after time.Time, want time.Duration) {
	t.Helper()
	if deadline.Before(before.Add(want)) || deadline.After(after.Add(want)) {
		t.Fatalf("deadline = %s, want %s after call", deadline, want)
	}
}

func TestHTTPTransferTimeoutUsesSameBoundForBothLargeRoutes(t *testing.T) {
	for _, test := range []struct {
		name string
		size int64
		want time.Duration
	}{
		{"control", httpControlBytes, 30 * time.Second},
		{"threshold", 30 * (64 << 10), 30 * time.Second},
		{"above threshold", 30*(64<<10) + 1, 31 * time.Second},
		{"outcome maximum", httpOutcomeBytes, 1024 * time.Second},
		{"assignment maximum", httpJobBytes, 4096 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := httpTransferTimeout(test.size); got != test.want {
				t.Fatalf("transfer timeout = %s, want %s", got, test.want)
			}
		})
	}
}

func TestHTTPServerTransferDeadlinesUseBodySizeAndStopBound(t *testing.T) {
	for _, test := range []struct {
		name   string
		length int64
		want   time.Duration
	}{
		{"small", httpControlBytes, httpControlTimeout},
		{"large outcome", httpOutcomeBytes, httpTransferTimeout(httpOutcomeBytes)},
		{"unknown outcome", -1, httpTransferTimeout(httpOutcomeBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/outcome", bytes.NewBufferString("{}"))
			request.Header.Set("Content-Type", "application/json")
			request.ContentLength = test.length
			writer := &httpDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			var value map[string]any
			before := time.Now()
			err := httpServerRead(writer, request, httpOutcomeBytes, &value)
			after := time.Now()
			if err != nil || len(writer.reads) != 2 || !writer.reads[1].IsZero() {
				t.Fatalf("server read deadline or decode: deadlines=%v error=%v", writer.reads, err)
			}
			checkHTTPDeadline(t, writer.reads[0], before, after, test.want)
		})
	}
	body := make([]byte, 2<<20)
	writer := &httpDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	err := httpServerWriteBytes(writer, http.StatusOK, body)
	after := time.Now()
	if err != nil || len(writer.writes) != 2 || !writer.writes[1].IsZero() {
		t.Fatalf("server write deadline: deadlines=%v error=%v", writer.writes, err)
	}
	checkHTTPDeadline(t, writer.writes[0], before, after, 32*time.Second)

	stopUntil := time.Now().Add(5 * time.Second)
	tx := &httpTransaction{stop: make(chan struct{}), stopUntil: stopUntil}
	writer = &httpDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := httpServerWriteBytes(writer, http.StatusOK, body, tx); err != nil {
		t.Fatal(err)
	}
	if !writer.writes[0].Equal(stopUntil) {
		t.Fatalf("large transfer extended the action stop deadline: got=%s want=%s", writer.writes[0], stopUntil)
	}
}

func TestHTTPServerRejectsOversizedDeclaredBodyBeforeReading(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/outcome", bytes.NewReader(nil))
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = httpOutcomeBytes + 1
	writer := &httpDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	var value map[string]any
	if err := httpServerRead(writer, request, httpOutcomeBytes, &value); !errors.Is(err, errHTTPBodyLimit) || len(writer.reads) != 0 {
		t.Fatalf("oversized header entered body read: deadlines=%v error=%v", writer.reads, err)
	}
}

type httpBlockedResponseBody struct{ ctx context.Context }

func (b httpBlockedResponseBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, context.Cause(b.ctx)
}

func (httpBlockedResponseBody) Close() error { return nil }

func TestHTTPClientAssignmentBodyTimerUsesContentLength(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 32 << 20
		base, err := url.Parse("http://example.test")
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				ContentLength: size, Body: httpBlockedResponseBody{request.Context()}, Request: request}, nil
		})}
		transport := &httpExecutionClient{client: client, base: base}
		finished := make(chan error, 1)
		started := time.Now()
		go func() {
			var assignment httpAssignment
			finished <- transport.post(t.Context(), "next", []byte(`{}`), httpJobBytes, &assignment, httpBinding{}, 0)
		}()
		synctest.Wait()
		time.Sleep(httpControlTimeout + time.Second)
		select {
		case err := <-finished:
			t.Fatalf("large assignment kept the old 30-second limit: %v", err)
		default:
		}
		time.Sleep(httpTransferTimeout(size) - httpControlTimeout - time.Second)
		if err := <-finished; !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("large assignment did not stop at its size bound: %v", err)
		}
		if elapsed := time.Since(started); elapsed != httpTransferTimeout(size) {
			t.Fatalf("large assignment timer elapsed %s, want %s", elapsed, httpTransferTimeout(size))
		}
	})
}

func TestHTTPClientLargeTransfersCompleteAfterThirtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 2 << 20 // 32-second transfer budget.
		job := testJob(command.Comment)
		assignmentBody, err := httpEncode(httpAssignment{Job: job, StopGrace: httpControlTimeout}, httpJobBytes)
		if err != nil {
			t.Fatal(err)
		}
		assignmentBody = append(assignmentBody, bytes.Repeat([]byte(" "), size-len(assignmentBody))...)
		outcomeBody, err := httpEncode(httpOutcome{Sequence: 1, Result: Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}}, httpOutcomeBytes)
		if err != nil {
			t.Fatal(err)
		}
		outcomeBody = append(outcomeBody, bytes.Repeat([]byte(" "), size-len(outcomeBody))...)
		ack, err := httpEncode(httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "outcome", Sequence: 1}, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		base, err := url.Parse("http://example.test")
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case "/next":
				reader, writer := io.Pipe()
				go func() {
					time.Sleep(httpControlTimeout + time.Second)
					_, _ = writer.Write(assignmentBody)
					_ = writer.Close()
				}()
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					ContentLength: size, Body: reader, Request: request}, nil
			case "/outcome":
				time.Sleep(httpControlTimeout + time.Second)
				if err := request.Context().Err(); err != nil {
					return nil, err
				}
				var got httpOutcome
				if err := httpDecode(request.Body, httpOutcomeBytes, &got); err != nil || got.Sequence != 1 {
					t.Errorf("delayed outcome upload: sequence=%d error=%v", got.Sequence, err)
				}
				time.Sleep(2 * time.Second) // The parent saves the received outcome before replying.
				if err := request.Context().Err(); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					ContentLength: int64(len(ack)), Body: io.NopCloser(bytes.NewReader(ack)), Request: request}, nil
			default:
				t.Errorf("unexpected route %s", request.URL.Path)
				return nil, errors.New("unexpected route")
			}
		})}
		transport := &httpExecutionClient{client: client, base: base}
		var assignment httpAssignment
		if err := transport.post(t.Context(), "next", []byte(`{}`), httpJobBytes, &assignment, httpBinding{}, 0); err != nil || assignment.Job.Identity != job.Identity {
			t.Fatalf("valid large assignment failed after 31 seconds: error=%v identity=%+v", err, assignment.Job.Identity)
		}
		checkpoints := &httpClientCheckpoints{transport: transport, control: t.Context(), binding: httpJobBinding(job)}
		if err := checkpoints.acknowledge(t.Context(), "outcome", outcomeBody, 1); err != nil {
			t.Fatalf("valid large outcome failed after 31 seconds: %v", err)
		}
	})
}

func TestRunHTTPRunnerReturnKeepsOutcomeTransferBudget(t *testing.T) {
	for _, test := range []struct {
		name      string
		modelErr  error
		wantError bool
	}{
		{name: "complete"},
		{name: "failed model", modelErr: errors.New("model failed"), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				job := testJob(command.Comment)
				binding := httpJobBinding(job)
				outcomes := make(chan httpOutcome, 1)
				response := func(request *http.Request, value any) (*http.Response, error) {
					data, err := httpEncode(value, httpJobBytes)
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)), Request: request}, nil
				}
				client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
					switch request.URL.Path {
					case "/next":
						return response(request, httpAssignment{Job: job, StopGrace: time.Second})
					case "/cancel":
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
							ContentLength: -1, Body: httpBlockedResponseBody{request.Context()}, Request: request}, nil
					case "/before", "/after":
						var call httpCall
						if err := httpDecode(request.Body, httpControlBytes, &call); err != nil {
							return nil, err
						}
						return response(request, httpAcknowledgement{httpBinding: binding, Sequence: call.Sequence, Kind: request.URL.Path[1:]})
					case "/status":
						var observation httpObservation
						if err := httpDecode(request.Body, httpControlBytes, &observation); err != nil {
							return nil, err
						}
						return response(request, httpAcknowledgement{httpBinding: binding, Sequence: observation.Status.Sequence, Kind: "status"})
					case "/outcome":
						var outcome httpOutcome
						if err := httpDecode(request.Body, httpOutcomeBytes, &outcome); err != nil {
							return nil, err
						}
						outcomes <- outcome
						// Parent result storage can outlast stop grace on a normal return.
						time.Sleep(2 * time.Second)
						if err := request.Context().Err(); err != nil {
							return nil, err
						}
						return response(request, httpAcknowledgement{httpBinding: binding, Sequence: outcome.Sequence, Kind: "outcome"})
					default:
						return nil, errors.New("unexpected HTTP route: " + request.URL.Path)
					}
				})}
				runner := &Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
					return aicli.Result{Answer: "Complete", UsageComplete: true}, test.modelErr
				}}
				err := RunHTTP(t.Context(), client, "http://example.test", job.Identity.ExecutorID, runner)
				if test.wantError {
					if !errors.Is(err, test.modelErr) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
						t.Fatalf("failed model result lost its outcome ACK: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				select {
				case outcome := <-outcomes:
					if (outcome.Failure != nil) != test.wantError {
						t.Fatalf("outcome failure = %+v, want failed=%t", outcome.Failure, test.wantError)
					}
				default:
					t.Fatal("worker did not upload an outcome")
				}
			})
		})
	}
}

func TestHTTPClientOutcomeUploadUsesSizeAndActionStop(t *testing.T) {
	base, err := url.Parse("http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	binding := httpBinding{Version: ProtocolVersion}
	ack, err := httpEncode(httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: 1}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
		want   time.Duration
	}{
		{"size", func() (context.Context, context.CancelFunc) { return context.WithCancel(t.Context()) }, 62 * time.Second},
		{"action stop", func() (context.Context, context.CancelFunc) { return context.WithTimeout(t.Context(), 5*time.Second) }, 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent, cancel := test.parent()
			defer cancel()
			var deadline time.Time
			client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
				deadline, _ = request.Context().Deadline()
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					ContentLength: int64(len(ack)), Body: io.NopCloser(bytes.NewReader(ack)), Request: request}, nil
			})}
			checkpoints := &httpClientCheckpoints{transport: &httpExecutionClient{client: client, base: base}, control: parent, binding: binding}
			before := time.Now()
			err := checkpoints.acknowledge(parent, "outcome", make([]byte, 2<<20), 1)
			after := time.Now()
			if err != nil || deadline.IsZero() {
				t.Fatalf("outcome upload did not use a request deadline: deadline=%s error=%v", deadline, err)
			}
			if test.name == "action stop" {
				stop, _ := parent.Deadline()
				if !deadline.Equal(stop) {
					t.Fatalf("outcome upload extended the action stop deadline: got=%s want=%s", deadline, stop)
				}
			} else {
				checkHTTPDeadline(t, deadline, before, after, test.want)
			}
		})
	}
}
