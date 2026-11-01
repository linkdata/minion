package action

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
)

func TestPostPullOverdueWorkerLimitUsesFirstDeadline(t *testing.T) {
	started := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		hard   time.Duration
		idle   time.Duration
		ownCap bool
		want   workerLimit
	}{
		{name: "inactivity before runtime cap", hard: 7 * time.Second, idle: 5 * time.Second, ownCap: true, want: workerLimitInactivity},
		{name: "runtime cap before inactivity", hard: 5 * time.Second, idle: 7 * time.Second, ownCap: true, want: workerLimitRuntimeCap},
		{name: "runtime cap wins tie", hard: 5 * time.Second, idle: 5 * time.Second, ownCap: true, want: workerLimitRuntimeCap},
		{name: "caller deadline before inactivity", hard: 5 * time.Second, idle: 7 * time.Second, want: workerLimitNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := selectWorkerLimit(nil, started.Add(8*time.Second), started.Add(test.hard), started.Add(test.idle), test.ownCap)
			if got != test.want {
				t.Fatalf("post-pull limit = %v, want %v", got, test.want)
			}
		})
	}
}

func TestStoppedInactivityDoesNotFailOutcomeAfterIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		binding := httpJobBinding(job)
		result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		action, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		started := time.Now()
		idle := startInactivityTimer(started, time.Second, cancel)
		defer idle.stop()
		var received httpOutcome
		client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/outcome" {
				t.Fatalf("unexpected request: %s", r.URL.Path)
			}
			if err := httpDecode(r.Body, httpOutcomeBytes, &received); err != nil {
				t.Fatal(err)
			}
			data, err := httpEncode(httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: received.Sequence}, httpControlBytes)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(data))}, nil
		})}
		checkpoints := &httpClientCheckpoints{
			transport: &httpExecutionClient{client: client, base: &url.URL{Scheme: "http", Host: "worker.test"}},
			control:   t.Context(), action: action, inactivity: idle, hardDeadline: started.Add(5 * time.Second), ownRuntimeCap: true,
			binding: binding,
		}
		idle.stop()                 // Runner completed before inactivity; the later upload is not model work.
		time.Sleep(2 * time.Second) // Virtual clock crosses the old idle deadline.
		if action.Err() != nil {
			t.Fatalf("stopped timer canceled successful action: %v", context.Cause(action))
		}
		if err := checkpoints.outcome(result, nil); err != nil {
			t.Fatalf("successful outcome: %v", err)
		}
		if received.Failure != nil || !checkpoints.transport.outcomeAcknowledged {
			t.Fatalf("stopped timer changed successful outcome: failure=%+v acknowledged=%t",
				received.Failure, checkpoints.transport.outcomeAcknowledged)
		}
	})
}
