package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func usageJob() monitor.Job {
	return monitor.Job{
		ID: strings.Repeat("a", 64), Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "Issue", Number: 7,
		Source: github.Item{Author: github.User{ID: 7, Login: "writer"}}, Command: command.Command{Action: command.Review},
	}
}

func TestUsageDoesNotCountCanceledDispatch(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tracked, finish, err := (&Orchestrator{Usage: store}).trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = tracked.Runner.aroundModel(ctx, modelRun{provider: "codex", role: roleMain}, func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("dispatched a canceled model call")
		return aicli.Result{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dispatch: %v", err)
	}
	if err := finish(err); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if got := records[0]; got.CodexCalls != 0 || got.ClaudeCalls != 0 || !got.UsageComplete || got.Tokens != (usage.Tokens{}) {
		t.Fatalf("counted an undispatched call: %+v", got)
	}
}

func TestUsageAccumulatesPartialResults(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	runErr := errors.New("model interrupted")
	var calls int
	e := &Orchestrator{
		Usage:      store,
		CodexModel: "test-model",
		Runner: Runner{
			runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
				calls++
				if calls == 2 {
					return aicli.Result{Tokens: usage.Tokens{Input: 10, CachedInput: 8, Output: 2}}, runErr
				}
				return aicli.Result{Answer: "answer", Tokens: usage.Tokens{Input: 100, CachedInput: 80, Output: 20}, UsageComplete: true}, nil
			},
		},
	}
	tracked, finish, err := e.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		answer, err := tracked.Runner.runAI(context.Background(), Job{Main: RoleSettings{Provider: "codex", Model: "test-model"}}, "dir", "prompt", false, "")
		if i == 2 {
			if !errors.Is(err, runErr) {
				t.Fatalf("lost model error: %v", err)
			}
		} else if err != nil || answer != "answer" {
			t.Fatalf("answer=%q err=%v", answer, err)
		}
	}
	if err := finish(runErr); !errors.Is(err, runErr) {
		t.Fatalf("lost action failure: %v", err)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	got := records[0]
	if got.CodexCalls != 3 || got.Status != "failed" || got.UsageComplete || got.Tokens != (usage.Tokens{Input: 210, CachedInput: 168, Output: 42}) {
		t.Fatalf("partial usage lost or marked complete: %+v", got)
	}
}

func TestUsageTracksBothModelRoles(t *testing.T) {
	codex := RoleSettings{Provider: "codex", Model: "codex-model", Effort: "high"}
	claude := RoleSettings{Provider: "claude", Model: "claude-model", Effort: "medium"}
	for _, roles := range [][2]RoleSettings{{codex, claude}, {claude, codex}} {
		main, critic := roles[0], roles[1]
		t.Run(main.Provider, func(t *testing.T) {
			store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			runErr := context.Canceled
			e := &Orchestrator{Usage: store, Main: main.Provider, Adversarial: critic.Provider,
				CodexModel: "codex-model", CodexEffort: "high", ClaudeModel: "claude-model", ClaudeEffort: "medium",
				Runner: Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					checkpoint, err := usage.ReadAll(store.Dir)
					if err != nil || len(checkpoint) != 1 || checkpoint[0].UsageComplete {
						t.Fatalf("call has no durable incomplete checkpoint: %+v, %v", checkpoint, err)
					}
					if request.provider == critic.Provider {
						return aicli.Result{Tokens: usage.Tokens{Input: 10, CachedInput: 8, Output: 2}}, runErr
					}
					return aicli.Result{Answer: "answer", Tokens: usage.Tokens{Input: 100, CachedInput: 80, Output: 20}, UsageComplete: true}, nil
				}}}
			tracked, finish, err := e.trackUsage(usageJob())
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []struct {
				role     string
				settings RoleSettings
			}{{"main", main}, {"critic", critic}, {"main", main}} {
				result, err := tracked.Runner.runOnce(context.Background(), modelRun{role: call.role, provider: call.settings.Provider, model: call.settings.Model, effort: call.settings.Effort, dir: "dir", prompt: "prompt"})
				if call.role == "critic" {
					if !errors.Is(err, runErr) {
						t.Fatalf("lost critic error: %v", err)
					}
				} else if err != nil || result.Answer != "answer" {
					t.Fatalf("main result=%+v err=%v", result, err)
				}
			}
			if err := finish(runErr); !errors.Is(err, runErr) {
				t.Fatalf("lost action error: %v", err)
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%+v err=%v", records, err)
			}
			got := records[0]
			if got.Provider != main.Provider || got.CriticProvider != critic.Provider || got.Model != main.Model || got.Effort != main.Effort || got.CriticModel != critic.Model || got.CriticEffort != critic.Effort || got.Status != "failed" || got.UsageComplete || got.Tokens != (usage.Tokens{Input: 210, CachedInput: 168, Output: 42}) {
				t.Fatalf("inaccurate mixed usage: %+v", got)
			}
			if main.Provider == "codex" && (got.CodexCalls != 2 || got.ClaudeCalls != 1) || main.Provider == "claude" && (got.ClaudeCalls != 2 || got.CodexCalls != 1) {
				t.Fatalf("wrong provider counts: %+v", got)
			}
		})
	}
}

