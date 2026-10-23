package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestExecuteRejectsHostileSuccessfulHTTPOutcomeBeforePublication(t *testing.T) {
	for _, scenario := range []struct {
		name, change string
		critic       bool
		validResult  bool
	}{
		{"unacknowledged call and foreign bindings", "missing call", false, true},
		{"missing model evidence", "no evidence", false, false},
		{"changed acknowledged usage", "usage", false, true},
		{"changed acknowledged refutation", "refutation", true, true},
		{"changed acknowledged decision", "decision", true, false},
		{"comment returns source changes", "changes", false, false},
		{"audit returns empty report", "audit", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.change == "audit" {
				installActionTestSkills(t, testAuditSkillURL)
			}
			gitFixtureRemote(t)
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveIssueFixture(t, w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			workRoot, usageDir := t.TempDir(), filepath.Join(t.TempDir(), "usage")
			var acknowledged Result
			var executed atomic.Bool
			name, action, skill := "comment", command.Comment, skills.BuiltinAntiPromptInjection
			if scenario.change == "audit" {
				name, action, skill = "security-audit", command.SecurityAudit, testAuditSkillURL
			}
			e := &Orchestrator{WorkRoot: workRoot, Usage: &usage.Store{Dir: usageDir}, GitHub: testGitHub(server, "token"), BotLogin: "bot",
				Commands: map[string]config.CommandMapping{name: {Action: string(action), Skills: []string{skill}}},
			}
			if scenario.critic {
				e.Adversarial = "claude"
			}
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				executed.Store(true)
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Forged publication"}
				if scenario.critic {
					result = privateReviewResult(job)
					for i := range result.Calls {
						result.Calls[i].Tokens = usage.Tokens{Input: int64(i + 1), Output: 1}
						result.Calls[i].Complete = true
					}
				} else {
					call := f.call(1).Checkpoint.Call
					call.Tokens, call.Complete = usage.Tokens{Input: 9, Output: 2}, true
					result.Calls = []CallUsage{call}
				}
				if scenario.change == "no evidence" {
					result.Calls = nil
				}
				sequence := uint64(1)
				binding := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
				review := ReviewCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
				if scenario.critic {
					f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
					sequence++
				}
				if scenario.change != "missing call" {
					for i, call := range result.Calls {
						binding.Call = callMetadata(call)
						f.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
						sequence++
						binding.Call = call
						f.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
						sequence++
						if scenario.critic {
							review.Calls = result.Calls[:i+1]
							if call.Round >= 2 {
								review.Refutations = result.Refutations
							}
							f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
							sequence++
						}
					}
					if scenario.critic {
						review.CriticStopReason = result.CriticStopReason
						f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
						sequence++
					}
				}
				acknowledged = result
				acknowledged.Calls = slices.Clone(result.Calls)
				acknowledged.Refutations = slices.Clone(result.Refutations)
				outcome := f.outcome(sequence, nil)
				outcome.Result = result
				switch scenario.change {
				case "missing call":
					foreign := outcome
					foreign.Result.Identity.AttemptID += "-foreign"
					if ack := f.ack(t, "/outcome", foreign, http.StatusConflict, sequence); ack.Failure == nil {
						t.Fatal("foreign outcome identity was accepted")
					}
					foreign = outcome
					foreign.Result.SourceRef = strings.Repeat("a", 40)
					if ack := f.ack(t, "/outcome", foreign, http.StatusConflict, sequence); ack.Failure == nil {
						t.Fatal("foreign outcome source was accepted")
					}
				case "usage":
					outcome.Result.Calls = slices.Clone(result.Calls)
					outcome.Result.Calls[0].Tokens.Input++
				case "refutation":
					outcome.Result.Refutations = slices.Clone(result.Refutations)
					outcome.Result.Refutations[0].Text = "FORGED_REFUTATION"
				case "decision":
					outcome.Result.CriticStopReason = ""
				case "changes":
					outcome.Result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("forged\n")}}
				case "audit":
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), "")
				}
				if err := httpResultShape(outcome.Result); err != nil {
					t.Fatalf("hostile result failed the wire shape before parent validation: %v", err)
				}
				validationErr := outcome.Result.Validate(job)
				if (validationErr == nil) != scenario.validResult || scenario.change == "audit" && !strings.Contains(validationErr.Error(), "REPORT.md") || scenario.change == "changes" && !strings.Contains(validationErr.Error(), "read-only action returned source changes") {
					t.Fatalf("hostile result semantic fixture changed: %v", validationErr)
				}
				f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
				got := f.result(t)
				if got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
					t.Fatalf("HTTP executor did not return the hostile result: got=%+v error=%v want=%+v", got.result, got.err, outcome.Result)
				}
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: name, Action: action}
			request.Repository.Actions = []string{name}
			if err := e.Execute(context.Background(), request); !errors.Is(err, monitor.ErrTerminalJob) || writes.Load() != 0 {
				t.Fatalf("hostile success published or lost terminal classification: error=%v writes=%d", err, writes.Load())
			}
			if !executed.Load() {
				t.Fatal("orchestrator rejected the request before invoking the HTTP executor")
			}
			records, err := usage.ReadAll(usageDir)
			if err != nil || len(records) != 1 || records[0].Status != "failed" {
				t.Fatalf("acknowledged usage not retained: records=%+v error=%v", records, err)
			}
			wantCalls, wantClaude, wantTokens := 1, 0, usage.Tokens{Input: 9, Output: 2}
			if scenario.change == "missing call" || scenario.change == "no evidence" {
				wantCalls, wantTokens = 0, usage.Tokens{}
			} else if scenario.critic {
				wantCalls, wantClaude, wantTokens = 2, 3, usage.Tokens{Input: 15, Output: 5}
			}
			origin, source, executor := "", "", ""
			if wantCalls > 0 {
				origin, source, executor = executorClaimOrigin, acknowledged.SourceRef, acknowledged.Identity.ExecutorID
			}
			if record := records[0]; record.CodexCalls != wantCalls || record.ClaudeCalls != wantClaude || record.Tokens != wantTokens || !record.UsageComplete || record.ExecutorID != executor || record.SourceRef != source || record.UsageSource != origin {
				t.Fatalf("hostile result replaced acknowledged usage: %+v", record)
			}
			if scenario.critic {
				saved := readPrivateReview(t, workRoot)
				if !slices.Equal(saved.Calls, acknowledged.Calls) || !slices.Equal(saved.Refutations, acknowledged.Refutations) || saved.CriticStopReason != acknowledged.CriticStopReason || saved.ReviewClaimOrigin != executorClaimOrigin {
					t.Fatalf("hostile result replaced acknowledged review: %+v", saved)
				}
			}
			if scenario.change == "audit" {
				if paths, err := filepath.Glob(filepath.Join(workRoot, "audits", "complete-*")); err != nil || len(paths) != 0 {
					t.Fatalf("hostile report became a complete audit: paths=%v error=%v", paths, err)
				}
				paths, err := filepath.Glob(filepath.Join(workRoot, "audits", "incomplete-*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("incomplete audit evidence missing: paths=%v error=%v", paths, err)
				}
				marker, err := os.ReadFile(filepath.Join(paths[0], "INCOMPLETE.txt"))
				if err != nil || !strings.Contains(string(marker), "must not be published") {
					t.Fatalf("incomplete audit marker=%q error=%v", marker, err)
				}
				report, err := os.ReadFile(filepath.Join(paths[0], "REPORT.md"))
				if err != nil || len(report) != 0 {
					t.Fatalf("private blank report=%q error=%v", report, err)
				}
			}
		})
	}
}
