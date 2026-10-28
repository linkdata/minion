package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
)

func TestOrchestratorAllowsQuotedRequestNotesButRejectsNewPrivateText(t *testing.T) {
	const quoted = "The requester already wrote this sentence in the issue."
	const novel = "This new private review sentence must stay internal."
	for _, tc := range []struct {
		name, published string
		wantWrite       bool
	}{
		{name: "existing requester text", published: quoted, wantWrite: true},
		{name: "new private text", published: novel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitFixtureRemote(t)
			var reply string
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveIssueFixture(t, w, r) {
					return
				}
				if r.Method == http.MethodPost {
					writes.Add(1)
				}
				if !captureComment(t, w, r, &reply) {
					t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			e := &Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(server, "token"), Store: openState(t, time.Now()), BotLogin: "bot", BotID: 8, Main: "codex", Adversarial: "claude",
				Commands: map[string]config.CommandMapping{"comment": {Action: string(command.Comment), Skills: []string{skills.BuiltinAntiPromptInjection}}},
			}
			e.Executor = executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				if job.Notes != quoted {
					t.Fatalf("requester notes did not reach prepared action: %q", job.Notes)
				}
				result := privateReviewResult(job)
				result.Refutations[0].Text = tc.published
				result.Answer = "Public reply: " + tc.published
				review := ReviewCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
				if err := checkpoints.Review(review); err != nil {
					t.Fatal(err)
				}
				for i, call := range result.Calls {
					call.Complete = true
					result.Calls[i] = call
					fact := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: callMetadata(call)}
					if err := checkpoints.BeforeCall(fact); err != nil {
						t.Fatal(err)
					}
					fact.Call = call
					if err := checkpoints.AfterCall(fact); err != nil {
						t.Fatal(err)
					}
					review.Calls = result.Calls[:i+1]
					if call.Round >= 2 {
						review.Refutations = result.Refutations
					}
					if err := checkpoints.Review(review); err != nil {
						t.Fatal(err)
					}
				}
				review.CriticStopReason = result.CriticStopReason
				if err := checkpoints.Review(review); err != nil {
					t.Fatal(err)
				}
				if err := result.Validate(job); err != nil {
					t.Fatalf("review result is invalid before publication: %v", err)
				}
				return result, nil
			})
			request := usageJob()
			request.Command = command.Command{Name: "comment", Action: command.Comment}
			request.Repository.Actions = []string{"comment"}
			request.Source.Body = "@bot comment\n" + quoted
			err := e.Execute(t.Context(), request)
			if tc.wantWrite {
				if err != nil || writes.Load() != 1 || !strings.Contains(reply, quoted) {
					t.Fatalf("public requester quote was not published: error=%v writes=%d reply=%q", err, writes.Load(), reply)
				}
			} else if !errors.Is(err, monitor.ErrTerminalJob) || writes.Load() != 0 {
				t.Fatalf("new private text crossed publication: error=%v writes=%d", err, writes.Load())
			}
		})
	}
}

func TestPublicationValidationStopsGitHubWrites(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, action := range []command.Action{command.Review, command.Comment} {
			name := string(action) + "/in-process"
			if selected {
				name = string(action) + "/selected"
			}
			t.Run(name, func(t *testing.T) {
				job := testJob(action)
				result := completedRecoveryResult(job)
				store, _, request := savedRecoveryFixture(t, job, result)
				if !selected {
					request.AttemptID = ""
				}
				var writes atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo" {
						_, _ = w.Write([]byte(`{"id":1,"full_name":"owner/repo","owner":{"id":2,"login":"owner"},"default_branch":"main"}`))
						return
					}
					if r.Method != http.MethodGet {
						writes.Add(1)
					}
					t.Errorf("unexpected GitHub request after publication rejection: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}))
				defer server.Close()
				denied := errors.New("requester no longer has permission")
				var checks int
				e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), WorkRoot: t.TempDir(), BotLogin: "bot",
					ValidatePublication: func(_ context.Context, got monitor.Job, requireAssigned bool) error {
						checks++
						if got.ID != request.ID || got.AttemptID != request.AttemptID || requireAssigned != selected {
							t.Errorf("publication checked the wrong reservation: job=%+v assigned=%t", got, requireAssigned)
						}
						return denied
					}}
				err := e.publish(t.Context(), &preparedAction{job: job, request: request}, result)
				if !errors.Is(err, denied) || checks != 1 || writes.Load() != 0 {
					t.Fatalf("rejected publication wrote to GitHub: err=%v checks=%d writes=%d", err, checks, writes.Load())
				}
			})
		}
	}
}

func TestInProcessPublicationAllowedRechecksAfterValidation(t *testing.T) {
	t.Run("missing selected attempt", func(t *testing.T) {
		e := &Orchestrator{assignmentRequired: true, ValidatePublication: func(context.Context, monitor.Job, bool) error {
			t.Fatal("malformed selected result reached in-process validation")
			return nil
		}}
		if err := e.publicationAllowed(t.Context(), monitor.Job{}, true); !errors.Is(err, monitor.ErrAcceptanceState) {
			t.Fatalf("selected result without an attempt was accepted: %v", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		e := &Orchestrator{ValidatePublication: func(context.Context, monitor.Job, bool) error {
			cancel()
			return nil
		}}
		if err := e.publicationAllowed(ctx, monitor.Job{}, false); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during validation was ignored: %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			job := monitor.Job{Deadline: time.Now().Add(time.Second)}
			e := &Orchestrator{ValidatePublication: func(context.Context, monitor.Job, bool) error {
				time.Sleep(2 * time.Second)
				return nil
			}}
			if err := e.publicationAllowed(t.Context(), job, false); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline during validation was ignored: %v", err)
			}
		})
	})
}
