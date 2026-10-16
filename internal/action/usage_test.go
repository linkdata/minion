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
	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func usageJob() monitor.Job {
	return monitor.Job{
		ID: strings.Repeat("a", 64), Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "Issue", Number: 7,
		Source: github.Item{Author: github.User{Login: "writer"}}, Command: command.Command{Action: command.Review},
	}
}

func TestUsageAccumulatesPartialResults(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	runErr := errors.New("model interrupted")
	var calls int
	e := &Executor{
		Usage: store, Codex: config.CodexConfig{Model: "test-model"},
		RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
			calls++
			if calls == 2 {
				return codexcli.Result{Tokens: usage.Tokens{Input: 10, CachedInput: 8, Output: 2}}, runErr
			}
			return codexcli.Result{Answer: "answer", Tokens: usage.Tokens{Input: 100, CachedInput: 80, Output: 20}, UsageComplete: true}, nil
		},
	}
	tracked, finish, err := e.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		answer, err := tracked.runAI(context.Background(), "dir", "prompt", false, "")
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
	for _, main := range []string{"codex", "claude"} {
		t.Run(main, func(t *testing.T) {
			store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			critic := "claude"
			if main == "claude" {
				critic = "codex"
			}
			runErr := context.Canceled
			runner := func(provider string) func(context.Context, string, string, bool, string) (aicli.Result, error) {
				return func(context.Context, string, string, bool, string) (aicli.Result, error) {
					checkpoint, err := usage.ReadAll(store.Dir)
					if err != nil || len(checkpoint) != 1 || checkpoint[0].UsageComplete {
						t.Fatalf("call has no durable incomplete checkpoint: %+v, %v", checkpoint, err)
					}
					if provider == critic {
						return aicli.Result{Tokens: usage.Tokens{Input: 10, CachedInput: 8, Output: 2}}, runErr
					}
					return aicli.Result{Answer: "answer", Tokens: usage.Tokens{Input: 100, CachedInput: 80, Output: 20}, UsageComplete: true}, nil
				}
			}
			e := &Executor{
				Usage: store, Main: main, Adversarial: critic,
				Codex:    config.CodexConfig{Model: "codex-model", ReasoningEffort: "high"},
				Claude:   config.ClaudeConfig{Effort: "medium"},
				RunCodex: runner("codex"), RunClaude: runner("claude"),
			}
			tracked, finish, err := e.trackUsage(usageJob())
			if err != nil {
				t.Fatal(err)
			}
			for _, provider := range []string{main, critic, main} {
				result, err := tracked.runOnce(context.Background(), modelRun{provider: provider, dir: "dir", prompt: "prompt"})
				if provider == critic {
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
			if got.Provider != main || got.CriticProvider != critic || got.Status != "failed" || got.UsageComplete || got.Tokens != (usage.Tokens{Input: 210, CachedInput: 168, Output: 42}) {
				t.Fatalf("inaccurate mixed usage: %+v", got)
			}
			if main == "codex" {
				if got.CodexCalls != 2 || got.ClaudeCalls != 1 || got.Model != "codex-model" || got.Effort != "high" || got.CriticModel != "default" || got.CriticEffort != "medium" {
					t.Fatalf("wrong Codex main metadata or counters: %+v", got)
				}
			} else if got.ClaudeCalls != 2 || got.CodexCalls != 1 || got.Model != "default" || got.Effort != "medium" || got.CriticModel != "codex-model" || got.CriticEffort != "high" {
				t.Fatalf("wrong Claude main metadata or counters: %+v", got)
			}
		})
	}
}

func TestUsageAccumulatesCriticRevisions(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	var critiques int
	e := &Executor{
		Usage: store, Main: "claude", Adversarial: "codex",
		RunClaude: func(context.Context, string, string, bool, string) (aicli.Result, error) {
			return aicli.Result{Answer: proposalJSON("proposal", "Private evidence"), Tokens: usage.Tokens{Input: 100, Output: 20}, UsageComplete: true}, nil
		},
		RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
			critiques++
			answer := `{"approved":true,"feedback":""}`
			if critiques == 1 {
				answer = `{"approved":false,"feedback":"Check the error path."}`
			}
			return codexcli.Result{Answer: answer, Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
		},
	}
	tracked, finish, err := e.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := tracked.runAI(context.Background(), "dir", "prompt", false, "")
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
	if got.CodexCalls != 2 || got.ClaudeCalls != 2 || got.Status != "succeeded" || !got.UsageComplete || got.Tokens != (usage.Tokens{Input: 220, Output: 44}) {
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
					run := func(context.Context, string, string, bool, string) (aicli.Result, error) {
						calls++
						store.Dir = blocked
						return aicli.Result{Tokens: usage.Tokens{Input: 10, Output: 2}}, modelErr
					}
					e := &Executor{
						Usage: store, Main: provider, Codex: config.CodexConfig{Model: "test-model"}, RunCodex: run, RunClaude: run,
					}
					tracked, finish, err := e.trackUsage(usageJob())
					if err != nil {
						t.Fatal(err)
					}
					if stage == "before call" {
						store.Dir = blocked
					}
					_, err = tracked.runAI(context.Background(), "dir", "prompt", false, "")
					if err == nil || !strings.Contains(err.Error(), "save action usage") {
						t.Fatalf("storage failure not returned: %v", err)
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
			e := &Executor{
				GitHub: client, BotLogin: "bot", Usage: &usage.Store{Dir: dir}, Codex: config.CodexConfig{Model: "test-model"},
				Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{testReviewSkillURL}}},
				RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
					t.Fatal("review on an issue must not invoke Codex")
					return codexcli.Result{}, nil
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
