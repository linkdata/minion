package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunHTTPCollectsCodexUsageAfterParentDeadlineSignal(t *testing.T) {
	// The local action deadline and grace bounds are covered separately in http_client_test.go.
	installActionTestSkills(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	const id = "11111111-1111-4111-8111-000000000001"
	day := time.Now().UTC().Format("2006/01/02")
	rollout := filepath.Join(home, "sessions", filepath.FromSlash(day), "rollout-"+id+".jsonl")
	ready, calls := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
set -eu
cat >/dev/null
printf x >> ` + shellQuote(calls) + `
dir="$CODEX_HOME/sessions/` + day + `"
mkdir -p "$dir"
printf '%s\n' '{"type":"session_meta","payload":{"id":"` + id + `","session_id":"` + id + `","source":"exec"}}' '{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}' '{"type":"token_usage_record","payload":{"thread_id":"` + id + `","session_id":"` + id + `","turn_id":"turn-1","response_id":"response-1","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}}' '{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}' > "$dir/rollout-` + id + `.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"` + id + `"}'
printf ready > ` + shellQuote(ready) + `
exec sleep 30
`
	fakeOnPath(t, "codex", script)
	job := testJob(command.Comment)
	job.Main.Model = "fixture-codex"
	binding := httpJobBinding(job)
	started := make(chan struct{})
	after := make(chan CallUsage, 1)
	outcomes := make(chan httpOutcome, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := strings.TrimPrefix(r.URL.Path, "/")
		switch kind {
		case "next":
			httpClientTestJSON(t, w, httpAssignment{Job: job, StopGrace: 5 * time.Second})
		case "cancel":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-started:
				httpClientTestJSON(t, w, httpCancellation{httpBinding: binding, Reason: "deadline", StopRemaining: 5 * time.Second})
			case <-r.Context().Done():
			}
		case "before", "after":
			var call httpCall
			if err := httpDecode(r.Body, httpControlBytes, &call); err != nil {
				t.Error(err)
			}
			if kind == "after" {
				select {
				case after <- call.Checkpoint.Call:
				default:
					t.Error("duplicate AfterCall")
				}
			}
			httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Kind: kind, Sequence: call.Sequence})
		case "status":
			var status httpObservation
			if err := httpDecode(r.Body, httpControlBytes, &status); err != nil {
				// The deadline may cancel a best-effort Status upload mid-body.
				if r.Context().Err() == nil {
					t.Error(err)
				}
				return
			}
			httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Kind: kind, Sequence: status.Status.Sequence})
		case "outcome":
			var outcome httpOutcome
			if err := httpDecode(r.Body, httpOutcomeBytes, &outcome); err != nil {
				t.Error(err)
			}
			select {
			case outcomes <- outcome:
			default:
				t.Error("duplicate Outcome")
			}
			httpClientTestJSON(t, w, httpAcknowledgement{httpBinding: binding, Kind: kind, Sequence: outcome.Sequence})
		default:
			t.Errorf("unexpected worker request: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := *server.Client()
	client.Timeout = 0
	workRoot := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- RunHTTP(t.Context(), &client, server.URL, job.Identity.ExecutorID, &Runner{WorkRoot: workRoot})
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	startupDeadline := time.NewTimer(10 * time.Second)
	defer startupDeadline.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			close(started)
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("Codex exited before deadline: %v", err)
		case <-ticker.C:
		case <-startupDeadline.C:
			t.Fatal("Codex did not start")
		}
	}
	completionDeadline := time.NewTimer(10 * time.Second)
	defer completionDeadline.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) {
			t.Fatalf("deadline did not stop the model: %v", err)
		}
	case <-completionDeadline.C:
		t.Fatal("worker did not finish within the stop grace")
	}
	if len(after) != 1 || len(outcomes) != 1 {
		t.Fatalf("deadline lost paid receipt or final Result: after=%d outcome=%d", len(after), len(outcomes))
	}
	paid, outcome := <-after, <-outcomes
	if paid.Tokens != (usage.Tokens{Input: 3, Output: 2}) || paid.Complete || len(outcome.Result.Calls) != 1 || outcome.Result.Calls[0] != paid || outcome.Failure == nil {
		t.Fatalf("post-call collection lost known usage: paid=%+v outcome=%+v", paid, outcome)
	}
	for _, diagnostic := range outcome.Result.Diagnostics {
		if diagnostic.Code == "cleanup_failed" {
			t.Fatalf("cancellation became a cleanup failure: %+v", outcome.Result.Diagnostics)
		}
	}
	if got, err := os.ReadFile(calls); err != nil || string(got) != "x" {
		t.Fatalf("model ran again after stop: calls=%q error=%v", got, err)
	}
	if _, err := os.Lstat(rollout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified owned rollout remains after cleanup: %v", err)
	}
}
