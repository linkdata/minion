package action

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHostileHTTPResultValidationErrorRedactsProviderKey(t *testing.T) {
	const key = "fixture-codex-secret"
	for _, tc := range []struct {
		name    string
		critic  bool
		failed  bool
		encoded bool
	}{
		{name: "normal outcome"},
		{name: "failed outcome", failed: true},
		{name: "critic outcome", critic: true},
		{name: "encoded normal outcome", encoded: true},
		{name: "encoded failed outcome", failed: true, encoded: true},
		{name: "encoded critic outcome", critic: true, encoded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveFixFlowFixture(t, w, r) || serveIssueFixture(t, w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			var journal bytes.Buffer
			usageDir := filepath.Join(t.TempDir(), "usage")
			e := &Orchestrator{
				WorkRoot: t.TempDir(), Store: openState(t, time.Now()), Usage: &usage.Store{Dir: usageDir},
				BotID:  8,
				GitHub: testGitHub(server, "fixture-token"), BotLogin: "bot", Logger: log.New(&journal, "", 0),
				Runner:   Runner{Codex: config.CodexConfig{APIKey: key}},
				Commands: map[string]config.CommandMapping{"fix": {Action: "fix", Skills: []string{skills.BuiltinAntiPromptInjection}}},
			}
			if tc.critic {
				e.Adversarial = "claude"
			}
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				job.Source.BaseFiles = nil // The issue fix has no base; preserve that across the HTTP JSON round trip.
				admitFixtureAssignment(t, checkpoints, job)
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Safe response."}
				if tc.critic {
					result = privateReviewResult(job)
					for i := range result.Calls {
						result.Calls[i].Tokens = usage.Tokens{Input: int64(i + 1), Output: 1}
						result.Calls[i].Complete = true
					}
				} else {
					call := f.call(1).Checkpoint.Call
					call.Tokens, call.Complete = usage.Tokens{Input: 3, Output: 2}, true
					result.Calls = []CallUsage{call}
				}
				pathKey := key
				if tc.encoded {
					pathKey = strings.Replace(key, "secret", "&#x73;ecret", 1)
				}
				result.Changes = []workspace.Change{{Path: "../" + pathKey, Mode: workspace.Regular, Data: []byte("hostile\n")}}
				if err := httpResultShape(result); err != nil {
					t.Fatalf("hostile result failed HTTP bounds before parent validation: %v", err)
				}
				if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), pathKey) {
					t.Fatalf("fixture no longer reaches the key-bearing validation error: %v", err)
				}
				sequence := uint64(1)
				binding := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
				review := ReviewCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
				if tc.critic {
					f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
					sequence++
				}
				for i, call := range result.Calls {
					binding.Call = callMetadata(call)
					f.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
					sequence++
					binding.Call = call
					f.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
					sequence++
					if tc.critic {
						review.Calls = result.Calls[:i+1]
						if call.Round >= 2 {
							review.Refutations = result.Refutations
						}
						f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
						sequence++
					}
				}
				if tc.critic {
					review.CriticStopReason = result.CriticStopReason
					f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
					sequence++
				}
				outcome := f.outcome(sequence, nil)
				outcome.Result = result
				if tc.failed {
					outcome.Failure = newHTTPFailure(errors.New("worker stopped after returning evidence"))
				}
				status := http.StatusInternalServerError
				if tc.failed {
					status = http.StatusOK
				}
				ack := f.ack(t, "/outcome", outcome, status, sequence)
				if status != http.StatusOK && ack.Failure == nil {
					t.Fatal("hostile result was acknowledged")
				}
				got := f.result(t)
				if got.err == nil {
					t.Fatalf("HTTP outcome error changed: %v", got.err)
				}
				return got.result, got.err
			})
			request := usageJob()
			request.Source.UpdatedAt = time.Now()
			request.Command = command.Command{Name: "fix", Action: command.Fix}
			request.Repository.Actions = []string{"fix"}
			selectFixtureRequest(t, e, &request, false)
			err := e.Execute(t.Context(), request)
			want := "unsafe or duplicate changed path"
			if tc.encoded {
				want = "action error could not be safely displayed"
			}
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "&#x73;ecret") || writes.Load() != 0 {
				t.Fatalf("hostile result escaped error boundary or published: error=%v writes=%d", err, writes.Load())
			}
			log.New(&journal, "", 0).Printf("fix failed: %v", err)
			if strings.Contains(journal.String(), key) || strings.Contains(journal.String(), "&#x73;ecret") {
				t.Fatalf("hostile result escaped monitor-style log boundary: %s", &journal)
			}
			records, err := usage.ReadAll(usageDir)
			if err != nil || len(records) != 1 || records[0].Status != "failed" {
				t.Fatalf("failed result lost usage evidence: records=%+v error=%v", records, err)
			}
		})
	}
}
