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
			e := &Orchestrator{WorkRoot: t.TempDir(), Usage: store, BotLogin: "bot", GitHub: testGitHub(server, "fixture"),
				Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Executor: executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
					call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Model: job.Main.Model, Effort: job.Main.Effort}
					fact := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}
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
			err := e.Execute(context.Background(), request)
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