func TestUsageAccumulatesCriticRevisions(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	var critiques int
	e := &Orchestrator{
		Usage:       store,
		Main:        "claude",
		Adversarial: "codex",
		Runner: Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			if request.provider == "claude" {
				return aicli.Result{Answer: proposalJSON("proposal", "Private evidence"), Tokens: usage.Tokens{Input: 100, Output: 20}, UsageComplete: true}, nil
			}
			critiques++
			answer := `{"approved":true,"feedback":""}`
			if critiques == 1 {
				answer = `{"approved":false,"feedback":"Check the error path."}`
			}
			return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
		}},
	}
	tracked, finish, err := e.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := tracked.Runner.runAI(context.Background(), Job{Main: RoleSettings{Provider: "claude"}, Critic: &RoleSettings{Provider: "codex"}, CriticMaxRounds: config.DefaultCriticMaxRounds}, "dir", "prompt", false, "")
	if err != nil || answer != "proposal" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	got := records[0]
	if got.CodexCalls != 3 || got.ClaudeCalls != 2 || got.Status != "succeeded" || !got.UsageComplete || got.Tokens != (usage.Tokens{Input: 230, Output: 46}) {
		t.Fatalf("revision usage not accumulated: %+v", got)
	}
}

func TestUsagePersistenceFailureStopsModelsOrPublication(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			for _, stage := range []string{"before call", "after call"} {
				t.Run(stage, func(t *testing.T) {
					root := t.TempDir()
					originalDir := filepath.Join(root, "usage")
					blocked := filepath.Join(root, "blocked")
					if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
						t.Fatal(err)
					}
					store := &usage.Store{Dir: originalDir}
					modelErr := errors.New("model error")
					var calls int
					run := func(context.Context, modelRun) (aicli.Result, error) {
						calls++
						store.Dir = blocked
						return aicli.Result{Tokens: usage.Tokens{Input: 10, Output: 2}}, modelErr
					}
					e := &Orchestrator{
						Usage:      store,
						Main:       provider,
						CodexModel: "test-model",
						Runner: Runner{
							runModel: run,
						},
					}
					tracked, finish, err := e.trackUsage(usageJob())
					if err != nil {
						t.Fatal(err)
					}
					if stage == "before call" {
						store.Dir = blocked
					}
					_, err = tracked.Runner.runAI(context.Background(), Job{Main: RoleSettings{Provider: provider}}, "dir", "prompt", false, "")
					if err == nil || !strings.Contains(err.Error(), "save action usage") {
						t.Fatalf("storage failure not returned: %v", err)
					}
					if errors.Is(err, monitor.ErrTerminalJob) != (stage == "after call") {
						t.Fatalf("storage failure classification at %s: %v", stage, err)
					}
					if stage == "before call" && calls != 0 {
						t.Fatal("model ran without durable checkpoint")
					}
					if stage == "after call" && (calls != 1 || !errors.Is(err, modelErr)) {
						t.Fatalf("model failure missing: calls=%d error=%v", calls, err)
					}
					if finalErr := finish(err); finalErr == nil || !strings.Contains(finalErr.Error(), "save action usage") {
						t.Fatalf("final save failure missing: %v", finalErr)
					}
					records, readErr := usage.ReadAll(originalDir)
					if readErr != nil || len(records) != 1 || records[0].Status != "running" {
						t.Fatalf("last durable checkpoint missing: records=%+v err=%v", records, readErr)
					}
					if stage == "after call" && records[0].UsageComplete {
						t.Fatal("interrupted usage claimed complete")
					}
				})
			}
		})
	}
}

func TestSuccessfulModelStopsAfterUsageSaveFailure(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &usage.Store{Dir: filepath.Join(root, "usage")}
	calls := 0
	e := Orchestrator{Usage: store, Runner: Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
		calls++
		store.Dir = blocked
		return aicli.Result{Answer: "completed paid work", Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
	}}}
	tracked, _, err := e.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := tracked.Runner.runAI(context.Background(), Job{Main: RoleSettings{Provider: "codex"}}, "dir", "prompt", false, "")
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "save action usage") || calls != 1 || answer != "" {
		t.Fatalf("paid work can retry or publish after failed accounting: calls=%d answer=%q err=%v", calls, answer, err)
	}
}

