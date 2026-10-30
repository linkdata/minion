package action

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

type executorFunc func(context.Context, Job, Checkpoints) (Result, error)

func (f executorFunc) Execute(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
	return f(ctx, job, checkpoints)
}

func TestPerformPassesSelectedFixDestinationToExecutor(t *testing.T) {
	gitFixtureRemote(t)
	installActionTestSkills(t, testFixSkillURL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveFixFlowFixture(t, w, r) {
			t.Errorf("unexpected fix preparation request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	request := usageJob()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	request.Repository.Actions = []string{"fix"}
	const attemptID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	accepted := acceptedForJob(Job{Identity: Identity{
		JobID: request.ID, AttemptID: attemptID, ExecutorID: "remote",
		Repository: request.Repository.Name, RepositoryID: request.Repository.ID, OwnerID: request.Repository.OwnerID,
		SubjectType: request.SubjectType, Number: request.Number,
		Requester: request.Source.Author.Login, RequesterID: request.Source.Author.ID,
	}, Command: "fix", Action: command.Fix})
	request.AttemptID = attemptID
	request.Source.Key, request.Source.NodeID = accepted.SourceKey, accepted.SourceNodeID
	request.Source.UpdatedAt = accepted.SourceUpdatedAt
	request.AcceptedAt, request.Deadline = accepted.AcceptedAt, accepted.Deadline
	store := openState(t, accepted.AcceptedAt)
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	var got FixDestination
	called := false
	stop := errors.New("executor stopped after assignment")
	e := &Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(server, "fixture"), BotLogin: "bot", Store: store, ExecutorID: "remote",
		Main: "codex", Commands: map[string]config.CommandMapping{"fix": {Action: "fix", Skills: []string{testFixSkillURL}}},
		assignmentRequired: true,
		ValidateAssignment: func(context.Context, monitor.Job) error { return nil },
		Executor: executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
			called = true
			if job.Identity.ExecutorID != "remote" {
				t.Fatalf("selected executor identity = %q; want remote", job.Identity.ExecutorID)
			}
			sink, ok := checkpoints.(*executionCheckpoints)
			if !ok || !sink.AssignmentRequired() {
				t.Fatalf("selected executor did not receive assignment checkpoints: %T", checkpoints)
			}
			if err := sink.BeforeAssignment(job); err != nil {
				t.Fatal(err)
			}
			if sink.fixDestination == nil {
				t.Fatal("selected fix destination was not passed to executor")
			}
			got = *sink.fixDestination
			return Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}, stop
		}),
	}
	e.attemptID = attemptID
	if err := e.perform(testActionContext(t), request); !errors.Is(err, stop) {
		t.Fatalf("executor was not reached: %v", err)
	}
	want := FixDestination{RepositoryID: 1, DefaultBranch: "main", Visibility: "public", ForkID: 4, ForkFullName: "bot/r"}
	if !called || got != want {
		t.Fatalf("selected fix destination = %+v; want %+v (executor called=%t)", got, want, called)
	}
}

