package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
)

func TestRunOnceConsumesUnknownExecutionOutcome(t *testing.T) {
	networkErr := &url.Error{Op: "Post", URL: "http://executor.invalid/outcome", Err: io.EOF}
	canceledNetworkErr := &url.Error{Op: "Post", URL: "http://executor.invalid/after", Err: context.Canceled}
	for _, test := range []struct {
		name         string
		cause        error
		cancelParent bool
	}{
		{name: "unknown"},
		{name: "internal cancellation", cause: context.Canceled},
		{name: "parent cancellation", cause: context.Canceled, cancelParent: true},
		{name: "network failure", cause: networkErr},
		{name: "request transport marker", cause: ErrRequestTransport},
		{name: "request transport marker after parent cancellation", cause: ErrRequestTransport, cancelParent: true},
		{name: "network failure after parent cancellation", cause: networkErr, cancelParent: true},
		{name: "canceled network failure", cause: canceledNetworkErr, cancelParent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			initial := now.Add(-time.Minute)
			item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
			gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			statePath := filepath.Join(t.TempDir(), "state.json")
			store := openStateAt(t, statePath, initial)
			handler := &fakeHandler{executeErr: errors.Join(ErrExecutionOutcomeUnknown, test.cause)}
			handler.onExecute = func() {
				if len(store.Data.JobFailures) != 1 {
					t.Fatal("execution started without its saved attempt")
				}
				for _, failure := range store.Data.JobFailures {
					if failure.Attempts != 1 {
						t.Fatalf("unexpected attempt before execution: %+v", failure)
					}
				}
				if test.cancelParent {
					cancel()
				}
			}
			var logs bytes.Buffer
			m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Logger: log.New(&logs, "", 0), Now: func() time.Time { return now }}
			_, err := m.RunOnce(ctx, store)
			if test.cancelParent {
				if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrExecutionOutcomeUnknown) || !errors.Is(err, test.cause) {
					t.Fatalf("unknown outcome lost joined execution/cancellation causes: %v", err)
				}
				var network *url.Error
				if errors.As(test.cause, &network) && !errors.As(err, &network) {
					t.Fatalf("unknown outcome erased the typed network failure: %v", err)
				}
			} else if err != nil {
				t.Fatalf("acknowledged terminal failure did not finish normally: %v", err)
			}
			if len(handler.jobs) != 1 || len(gh.comments) != 1 || len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || !strings.Contains(logs.String(), "failed (attempt 3/3)") || strings.Contains(logs.String(), " interrupted: ") {
				t.Fatalf("unknown outcome was refunded, retried, or falsely latched: jobs=%d comments=%d logs=%q state=%+v", len(handler.jobs), len(gh.comments), logs.String(), store.Data)
			}
			if gh.comments[0].Body != failureReply+"\n\n"+handler.jobs[0].Marker() {
				t.Fatalf("unknown outcome changed the normal marked failure acknowledgement: %+v", gh.comments)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, statePath, initial)
			if len(reopened.Data.Processed) != 1 || len(reopened.Data.JobFailures) != 0 {
				t.Fatalf("unknown outcome disposition was not durable: %+v", reopened.Data)
			}
			handler.onExecute = func() { t.Fatal("unknown execution automatically repeated after restart") }
			m.Now = func() time.Time { return now.Add(time.Minute) }
			if _, err := m.RunOnce(context.Background(), reopened); err != nil || len(handler.jobs) != 1 || gh.commentCalls != 1 {
				t.Fatalf("processed unknown outcome reran or repeated its reply: error=%v jobs=%d replies=%d", err, len(handler.jobs), gh.commentCalls)
			}
		})
	}
}

func TestRunOnceUnknownOutcomeRetainsAttemptLimitUntilFailureReply(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
	replyErr := errors.New("failure reply unavailable")
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}, commentErr: replyErr}
	handler := &fakeHandler{executeErr: errors.Join(ErrExecutionOutcomeUnknown, &url.Error{Op: "Post", URL: "http://executor.invalid/outcome", Err: io.EOF})}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, statePath, initial)
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); !errors.Is(err, replyErr) || len(handler.jobs) != 1 {
		t.Fatalf("unknown outcome failure reply error lost: error=%v jobs=%d", err, len(handler.jobs))
	}
	id := handler.jobs[0].ID
	if failure := store.Data.JobFailures[id]; failure.Attempts != maxJobAttempts || failure.ReplyAttempts != 1 || len(store.Data.Processed) != 0 {
		t.Fatalf("unknown outcome retained repeatable work instead of only a failure reply: failure=%+v state=%+v", failure, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, statePath, initial)
	if failure := reopened.Data.JobFailures[id]; failure.Attempts != maxJobAttempts || failure.ReplyAttempts != 1 {
		t.Fatalf("unknown outcome attempt limit was not durable: %+v", failure)
	}
	handler.onExecute = func() { t.Fatal("failure acknowledgement retry repeated unknown execution") }
	gh.commentErr = nil
	retryAt := advanceToRetry(now, reopened)
	m.Now = func() time.Time { return retryAt }
	if _, err := m.RunOnce(context.Background(), reopened); err != nil || len(handler.jobs) != 1 || gh.commentCalls != 2 || len(gh.comments) != 1 || len(reopened.Data.Processed) != 1 || len(reopened.Data.JobFailures) != 0 {
		t.Fatalf("unknown outcome did not retry only its failure acknowledgement: error=%v jobs=%d replies=%d state=%+v", err, len(handler.jobs), gh.commentCalls, reopened.Data)
	}
}
