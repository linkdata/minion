package action

import (
	"context"
	"errors"
	"fmt"
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

func TestExecuteRejectsHostileHTTPOutcomeBeforePublication(t *testing.T) {
	for _, scenario := range []struct {
		name, change string
		critic       bool
		validResult  bool
	}{
		{"unacknowledged call and foreign bindings", "missing call", false, true},
		{"missing model evidence", "no evidence", false, false},
		{"invisible answer", "invisible answer", false, false},
		{"changed acknowledged usage", "usage", false, true},
		{"changed acknowledged refutation", "refutation", true, true},
		{"prefixed private sentence", "prefixed refutation", true, true},
		{"source scan budget cannot prove private sentence", "budget refutation", true, true},
		{"changed acknowledged decision", "decision", true, false},
		{"incomplete critic receipt forged approval", "incomplete critic", true, true},
		{"failed outcome after paid call", "failed outcome", false, true},
		{"comment returns source changes", "changes", false, false},
		{"audit returns empty report", "audit", false, false},
		{"audit returns empty coverage", "audit coverage", false, false},
		{"audit artifact traversal", "audit path traversal", false, false},
		{"audit artifact absolute", "audit path absolute", false, false},
		{"audit foreign repository", "audit foreign repo", false, false},
		{"audit absolute target", "audit target absolute", false, false},
		{"audit traversal companion", "audit companion traversal", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			isAudit := strings.HasPrefix(scenario.change, "audit")
			if isAudit {
				installActionTestSkills(t, testAuditSkillURL)
			}
			var budgetQuotes []string
			if scenario.change == "budget refutation" {
				for i := range 1040 {
					budgetQuotes = append(budgetQuotes, fmt.Sprintf("Private quote %04d was already in source.", i))
				}
				gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte(strings.Repeat("f", 1<<20) + "\n" + strings.Join(budgetQuotes, "\n"))})
			} else if isAudit {
				gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
			} else {
				gitFixtureRemote(t)
			}
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
			outside := filepath.Join(workRoot, "outside")
			if isAudit {
				if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var acknowledged Result
			executed := false
			name, action, skill := "comment", command.Comment, skills.BuiltinAntiPromptInjection
			if isAudit {
				name, action, skill = "security-audit", command.SecurityAudit, testAuditSkillURL
			}
			e := &Orchestrator{WorkRoot: workRoot, Usage: &usage.Store{Dir: usageDir}, GitHub: testGitHub(server, "token"), BotLogin: "bot",
				Commands: map[string]config.CommandMapping{name: {Action: string(action), Skills: []string{skill}}},
			}
			if scenario.critic {
				e.Adversarial = "claude"
			}
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				executed = true
				if scenario.change == "budget refutation" || isAudit {
					job.Source.BaseFiles = nil // Empty base files are omitted on the HTTP wire.
				}
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
				if scenario.change == "incomplete critic" {
					last := &result.Calls[len(result.Calls)-1]
					last.ReviewComplete, last.Approved, last.NoBlockers = false, false, false
					result.CriticStopReason = ""
				}
				if scenario.change == "prefixed refutation" {
					result.Refutations[0].Text = "The first private review sentence stays internal. The second private review sentence stays internal."
					result.Answer = "Public reply The second private review sentence stays internal."
				}
				if scenario.change == "budget refutation" {
					const novel = "This newly introduced private review sentence must not appear in a public reply."
					result.Refutations[0].Text = strings.Join(budgetQuotes, "\n") + "\n" + novel
					result.Answer = strings.Join(budgetQuotes, "\n--;\n") + "\n--;\n" + novel
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
					if scenario.critic && scenario.change != "incomplete critic" {
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
				case "incomplete critic":
					outcome.Result.Calls = slices.Clone(result.Calls)
					last := &outcome.Result.Calls[len(outcome.Result.Calls)-1]
					last.ReviewComplete, last.Approved, last.NoBlockers = true, true, true
					outcome.Result.CriticStopReason = "approved"
				case "failed outcome":
					outcome.Failure = newHTTPFailure(errors.New("worker stopped after paid call"))
				case "changes":
					outcome.Result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("forged\n")}}
				case "invisible answer":
					outcome.Result.Answer = "\u200b\u2060"
				case "audit":
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), "")
				case "audit coverage":
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
					for i := range outcome.Result.Artifacts {
						if outcome.Result.Artifacts[i].Path == "coverage-ledger.json" {
							outcome.Result.Artifacts[i].Data = []byte("[]")
						}
					}
				case "audit path traversal", "audit path absolute":
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
					path := "../../outside"
					if scenario.change == "audit path absolute" {
						path = outside
					}
					outcome.Result.Artifacts[0].Path = path
				case "audit foreign repo", "audit target absolute", "audit companion traversal":
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
					before, after := "", ""
					switch scenario.change {
					case "audit foreign repo":
						before, after = fmt.Sprintf(`"repo":%q`, job.Identity.Repository), `"repo":"foreign/repo"`
					case "audit target absolute":
						before, after = fmt.Sprintf(`"target":%q`, sourceIdentity(job)), fmt.Sprintf(`"target":%q`, outside)
					case "audit companion traversal":
						before, after = `"selected_companion_files":[]`, `"selected_companion_files":["../outside"]`
					}
					metadata := string(outcome.Result.Artifacts[0].Data)
					changed := strings.Replace(metadata, before, after, 1)
					if changed == metadata {
						t.Fatalf("audit metadata lacks %s field", scenario.change)
					}
					outcome.Result.Artifacts[0].Data = []byte(changed)
				}
				if err := httpResultShape(outcome.Result); err != nil {
					t.Fatalf("hostile result failed the wire shape before parent validation: %v", err)
				}
				validationErr := outcome.Result.Validate(job)
				if (validationErr == nil) != scenario.validResult || scenario.change == "audit" && !strings.Contains(validationErr.Error(), "REPORT.md") || scenario.change == "audit coverage" && !strings.Contains(validationErr.Error(), "coverage-ledger.json") || scenario.change == "changes" && !strings.Contains(validationErr.Error(), "read-only action returned source changes") || scenario.change == "invisible answer" && !strings.Contains(validationErr.Error(), "action answer") {
					t.Fatalf("hostile result semantic fixture changed: %v", validationErr)
				}
				wantPathError := ""
				switch scenario.change {
				case "audit path traversal", "audit path absolute":
					wantPathError = "invalid action report artifact"
				case "audit foreign repo", "audit target absolute":
					wantPathError = "complete standard run"
				case "audit companion traversal":
					wantPathError = "audit companion file path is not portable"
				}
				if wantPathError != "" && !strings.Contains(validationErr.Error(), wantPathError) {
					t.Fatalf("hostile result missed %s: %v", wantPathError, validationErr)
				}
				f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
				got := f.result(t)
				if (got.err != nil) != (scenario.change == "failed outcome") || !reflect.DeepEqual(got.result, outcome.Result) {
					t.Fatalf("HTTP executor did not return the hostile result: got=%+v error=%v want=%+v", got.result, got.err, outcome.Result)
				}
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: name, Action: action}
			request.Repository.Actions = []string{name}
			if err := e.Execute(t.Context(), request); err == nil || (scenario.change != "failed outcome" && !errors.Is(err, monitor.ErrTerminalJob)) || writes.Load() != 0 || (scenario.change == "budget refutation" && !strings.Contains(err.Error(), "publication contains private review evidence")) {
				t.Fatalf("hostile outcome published or lost failure classification: error=%v writes=%d", err, writes.Load())
			}
			if !executed {
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
			if isAudit {
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
				wantReport := "Forged publication"
				if scenario.change == "audit" {
					wantReport = ""
				}
				if err != nil || string(report) != wantReport {
					t.Fatalf("private report=%q error=%v want=%q", report, err, wantReport)
				}
				if data, err := os.ReadFile(outside); err != nil || string(data) != "untouched" {
					t.Fatalf("outside file changed: %q error=%v", data, err)
				}
			}
		})
	}
}
