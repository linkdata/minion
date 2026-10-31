package action

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

func TestExecuteOrdersAccountingReviewAndPublicationCheckpoints(t *testing.T) {
	for _, overHTTP := range []bool{false, true} {
		for _, capped := range []bool{false, true} {
			t.Run(fmt.Sprintf("HTTP=%t/capped=%t", overHTTP, capped), func(t *testing.T) { testExecuteOrdersCheckpoints(t, overHTTP, capped) })
		}
	}
}

func testExecuteOrdersCheckpoints(t *testing.T, overHTTP, capped bool) {
	maxRounds, stopReason := 3, "approved"
	if capped {
		maxRounds, stopReason = 2, "round_cap"
	}
	sourceHead, _, _ := gitFixtureRemote(t)
	work := t.TempDir()
	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	const finding, refutation = "PRIVATE_BLOCKER_EVIDENCE", "PRIVATE_COUNTEREVIDENCE"
	const answer = "Public completed response."
	const mainModel, criticModel = "fixture-main", "fixture-critic"
	expected := []CallUsage{
		{Provider: "codex", Role: roleMain, Round: 1, Model: mainModel, Effort: "high", Tokens: usage.Tokens{Input: 10, CachedInput: 1, Output: 2}, Complete: true},
		{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Model: criticModel, Effort: "medium", Tokens: usage.Tokens{Input: 3, CachedInput: 1, Output: 1}, Complete: true, HasBlockers: true, ReviewComplete: true},
		{Provider: "codex", Role: roleMain, Round: 2, Model: mainModel, Effort: "high", Tokens: usage.Tokens{Input: 20, CachedInput: 2, Output: 4}, Complete: true},
		{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewVerification, Model: criticModel, Effort: "medium", Tokens: usage.Tokens{Input: 5, CachedInput: 1, Output: 1}, Complete: true, Approved: true, NoBlockers: true, ReviewComplete: true},
		{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewFull, Model: criticModel, Effort: "medium", Tokens: usage.Tokens{Input: 7, CachedInput: 2, Output: 2}, Complete: true, Approved: true, NoBlockers: true, ReviewComplete: true},
	}
	if capped {
		expected[4].Approved, expected[4].NoBlockers, expected[4].HasBlockers = false, false, true
	}
	wantTokens := usage.Tokens{Input: 45, CachedInput: 7, Output: 10}
	wantRefutations := []Refutation{{Round: 2, Text: refutation}}
	request := usageJob()
	request.Command, request.Repository.Actions = command.Command{Name: "comment", Action: command.Comment}, []string{"comment"}
	published := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveIssueFixture(t, w, r) {
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/issues/7/comments" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		records, err := usage.ReadAll(store.Dir)
		if err != nil || len(records) != 1 {
			t.Errorf("publication preceded durable accounting: records=%+v error=%v", records, err)
			http.Error(w, "accounting checkpoint missing", http.StatusInternalServerError)
			return
		}
		record := records[0]
		if record.Status != "running" || record.FinishedAt != nil || !record.UsageComplete || record.CodexCalls != 2 || record.ClaudeCalls != 3 || record.Tokens != wantTokens {
			t.Errorf("publication preceded final paid counters: %+v", record)
			http.Error(w, "accounting checkpoint incomplete", http.StatusInternalServerError)
			return
		}
		review, err := loadPrivateReview(work)
		if err != nil || review.Identity.JobID != request.ID || review.Identity.AttemptID != record.ID || review.SourceRef != sourceHead || review.CriticStopReason != stopReason || review.CriticMaxRounds != maxRounds || review.CriticNonblockingRounds != 0 || review.CriticConsecutiveNoBlockerRounds != 0 || !slices.Equal(review.Calls, expected) || !slices.Equal(review.Refutations, wantRefutations) {
			t.Errorf("publication preceded the acknowledged review decision: review=%+v error=%v", review, err)
			http.Error(w, "review decision missing", http.StatusInternalServerError)
			return
		}
		var payload struct{ Body string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(w, "invalid publication", http.StatusBadRequest)
			return
		}
		if strings.Contains(payload.Body, "Critic round limit reached (2).") != capped {
			t.Errorf("HTTP cap decision lost its public caveat: %q", payload.Body)
		}
		if !strings.Contains(payload.Body, answer) || strings.Contains(payload.Body, finding) || strings.Contains(payload.Body, refutation) {
			t.Errorf("publication lost the answer or exposed private review text: %s", payload.Body)
			http.Error(w, "invalid public answer", http.StatusInternalServerError)
			return
		}
		select {
		case published <- payload.Body:
		default:
			t.Error("published the action more than once")
			http.Error(w, "duplicate publication", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":1}`)
	}))
	defer server.Close()
	var journal bytes.Buffer
	zero, calls := 0, 0
	responses := make([]string, len(expected))
	prompts := make([]string, len(expected))
	agents := []string{"session-main", "session-critic-1", "session-main", "session-critic-1", "session-critic-2"}
	e := Orchestrator{WorkRoot: work, GitHub: testGitHub(server, "fixture"), BotLogin: "bot", BotID: 8, Usage: store, Store: openState(t, time.Now()), Logger: log.New(&journal, "", 0),
		Main: "codex", Adversarial: "claude", CodexModel: mainModel, CodexEffort: "high", ClaudeModel: criticModel, ClaudeEffort: "medium",
		CriticStopping: config.CriticStopping{MaxRounds: maxRounds, NonblockingAfterRound: &zero, ConsecutiveNoBlockerRounds: &zero},
		Commands:       map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		Runner: Runner{runModel: func(_ context.Context, native modelRun) (aicli.Result, error) {
			index := calls
			calls++
			if index >= len(expected) {
				return aicli.Result{}, fmt.Errorf("unexpected additional model call: %+v", native)
			}
			want := expected[index]
			if native.provider != want.Provider || native.role != want.Role || native.round != want.Round || native.reviewPhase != want.ReviewPhase || native.model != want.Model || native.effort != want.Effort || native.freshConversation != (want.Role == roleCritic && want.ReviewPhase == reviewFull) {
				return aicli.Result{}, fmt.Errorf("model call order/settings changed at %d: %+v", index, native)
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 {
				return aicli.Result{}, fmt.Errorf("native call lacks durable reservation: records=%+v error=%v", records, err)
			}
			record := records[0]
			var priorTokens usage.Tokens
			var codexCalls, claudeCalls int
			for i, call := range expected[:index+1] {
				if call.Provider == "codex" {
					codexCalls++
				} else {
					claudeCalls++
				}
				if i < index {
					priorTokens.Input += call.Tokens.Input
					priorTokens.CachedInput += call.Tokens.CachedInput
					priorTokens.Output += call.Tokens.Output
				}
			}
			if record.Status != "running" || record.FinishedAt != nil || record.UsageComplete || record.CodexCalls != codexCalls || record.ClaudeCalls != claudeCalls || record.Tokens != priorTokens {
				return aicli.Result{}, fmt.Errorf("native call %d preceded incomplete accounting reservation: %+v", index, record)
			}
			review, err := loadPrivateReview(work)
			if err != nil {
				return aicli.Result{}, fmt.Errorf("native call lacks private prefix: %w", err)
			}
			var priorRefutations []Refutation
			if index >= 3 {
				priorRefutations = wantRefutations
			}
			if review.Identity.JobID != request.ID || review.Identity.AttemptID != record.ID || review.SourceRef != sourceHead || review.CriticStopReason != "" || review.CriticMaxRounds != maxRounds || review.CriticNonblockingRounds != 0 || review.CriticConsecutiveNoBlockerRounds != 0 || !slices.Equal(review.Calls, expected[:index]) || !slices.Equal(review.Refutations, priorRefutations) {
				return aicli.Result{}, fmt.Errorf("native call %d preceded its private review prefix: %+v", index, review)
			}
			response := approvedCritiqueJSON
			switch index {
			case 0:
				response = proposalJSON("Initial public response.", "")
			case 1:
				response = criticFeedbackJSON(true, finding)
			case 2:
				response = proposalJSON(answer, refutation)
			case 4:
				if capped {
					response = criticFeedbackJSON(true, finding)
				}
			}
			responses[index] = response
			if overHTTP {
				if native.onInput == nil || native.onEvent == nil {
					return aicli.Result{}, fmt.Errorf("call %d has no provider input or event sink", index+1)
				}
				prompts[index] = native.prompt
				inputID := agents[index]
				mode := "fresh"
				if index == 0 { // A fresh Codex invocation has no native ID before launch.
					inputID = ""
				}
				if index == 2 || index == 3 {
					mode = "resume"
				}
				native.onInput(aicli.PreparedInput{NativeID: inputID, Mode: mode, Path: "/fixture/cli",
					Args: []string{"cli", "exec", "-"}, Dir: native.dir, Stdin: native.prompt, Schema: native.outputSchema})
				if native.provider == "codex" {
					native.onEvent(aicli.StreamEvent{NativeID: agents[index], Kind: "identity"})
				}
				native.onEvent(aicli.StreamEvent{NativeID: agents[index], Kind: "final", Text: response})
			}
			return aicli.Result{Answer: response, Tokens: want.Tokens, UsageComplete: true}, nil
		}},
	}
	selectFixtureRunner(t, &e, &request)
	var workerDone chan error
	var remote *HTTPExecutor
	if overHTTP {
		var err error
		remote, err = NewHTTPExecutor("local")
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.SetWorkRoot(work); err != nil {
			t.Fatal(err)
		}
		coveragePersisted := make(chan struct{})
		var coverageOnce sync.Once
		recordsPath := filepath.Join(work, "attempt-records", request.AttemptID+".jsonl")
		transport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				select {
				case <-coveragePersisted:
				case <-r.Context().Done():
					return
				}
			}
			remote.ServeHTTP(w, r)
			if r.URL.Path == "/status" {
				data, err := os.ReadFile(recordsPath)
				if err != nil {
					return
				}
				lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
				var last attemptRecord
				if json.Unmarshal(lines[len(lines)-1], &last) == nil && last.Kind == attemptRecordCoverage {
					coverageOnce.Do(func() { close(coveragePersisted) })
				}
			}
		}))
		t.Cleanup(transport.Close)
		workerCtx, cancelWorker := context.WithCancel(t.Context())
		t.Cleanup(func() { cancelWorker(); _ = remote.Close() })
		worker := e.Runner
		worker.WorkRoot = t.TempDir()
		client := transport.Client()
		workerDone = make(chan error, 1)
		go func() { workerDone <- RunHTTP(workerCtx, client, transport.URL, "local", &worker) }()
		e.Executor = executorFunc(func(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
			if err := sink.(assignmentCheckpoint).BeforeAssignment(job); err != nil {
				return Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}, err
			}
			return remote.Execute(ctx, job, sink)
		})
	}
	if err := e.Execute(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if overHTTP {
		select {
		case err := <-workerDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP worker did not receive outcome acknowledgement")
		}
		if status, receivedAt, ok := remote.Status(); ok && (status.Sequence == 0 || receivedAt.IsZero()) {
			t.Fatalf("HTTP worker sent invalid status: %+v received=%v", status, receivedAt)
		}
	}
	if calls != len(expected) {
		t.Fatalf("native calls=%d want=%d", calls, len(expected))
	}
	if overHTTP {
		data, err := os.ReadFile(filepath.Join(work, "attempt-records", request.AttemptID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
		seen := make(map[string]map[string]attemptRecord)
		covered := false
		var lastSequence uint64
		for _, line := range lines {
			var saved attemptRecord
			if err := json.Unmarshal(line, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Sequence != lastSequence+1 {
				t.Fatalf("attempt record sequence %d after %d", saved.Sequence, lastSequence)
			}
			lastSequence = saved.Sequence
			if saved.Kind == attemptRecordCoverage {
				covered = true
			}
			if saved.Call != "" {
				ordinal, err := strconv.Atoi(saved.Call)
				observedAt, timeErr := time.Parse(time.RFC3339Nano, saved.ObservedAt)
				if err != nil || ordinal < 1 || ordinal > len(expected) ||
					timeErr != nil || observedAt.IsZero() || !strings.HasSuffix(saved.ObservedAt, "Z") ||
					saved.Provider != expected[ordinal-1].Provider || saved.Model != expected[ordinal-1].Model ||
					saved.Role != expected[ordinal-1].Role || saved.Round != expected[ordinal-1].Round ||
					saved.ReviewPhase != expected[ordinal-1].ReviewPhase {
					t.Fatalf("call record lost its selected binding or worker observation time: %+v", saved)
				}
				if saved.EventSource == saved.Provider+".final" &&
					(saved.Kind != attemptRecordDiscussion || saved.Text != responses[ordinal-1] || saved.Agent != agents[ordinal-1]) {
					t.Fatalf("received final event lost its private binding: %+v", saved)
				}
				if seen[saved.Call] == nil {
					seen[saved.Call] = make(map[string]attemptRecord)
				}
				seen[saved.Call][saved.EventSource] = saved
			}
		}
		if !covered {
			t.Fatal("HTTP coverage marker missing")
		}
		for index, want := range expected {
			call := fmt.Sprint(index + 1)
			input, ok := seen[call][want.Provider+".input.prepared"]
			if !ok || input.Kind != attemptRecordInput || input.Call != call ||
				input.Role != want.Role || input.Round != want.Round || input.ReviewPhase != want.ReviewPhase {
				t.Fatalf("call %s lost its prepared input binding: %+v", call, input)
			}
			var prepared preparedInputPayload
			if err := json.Unmarshal([]byte(input.Text), &prepared); err != nil {
				t.Fatal(err)
			}
			wantSchema := mainOutputSchema
			if want.Role == roleCritic {
				wantSchema = criticOutputSchema
			}
			wantMode := "fresh"
			if index == 2 || index == 3 {
				wantMode = "resume"
			}
			wantInputAgent := agents[index]
			if index == 0 {
				wantInputAgent = ""
			}
			if input.Agent != wantInputAgent || prepared.Mode != wantMode ||
				prepared.Representation != "redacted" || prepared.Stage != "prepared" || prepared.Stdin != prompts[index] ||
				prepared.Schema != wantSchema ||
				!slices.Equal(prepared.Argv, []string{"cli", "exec", "-"}) {
				t.Fatalf("call %s prepared input was not preserved: %+v", call, prepared)
			}
			saved, ok := seen[call][want.Provider+".final"]
			if !ok {
				t.Fatalf("call %s lost its final provider event", call)
			}
			if saved.Kind != attemptRecordDiscussion || saved.Sequence <= input.Sequence ||
				saved.Call != fmt.Sprint(index+1) || saved.Agent != agents[index] ||
				saved.Role != want.Role || saved.Round != want.Round || saved.ReviewPhase != want.ReviewPhase ||
				saved.EventSource != want.Provider+".final" || saved.Text != responses[index] {
				t.Fatalf("call %d event lost its private binding: %+v", index+1, saved)
			}
			if want.Provider == "codex" {
				identity, ok := seen[call]["codex.identity"]
				if !ok || identity.Kind != attemptRecordInput || identity.Agent != saved.Agent || identity.Sequence <= input.Sequence || identity.Sequence >= saved.Sequence {
					t.Fatalf("call %s lost its later native identity: %+v", call, identity)
				}
			}
		}
	}
	select {
	case <-published:
	default:
		t.Fatal("action never published")
	}
	// Initial evidence, each parsed receipt and the decision require distinct durable writes.
	if got := strings.Count(journal.String(), "review evidence "); got != len(expected)+2 {
		t.Fatalf("decision was not a separate checkpoint: writes=%d log=%s", got, journal.String())
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].Status != "succeeded" || records[0].FinishedAt == nil || !records[0].UsageComplete || records[0].CodexCalls != 2 || records[0].ClaudeCalls != 3 || records[0].Tokens != wantTokens {
		t.Fatalf("successful publication lost final accounting: records=%+v error=%v", records, err)
	}
}
