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

func TestUsageDoesNotCountCanceledDispatch(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tracked := attemptFixture(&Orchestrator{Usage: store})
	finish, err := tracked.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = tracked.Runner.aroundModel(ctx, modelRun{provider: "codex", role: roleMain}, forbidModel(t))
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
			tracked := attemptFixture(e)
			finish, err := tracked.trackUsage(usageJob())
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

func TestUsageDefaultsUnspecifiedModelLabels(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tracked := attemptFixture(&Orchestrator{Usage: store, Main: "claude", Adversarial: "codex"})
	finish, err := tracked.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].Model != "default" || records[0].CriticModel != "default" {
		t.Fatalf("empty models lost their default usage labels: records=%+v err=%v", records, err)
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
			answer := approvedCritiqueJSON
			if critiques == 1 {
				answer = `{"approved":false,"has_blockers":true,"feedback":"Check the error path."}`
			}
			return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
		}},
	}
	tracked := attemptFixture(e)
	finish, err := tracked.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	answer, stopReason, err := tracked.Runner.runAI(context.Background(), Job{Main: RoleSettings{Provider: "claude"}, Critic: &RoleSettings{Provider: "codex"}, CriticMaxRounds: config.DefaultCriticMaxRounds}, "dir", "prompt", "", nil, &modelSessions{})
	if err != nil || answer != "proposal" || stopReason != "approved" {
		t.Fatalf("answer=%q stop=%q err=%v", answer, stopReason, err)
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
	modelErr := errors.New("model error")
	for _, tt := range []struct {
		name     string
		provider string
		before   bool
		modelErr error
		critic   bool
	}{
		{"codex main/before call", "codex", true, modelErr, false}, {"claude main/before call", "claude", true, modelErr, false},
		{"codex main/after failed call", "codex", false, modelErr, false}, {"claude main/after failed call", "claude", false, modelErr, false},
		{"codex main/after successful call", "codex", false, nil, false}, {"claude main/after successful call", "claude", false, nil, false},
		{"codex main/after critic call", "codex", false, nil, true}, {"claude main/after critic call", "claude", false, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			originalDir, blocked := filepath.Join(root, "usage"), filepath.Join(root, "blocked")
			if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			store := &usage.Store{Dir: originalDir}
			calls := 0
			tracked := attemptFixture(&Orchestrator{Usage: store, Main: tt.provider, Runner: Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if tt.critic && request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Candidate answer", ""), Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
				}
				store.Dir = blocked
				if tt.critic {
					return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"Blocking failure."}`, Tokens: usage.Tokens{Input: 11, Output: 3}, UsageComplete: true}, nil
				}
				return aicli.Result{Answer: "completed paid work", Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: tt.modelErr == nil}, tt.modelErr
			}}})
			finish, err := tracked.trackUsage(usageJob())
			if err != nil {
				t.Fatal(err)
			}
			if tt.before {
				store.Dir = blocked
			}
			job := testJob(command.Comment)
			job.Main.Provider = tt.provider
			if tt.critic {
				job = withClaudeCritic(job)
				if tt.provider == "claude" {
					job.Critic.Provider = "codex"
				}
			}
			tracked.Runner.WorkRoot = t.TempDir()
			result, err := tracked.Runner.Run(context.Background(), job)
			answer := result.Answer
			wantCalls := 1
			if tt.before {
				wantCalls = 0
			} else if tt.critic {
				wantCalls = 2
			}
			if err == nil || !strings.Contains(err.Error(), "save action usage") || errors.Is(err, monitor.ErrTerminalJob) == tt.before || calls != wantCalls || len(result.Calls) != wantCalls || answer != "" || (!tt.before && tt.modelErr != nil && !errors.Is(err, tt.modelErr)) {
				t.Fatalf("storage failure: calls=%d answer=%q error=%v", calls, answer, err)
			}
			if tt.critic {
				last := result.Calls[1]
				if !last.HasBlockers || last.Approved || last.NoBlockers || last.Tokens != (usage.Tokens{Input: 11, Output: 3}) || !last.Complete || result.validate(job, false) != nil {
					t.Fatalf("post-native save failure lost parsed critic evidence: %+v", result)
				}
			}
			if tt.before {
				store.Dir = originalDir
				if finalErr := finish(err); finalErr != err {
					t.Fatalf("final save changed the original action error: %v", finalErr)
				}
				records, readErr := usage.ReadAll(originalDir)
				if readErr != nil || len(records) != 1 || records[0].Status != "failed" || records[0].FinishedAt == nil || records[0].CodexCalls != 0 || records[0].ClaudeCalls != 0 || !records[0].UsageComplete || records[0].Tokens != (usage.Tokens{}) {
					t.Fatalf("undispatched reservation was not rolled back: records=%+v error=%v", records, readErr)
				}
				return
			}
			if finalErr := finish(err); finalErr == nil || !strings.Contains(finalErr.Error(), "save action usage") {
				t.Fatalf("final save failure missing: %v", finalErr)
			}
			records, readErr := usage.ReadAll(originalDir)
			if readErr != nil || len(records) != 1 || records[0].Status != "running" || (!tt.before && records[0].UsageComplete) {
				t.Fatalf("last durable checkpoint lost: records=%+v error=%v", records, readErr)
			}
			if tt.critic && (records[0].CodexCalls != 1 || records[0].ClaudeCalls != 1 || records[0].Tokens != (usage.Tokens{Input: 10, Output: 2})) {
				t.Fatalf("last pre-critic usage checkpoint changed: %+v", records[0])
			}
		})
	}
}

func TestExecuteRecordsNoModelAndSkipsAcknowledged(t *testing.T) {
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
						t.Errorf("provenance requested before completion reply: %+v", comments)
						http.Error(w, "early provenance", http.StatusInternalServerError)
						return
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
			client := testGitHub(server, "test-token")
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
					runModel: forbidModel(t),
				},
			}
			err := e.Execute(context.Background(), job)
			if failInitialSave {
				if err == nil || errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrPostModelPublication) || !strings.Contains(err.Error(), "save action usage") || len(comments) != 0 {
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
	t.Setenv("CODEX_HOME", t.TempDir())
	installActionTestSkills(t, testReviewSkillURL)
	root := t.TempDir()
	argsPath, promptPath := filepath.Join(root, "args"), filepath.Join(root, "prompt")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
cat > %q
printf '%%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"No findings."}}' '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":1,"output_tokens":2}}'
`, argsPath, promptPath)
	fakeOnPath(t, "codex", script)
	store := &usage.Store{Dir: filepath.Join(root, "usage")}
	work := t.TempDir()
	orchestrator := &Orchestrator{WorkRoot: work, Usage: store,
		CodexModel: "portable-model", CodexEffort: "high",
		Runner: Runner{
			WorkRoot: work,
			Codex:    config.CodexConfig{Model: "stale-host-model", ReasoningEffort: "low"},
		},
	}
	tracked := attemptFixture(orchestrator)
	finish, err := tracked.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	job.Main.Model, job.Main.Effort = "portable-model", "high"
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
	if !strings.Contains(string(args), ".minion-context-") || !strings.Contains(string(prompt), installed) || !strings.Contains(string(args), "--model\nportable-model\n") || !strings.Contains(string(args), `model_reasoning_effort="high"`) || strings.Contains(string(args), "stale-host-model") {
		t.Fatalf("usage wrapper lost worker context or skills: args=%s prompt=%s", args, prompt)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].Model != "portable-model" || records[0].CodexCalls != 1 || records[0].Tokens.Total() != 5 || len(result.Calls) != 1 {
		t.Fatalf("usage counted dispatch incorrectly: records=%+v calls=%+v err=%v", records, result.Calls, err)
	}
}
