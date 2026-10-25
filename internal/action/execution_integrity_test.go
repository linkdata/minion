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
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestExecuteRetainsPaidCriticReceiptAfterSuppliedMetadataIntegrityFailure(t *testing.T) {
	for _, approved := range []bool{false, true} {
		name := "rejected"
		if approved {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			sourceHead, _, _ := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("supplied source\n")})
			work := t.TempDir()
			store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			var publications atomic.Int32
			var reply string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments" {
					publications.Add(1)
				}
				if !serveIssueFixture(t, w, r) && !captureComment(t, w, r, &reply) {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			mainTokens, criticTokens := usage.Tokens{Input: 10, CachedInput: 2, Output: 3}, usage.Tokens{Input: 7, CachedInput: 1, Output: 2}
			var journal bytes.Buffer
			zero, calls := 0, 0
			e := Orchestrator{WorkRoot: work, Usage: store, GitHub: testGitHub(server, "fixture"), BotLogin: "bot", Main: "codex", Adversarial: "claude", Logger: log.New(&journal, "", 0),
				CriticStopping: config.CriticStopping{MaxRounds: 3, NonblockingAfterRound: &zero, ConsecutiveNoBlockerRounds: &zero},
				Commands:       map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Runner: Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if calls == 1 && request.role == roleMain && request.round == 1 {
						return aicli.Result{Answer: proposalJSON("Candidate comment.", ""), Tokens: mainTokens, UsageComplete: true}, nil
					}
					if calls != 2 || request.role != roleCritic || request.round != 1 || request.reviewPhase != reviewFull {
						t.Fatalf("unexpected call after supplied metadata mutation: %+v", request)
					}
					path := filepath.Join(request.dir, ".git", "config")
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, append(data, []byte("\n# critic metadata mutation\n")...), 0600); err != nil {
						t.Fatal(err)
					}
					verdict := approvedCritiqueJSON
					if !approved {
						verdict = criticFeedbackJSON(true, "PRIVATE_BLOCKING_FINDING")
					}
					return aicli.Result{Answer: verdict, Tokens: criticTokens, UsageComplete: true}, nil
				}},
			}
			request := usageJob()
			request.Command, request.Repository.Actions = command.Command{Name: "comment", Action: command.Comment}, []string{"comment"}
			err := e.Execute(t.Context(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "supplied repository changed") || !strings.Contains(err.Error(), "Git metadata changed") || calls != 2 || publications.Load() != 0 {
				t.Fatalf("supplied metadata integrity failure continued or published: calls=%d publications=%d error=%v", calls, publications.Load(), err)
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 {
				t.Fatalf("paid accounting missing after integrity failure: records=%+v error=%v", records, err)
			}
			record := records[0]
			if record.Status != "failed" || record.FinishedAt == nil || !record.UsageComplete || record.CodexCalls != 1 || record.ClaudeCalls != 1 || record.Tokens != (usage.Tokens{Input: 17, CachedInput: 3, Output: 5}) {
				t.Fatalf("integrity failure lost acknowledged paid counters: %+v", record)
			}
			wantCalls := []CallUsage{
				{Provider: "codex", Role: roleMain, Round: 1, Tokens: mainTokens, Complete: true},
				{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: criticTokens, Complete: true, Approved: approved, NoBlockers: approved, HasBlockers: !approved, ReviewComplete: true},
			}
			saved := readPrivateReview(t, work)
			if saved.Identity.JobID != request.ID || saved.Identity.AttemptID != record.ID || saved.SourceRef != sourceHead || !slices.Equal(saved.Calls, wantCalls) || len(saved.Refutations) != 0 || saved.CriticStopReason != "" {
				t.Fatalf("integrity failure lost captured verdict or invented a decision: %+v", saved)
			}
			if strings.Count(journal.String(), "action call ") != 2 || strings.Count(journal.String(), `policy_met=false stop_reason=""`) != 2 || strings.Contains(journal.String(), "policy_met=true") || strings.Contains(journal.String(), "capped=true") || strings.Contains(journal.String(), "PRIVATE_BLOCKING_FINDING") {
				t.Fatalf("journal promoted a verdict after metadata integrity failure: %s", journal.String())
			}
		})
	}
}