func TestSelectedExecuteRecordsPublicationBeforeMonitorCleanup(t *testing.T) {
	gitFixtureRemote(t)
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	request.Repository.Actions = []string{"comment"}
	request.AttemptID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	accepted := acceptedForJob(Job{Identity: Identity{
		JobID: request.ID, AttemptID: request.AttemptID, ExecutorID: LocalExecutorID,
		Repository: request.Repository.Name, RepositoryID: request.Repository.ID, OwnerID: request.Repository.OwnerID,
		SubjectType: request.SubjectType, Number: request.Number,
		Requester: request.Source.Author.Login, RequesterID: request.Source.Author.ID,
	}, Command: request.Command.Name, Action: command.Comment})
	request.Source.Key, request.Source.NodeID = accepted.SourceKey, accepted.SourceNodeID
	request.Source.UpdatedAt = accepted.SourceUpdatedAt
	request.AcceptedAt, request.Deadline = accepted.AcceptedAt, accepted.Deadline
	store := openState(t, accepted.AcceptedAt)
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var reply string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveIssueFixture(t, w, r) && !captureComment(t, w, r, &reply) {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	e := &Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(server, "fixture"), BotLogin: "bot", BotID: 8, Store: store,
		Commands:            map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		assignmentRequired:  true,
		ValidateAssignment:  func(context.Context, monitor.Job) error { return nil },
		ValidatePublication: func(context.Context, monitor.Job, bool) error { return nil },
		Executor: executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
			sink := checkpoints.(*executionCheckpoints)
			if err := sink.BeforeAssignment(job); err != nil {
				return Result{}, err
			}
			call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Model: job.Main.Model, Effort: job.Main.Effort}
			fact := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Ordinal: 1, Call: call}
			if err := sink.BeforeCall(fact); err != nil {
				return Result{}, err
			}
			call.Complete = true
			fact.Call = call
			if err := sink.AfterCall(fact); err != nil {
				return Result{}, err
			}
			result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head,
				Calls: []CallUsage{call}, Answer: "Published answer"}
			if err := sink.ReceiveCompletedResult(result); err != nil {
				return Result{}, err
			}
			return result, nil
		}),
	}
	if err := e.Execute(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Published answer") || store.Data.TerminalDecisions[request.ID].Outcome != "succeeded" ||
		store.Data.Accepted == nil || !store.Data.Accepted.Terminal || !store.Data.Accepted.ResultReady {
		t.Fatalf("selected publication was not terminal before monitor cleanup: reply=%q state=%+v", reply, store.Data)
	}
}

func TestFailedExecutorRetainsOnlyCheckpointCapturedReviewEvidence(t *testing.T) {
	for _, mode := range []string{"omitted", "unacknowledged suffix", "changed refutation", "unacknowledged decision", "failed After", "acknowledged decision"} {
		t.Run(mode, func(t *testing.T) {
			gitFixtureRemote(t)
			var reply string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !serveIssueFixture(t, w, r) && !captureComment(t, w, r, &reply) {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var journal bytes.Buffer
			work := t.TempDir()
			store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			var want Result
			executorErr := errors.New("executor stopped")
			e := &Orchestrator{WorkRoot: work, Usage: store, BotID: 8, BotLogin: "bot", Main: "codex", Adversarial: "claude", GitHub: testGitHub(server, "fixture"), Logger: log.New(&journal, "", 0),
				Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Executor: executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
					admitFixtureAssignment(t, checkpoints, job)
					result := privateReviewResult(job)
					want = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
					if mode == "omitted" {
						return result, executorErr
					}
					review := ReviewCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
					if err := checkpoints.Review(review); err != nil {
						t.Fatal(err)
					}
					for i, call := range result.Calls {
						fact := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Ordinal: i + 1, Call: callMetadata(call)}
						if err := checkpoints.BeforeCall(fact); err != nil {
							t.Fatal(err)
						}
						fact.Call = call
						if mode == "failed After" && i == len(result.Calls)-1 {
							blocked := filepath.Join(t.TempDir(), "blocked")
							if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
								t.Fatal(err)
							}
							store.Dir = blocked
						}
						afterErr := checkpoints.AfterCall(fact)
						want.Calls = append(want.Calls, call)
						if mode == "failed After" && i == len(result.Calls)-1 {
							if !errors.Is(afterErr, monitor.ErrTerminalJob) {
								t.Fatalf("fixture did not fail post-save: %v", afterErr)
							}
							return result, errors.Join(executorErr, afterErr)
						}
						if afterErr != nil {
							t.Fatal(afterErr)
						}
						review.Calls = result.Calls[:i+1]
						if call.Round >= 2 {
							review.Refutations = result.Refutations
						}
						if err := checkpoints.Review(review); err != nil {
							t.Fatal(err)
						}
						want.Refutations = slices.Clone(review.Refutations)
						if mode == "unacknowledged suffix" {
							return result, executorErr
						}
					}
					if mode == "changed refutation" {
						result.Refutations[0].Text = "FORGED_PRIVATE_REFUTATION"
					}
					if mode == "acknowledged decision" {
						review.CriticStopReason = result.CriticStopReason
						if err := checkpoints.Review(review); err != nil {
							t.Fatal(err)
						}
						want.CriticStopReason = result.CriticStopReason
					}
					return result, executorErr
				}),
			}
			request := usageJob()
			request.Command = command.Command{Name: "comment", Action: command.Comment}
			request.Repository.Actions = []string{"comment"}
			selectFixtureRequest(t, e, &request, false)
			if err := e.Execute(t.Context(), request); !errors.Is(err, executorErr) || reply != "" {
				t.Fatalf("failed executor published or lost error: reply=%q error=%v", reply, err)
			}
			if len(want.Calls) == 0 {
				if paths, err := filepath.Glob(filepath.Join(work, "reviews", "*.json")); err != nil || len(paths) != 0 {
					t.Fatalf("unacknowledged result created private evidence: paths=%v error=%v", paths, err)
				}
			} else {
				saved := readPrivateReview(t, work)
				if !slices.Equal(saved.Calls, want.Calls) || !slices.Equal(saved.Refutations, want.Refutations) || saved.CriticStopReason != want.CriticStopReason {
					t.Fatalf("rejected result changed captured evidence: saved=%+v want=%+v", saved, want)
				}
			}
			if strings.Count(journal.String(), "action call ") != len(want.Calls) || strings.Contains(journal.String(), "policy_met=true") != (want.CriticStopReason == "approved") {
				t.Fatalf("journal reported unacknowledged calls/decision: %s", &journal)
			}
		})
	}
}