func TestExecuteRecordsNoCodexAndSkipsAcknowledged(t *testing.T) {
	for _, failInitialSave := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail initial save %t", failInitialSave), func(t *testing.T) {
			job := usageJob()
			job.Command.Name = "review"
			job.Repository.Actions = []string{"review"}
			installActionTestSkills(t, testReviewSkillURL)
			var comments []github.Comment
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"}}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					if _, err := fmt.Fprint(w, `{"title":"Request"}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
					if err := json.NewEncoder(w).Encode(comments); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					if len(comments) != 1 {
						t.Fatalf("provenance requested before completion reply: %+v", comments)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"body": comments[0].Body, "author": map[string]string{"login": "bot"}}}})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					var payload struct{ Body string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					comments = append(comments, github.Comment{NodeID: "completion-reply-1", Body: payload.Body, User: github.User{Login: "bot"}})
					w.WriteHeader(http.StatusCreated)
					if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			dir := filepath.Join(t.TempDir(), "usage")
			if failInitialSave {
				if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			e := &Orchestrator{
				GitHub:     client,
				BotLogin:   "bot",
				Usage:      &usage.Store{Dir: dir},
				Commands:   map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{testReviewSkillURL}}},
				CodexModel: "test-model",
				Runner: Runner{
					runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
						t.Fatal("review on an issue must not invoke Codex")
						return aicli.Result{}, nil
					},
				},
			}
			err := e.Execute(context.Background(), job)
			if failInitialSave {
				if err == nil || !strings.Contains(err.Error(), "save action usage") || len(comments) != 0 {
					t.Fatalf("action ran without initial usage record: err=%v comments=%+v", err, comments)
				}
				return
			}
			if err != nil || len(comments) != 1 {
				t.Fatalf("execute error=%v comments=%+v", err, comments)
			}
			if err := e.Execute(context.Background(), job); err != nil || len(comments) != 1 {
				t.Fatalf("dedupe error=%v comments=%+v", err, comments)
			}
			records, err := usage.ReadAll(dir)
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%+v err=%v", records, err)
			}
			record := records[0]
			if record.Status != "succeeded" || record.CodexCalls != 0 || record.Tokens.Total() != 0 || !record.UsageComplete || record.FinishedAt == nil {
				t.Fatalf("incorrect no-Codex record: %+v", record)
			}
		})
	}
}

func TestUsagePreservesWorkerModelSkillsAndContext(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	root := t.TempDir()
	argsPath, promptPath := filepath.Join(root, "args"), filepath.Join(root, "prompt")
	binary := filepath.Join(root, "codex")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
cat > %q
printf '%%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"No findings."}}' '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":1,"output_tokens":2}}'
`, argsPath, promptPath)
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := &usage.Store{Dir: filepath.Join(root, "usage")}
	executor := &Orchestrator{WorkRoot: filepath.Join(root, "work"), Usage: store,
		CodexModel: "stale-host-model",
		Runner: Runner{
			WorkRoot: filepath.Join(root, "work"),
			Codex:    config.CodexConfig{Model: "stale-host-model"},
		},
	}
	tracked, finish, err := executor.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	job := portableJob(command.Comment)
	job.Main.Model = "portable-model"
	job.Main.Skills = []string{testReviewSkillURL}
	result, err := tracked.Runner.Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "ponytail-review", "SKILL.md")
	if !strings.Contains(string(args), "portable-model") || strings.Contains(string(args), "stale-host-model") || !strings.Contains(string(args), ".minion-context-") || !strings.Contains(string(prompt), installed) {
		t.Fatalf("usage wrapper lost worker settings: args=%s prompt=%s", args, prompt)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].Model != "portable-model" || records[0].CodexCalls != 1 || records[0].Tokens.Total() != 5 || len(result.Calls) != 1 {
		t.Fatalf("usage counted dispatch incorrectly: records=%+v calls=%+v err=%v", records, result.Calls, err)
	}
}

func TestRunnerDoesNotRecordUndispatchedUsageCall(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &usage.Store{Dir: filepath.Join(root, "usage")}
	executor := &Orchestrator{WorkRoot: filepath.Join(root, "work"), Usage: store, Runner: Runner{WorkRoot: filepath.Join(root, "work"), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("model ran without durable checkpoint")
		return aicli.Result{}, nil
	}}}
	tracked, _, err := executor.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	store.Dir = blocked
	result, err := tracked.Runner.Run(context.Background(), portableJob(command.Comment))
	if err == nil || !strings.Contains(err.Error(), "save action usage") || len(result.Calls) != 0 {
		t.Fatalf("undispatched calls=%+v err=%v", result.Calls, err)
	}
}
