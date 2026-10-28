package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestRecoveredUsageCompletesOnlyMatchingExistingAttempt(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	job := usageJob()
	job.AttemptID = strings.Repeat("b", 32)
	started := time.Now().UTC().Add(-time.Hour)
	failedAt := started.Add(5 * time.Minute)
	prior := usage.Record{
		ID: job.AttemptID, JobID: job.ID, Repo: job.Repository.Name, User: job.Source.Author.Login,
		Action: string(job.Command.Action), Provider: "codex", Model: "default", StartedAt: started,
		FinishedAt: &failedAt, Status: "failed", ExecutorID: "local", SourceRef: strings.Repeat("a", 40),
		UsageSource: "executor_reported", CodexCalls: 1, Tokens: usage.Tokens{Input: 7, Output: 2}, UsageComplete: true,
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	var journal bytes.Buffer
	e := &Orchestrator{Usage: store, Logger: log.New(&journal, "", 0)}
	e.completeRecoveredUsage(job)
	got, found, err := store.Load(job.AttemptID)
	if err != nil || !found || got.FinishedAt == nil || got.FinishedAt.Before(failedAt) {
		t.Fatalf("recovered usage was not completed: record=%+v found=%t err=%v", got, found, err)
	}
	want := prior
	want.Status, want.FinishedAt = "succeeded", got.FinishedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recovery changed usage counters or provenance: got=%+v want=%+v", got, want)
	}
	e.completeRecoveredUsage(job)
	again, found, err := store.Load(job.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(got, again) {
		t.Fatalf("recovery changed completed usage twice: got=%+v again=%+v err=%v", got, again, err)
	}
	other := job
	other.AttemptID = strings.Repeat("c", 32)
	e.completeRecoveredUsage(other)
	other.AttemptID, other.ID = job.AttemptID, "different-job"
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	e.completeRecoveredUsage(other)
	unchanged, found, err := store.Load(job.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(unchanged, prior) || !strings.Contains(journal.String(), "belongs to another job") {
		t.Fatalf("mismatched recovery replaced usage or skipped warning: got=%+v err=%v log=%q", unchanged, err, journal.String())
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 {
		t.Fatalf("recovery invented a usage record: records=%+v err=%v", records, err)
	}
}

func TestRecoveredUsageValidationFailureOnlyLogs(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	job := usageJob()
	job.AttemptID = strings.Repeat("b", 32)
	prior := usage.Record{
		ID: job.AttemptID, JobID: job.ID, Repo: job.Repository.Name, User: job.Source.Author.Login,
		Action: string(job.Command.Action), Provider: "codex", Model: "default", StartedAt: time.Now().UTC().Add(-time.Hour),
		Status: "running", ExecutorID: "local", SourceRef: strings.Repeat("a", 40), CodexCalls: 1,
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	var journal bytes.Buffer
	e := &Orchestrator{Usage: store, Logger: log.New(&journal, "", 0)}
	// Completing a record with model calls but no usage source fails validation.
	e.completeRecoveredUsage(job)
	got, found, err := store.Load(job.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(got, prior) || !strings.Contains(journal.String(), "final recovered action usage checkpoint failed:") {
		t.Fatalf("usage save failure changed publication evidence or went unlogged: got=%+v err=%v log=%q", got, err, journal.String())
	}
}

func TestSelectedUsageStartsEachRetryOnItsOwnDayAndKeepsRecoveryStart(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tracked := attemptFixture(&Orchestrator{Usage: store})
	job := usageJob()
	job.AttemptID = tracked.attemptID
	job.AcceptedAt = time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	finish, err := tracked.trackUsage(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(errors.New("preparation failed before a model call")); err == nil {
		t.Fatal("missing preparation error")
	}
	first, found, err := store.Load(job.AttemptID)
	if err != nil || !found {
		t.Fatalf("first attempt: found=%t error=%v", found, err)
	}
	first.StartedAt = job.AcceptedAt
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	finish, err = tracked.trackUsage(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(errors.New("preparation failed before a model call")); err == nil {
		t.Fatal("missing preparation error")
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || !records[0].StartedAt.Equal(job.AcceptedAt) || records[0].ID != job.AttemptID {
		t.Fatalf("recovery moved the same attempt to a new reporting day: records=%+v error=%v", records, err)
	}
	retry := attemptFixture(&Orchestrator{Usage: store})
	retry.attemptID = strings.Repeat("b", 32)
	job.AttemptID = retry.attemptID
	started := time.Now().UTC()
	if _, err := retry.trackUsage(job); err != nil {
		t.Fatal(err)
	}
	if got := retry.usageCheckpoint.record.StartedAt; got.Before(started) || got.After(time.Now().UTC()) || got.UTC().Format("2006-01-02") == first.StartedAt.UTC().Format("2006-01-02") {
		t.Fatalf("retry used original attempt's reporting day: started=%s original=%s", got, first.StartedAt)
	}
	legacy := usageJob()
	legacy.AcceptedAt = job.AcceptedAt
	other := attemptFixture(&Orchestrator{Usage: &usage.Store{Dir: filepath.Join(t.TempDir(), "legacy")}})
	if _, err := other.trackUsage(legacy); err != nil {
		t.Fatal(err)
	}
	if !other.usageCheckpoint.record.StartedAt.After(legacy.AcceptedAt.Add(time.Hour)) {
		t.Fatalf("legacy attempt reused original acceptance time: started=%s", other.usageCheckpoint.record.StartedAt)
	}
}

func TestSelectedUsageDoesNotOverwriteStartedModelCall(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tracked := attemptFixture(&Orchestrator{Usage: store})
	job := usageJob()
	job.AttemptID = tracked.attemptID
	prior := usage.Record{
		ID: job.AttemptID, JobID: job.ID, Repo: job.Repository.Name, User: job.Source.Author.Login,
		Action: string(job.Command.Action), Provider: "codex", Model: "default", StartedAt: time.Now().UTC().Add(-time.Hour),
		Status: "running", ExecutorID: "local", SourceRef: strings.Repeat("a", 40), CodexCalls: 1,
	}
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	if _, err := tracked.trackUsage(job); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("existing model call was overwritten: %v", err)
	}
	got, found, err := store.Load(job.AttemptID)
	if err != nil || !found || got.CodexCalls != 1 || !got.StartedAt.Equal(prior.StartedAt) {
		t.Fatalf("existing checkpoint changed: %+v found=%t err=%v", got, found, err)
	}
}

func TestUsageDoesNotCountCanceledDispatch(t *testing.T) {
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	work := t.TempDir()
	tracked := attemptFixture(&Orchestrator{Usage: store, WorkRoot: work, Runner: Runner{WorkRoot: work, runModel: forbidModel(t)}})
	finish, err := tracked.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	job := testJob(command.Comment)
	job.Identity.AttemptID = tracked.attemptID
	_, err = tracked.Runner.Execute(ctx, job, newExecutionCheckpoints(tracked, job))
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
			}
			tracked := attemptFixture(e)
			finish, err := tracked.trackUsage(usageJob())
			if err != nil {
				t.Fatal(err)
			}
			tracked.usageCheckpoint.record.ExecutorID, tracked.usageCheckpoint.record.SourceRef = "local", strings.Repeat("a", 40)
			for _, call := range []struct {
				role     string
				settings RoleSettings
			}{{"main", main}, {"critic", critic}, {"main", main}} {
				receipt := CallUsage{Role: call.role, Provider: call.settings.Provider, Model: call.settings.Model, Effort: call.settings.Effort}
				if err := tracked.usageCheckpoint.beforeCall(receipt); err != nil {
					t.Fatal(err)
				}
				checkpoint, err := usage.ReadAll(store.Dir)
				if err != nil || len(checkpoint) != 1 || checkpoint[0].UsageComplete {
					t.Fatalf("call has no durable incomplete checkpoint: %+v, %v", checkpoint, err)
				}
				receipt.Tokens, receipt.Complete = usage.Tokens{Input: 100, CachedInput: 80, Output: 20}, true
				if call.role == "critic" {
					receipt.Tokens, receipt.Complete = usage.Tokens{Input: 10, CachedInput: 8, Output: 2}, false
				}
				if err := tracked.usageCheckpoint.afterCall(receipt); err != nil {
					t.Fatal(err)
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
	work := t.TempDir()
	var critiques int
	e := &Orchestrator{
		Usage:       store,
		Main:        "claude",
		Adversarial: "codex",
		WorkRoot:    work,
		Runner: Runner{WorkRoot: work, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			if request.provider == "claude" {
				return aicli.Result{Answer: proposalJSON("proposal", "Private evidence"), Tokens: usage.Tokens{Input: 100, Output: 20}, UsageComplete: true}, nil
			}
			critiques++
			answer := approvedCritiqueJSON
			if critiques == 1 {
				answer = `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Check the error path."}`
			}
			return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
		}},
	}
	tracked := attemptFixture(e)
	finish, err := tracked.trackUsage(usageJob())
	if err != nil {
		t.Fatal(err)
	}
	job := withClaudeCritic(testJob(command.Comment))
	job.Identity.AttemptID = tracked.attemptID
	job.Main.Provider, job.Critic.Provider = "claude", "codex"
	result, err := tracked.Runner.Execute(t.Context(), job, newExecutionCheckpoints(tracked, job))
	if err != nil || result.Answer != "proposal" || result.CriticStopReason != "approved" {
		t.Fatalf("answer=%q stop=%q err=%v", result.Answer, result.CriticStopReason, err)
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
		name           string
		provider       string
		before         bool
		modelErr       error
		critic         bool
		approvedCritic bool
	}{
		{"codex main/before call", "codex", true, modelErr, false, false}, {"claude main/before call", "claude", true, modelErr, false, false},
		{"codex main/after failed call", "codex", false, modelErr, false, false}, {"claude main/after failed call", "claude", false, modelErr, false, false},
		{"codex main/after successful call", "codex", false, nil, false, false}, {"claude main/after successful call", "claude", false, nil, false, false},
		{"codex main/after critic call", "codex", false, nil, true, false}, {"claude main/after critic call", "claude", false, nil, true, false},
		{"codex main/after approving critic call", "codex", false, nil, true, true}, {"claude main/after approving critic call", "claude", false, nil, true, true},
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
					answer := `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Blocking failure."}`
					if tt.approvedCritic {
						answer = approvedCritiqueJSON
					}
					return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 11, Output: 3}, UsageComplete: true}, nil
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
			job.Identity.AttemptID = tracked.attemptID
			job.Main.Provider = tt.provider
			if tt.critic {
				job = withClaudeCritic(job)
				if tt.provider == "claude" {
					job.Critic.Provider = "codex"
				}
			}
			tracked.WorkRoot = t.TempDir()
			tracked.Runner.WorkRoot = tracked.WorkRoot
			result, err := tracked.Runner.Execute(t.Context(), job, newExecutionCheckpoints(tracked, job))
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
				if last.HasBlockers == tt.approvedCritic || last.Approved != tt.approvedCritic || last.NoBlockers != tt.approvedCritic || !last.ReviewComplete || last.Tokens != (usage.Tokens{Input: 11, Output: 3}) || !last.Complete || result.CriticStopReason != "" || result.validate(job, false) != nil {
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
			err := e.Execute(t.Context(), job)
			if failInitialSave {
				if err == nil || errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrPostModelPublication) || !strings.Contains(err.Error(), "save action usage") || len(comments) != 0 {
					t.Fatalf("action ran without initial usage record: err=%v comments=%+v", err, comments)
				}
				return
			}
			if err != nil || len(comments) != 1 {
				t.Fatalf("execute error=%v comments=%+v", err, comments)
			}
			if err := e.Execute(t.Context(), job); err != nil || len(comments) != 1 {
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
	job.Identity.AttemptID = tracked.attemptID
	job.Main.Model, job.Main.Effort = "portable-model", "high"
	job.Main.Skills = []string{testReviewSkillURL}
	result, err := tracked.Runner.Execute(t.Context(), job, newExecutionCheckpoints(tracked, job))
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
	if !strings.Contains(string(prompt), ".minion-context-") || strings.Contains(string(args), ".minion-context-") || !strings.Contains(string(prompt), installed) || !strings.Contains(string(args), "--model\nportable-model\n") || !strings.Contains(string(args), `model_reasoning_effort="high"`) || strings.Contains(string(args), "stale-host-model") {
		t.Fatalf("usage checkpoints lost worker context or skills: args=%s prompt=%s", args, prompt)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].Model != "portable-model" || records[0].CodexCalls != 1 || records[0].Tokens.Total() != 5 || len(result.Calls) != 1 {
		t.Fatalf("usage counted dispatch incorrectly: records=%+v calls=%+v err=%v", records, result.Calls, err)
	}
}