func TestOrchestratorRequiresExecutorAcknowledgementsBeforePublication(t *testing.T) {
	for _, mode := range []string{"complete", "omitted", "ignored refusal"} {
		t.Run(mode, func(t *testing.T) {
			gitFixtureRemote(t)
			var reply string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !serveIssueFixture(t, w, r) && !captureComment(t, w, r, &reply) {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			blocked := filepath.Join(t.TempDir(), "blocked")
			if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			var refusal error
			executorErr := errors.New("executor stopped")
			e := &Orchestrator{WorkRoot: t.TempDir(), Usage: store, Store: openState(t, time.Now()), BotLogin: "bot", BotID: 8, GitHub: testGitHub(server, "fixture"),
				Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Executor: executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
					admitFixtureAssignment(t, checkpoints, job)
					call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Model: job.Main.Model, Effort: job.Main.Effort}
					fact := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Ordinal: 1, Call: call}
					if mode == "ignored refusal" {
						store.Dir = blocked
						refusal = checkpoints.BeforeCall(fact)
						if refusal == nil || errors.Is(refusal, monitor.ErrTerminalJob) {
							t.Fatalf("fixture did not produce ordinary pre-save failure: %v", refusal)
						}
					}
					if mode == "complete" {
						if err := checkpoints.BeforeCall(fact); err != nil {
							t.Fatal(err)
						}
					}
					call.Tokens, call.Complete = usage.Tokens{Input: 3, Output: 1}, true
					fact.Call = call
					if mode == "complete" {
						if err := checkpoints.AfterCall(fact); err != nil {
							t.Fatal(err)
						}
					}
					result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{call}, Answer: "Public answer"}
					if err := result.Validate(job); err != nil {
						t.Fatalf("fixture must return an otherwise valid complete result: %v", err)
					}
					if mode == "ignored refusal" {
						return result, executorErr
					}
					return result, nil
				}),
			}
			request := usageJob()
			request.Command = command.Command{Name: "comment", Action: command.Comment}
			request.Repository.Actions = []string{"comment"}
			selectFixtureRequest(t, e, &request, false)
			err := e.Execute(t.Context(), request)
			if mode == "complete" {
				if err != nil || !strings.Contains(reply, "Public answer") {
					t.Fatalf("acknowledged execution failed: reply=%q error=%v", reply, err)
				}
			} else if !errors.Is(err, monitor.ErrTerminalJob) || reply != "" {
				t.Fatalf("unacknowledged executor result published or became retryable: reply=%q error=%v", reply, err)
			}
			if mode == "ignored refusal" && (!errors.Is(err, refusal) || !errors.Is(err, executorErr)) {
				t.Fatalf("ignored failure lost original causes: %v", err)
			}
		})
	}
}
