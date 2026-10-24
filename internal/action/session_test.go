package action

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRunnerKeepsRoleSessionsWithinEachAction(t *testing.T) {
	requireActionProcessGroup(t)
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 3
	var actions [][]modelRun
	var calls []modelRun
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls = append(calls, request)
		if request.role == roleMain {
			return aicli.Result{Answer: proposalJSON(fmt.Sprintf("proposal%d", request.round), "")}, nil
		}
		if request.reviewPhase == reviewFull && request.round < 3 {
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Correct the proposal."}`}, nil
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	for range 2 {
		calls = nil
		result, err := runner.Run(t.Context(), job)
		if err != nil || result.Answer != "proposal3" || len(calls) != 8 {
			t.Fatalf("answer=%q calls=%d error=%v", result.Answer, len(calls), err)
		}
		main, critic := calls[0].session, calls[1].session
		if main == nil || critic == nil || main == critic {
			t.Fatal("missing or shared main/critic session")
		}
		for _, call := range calls {
			if call.role == roleMain {
				if call.session != main || call.freshConversation {
					t.Fatal("main revision lost its action session")
				}
			} else if call.session != critic || call.freshConversation != (call.reviewPhase == reviewFull) {
				t.Fatalf("critic phase %s has wrong continuation policy", call.reviewPhase)
			}
		}
		actions = append(actions, calls)
	}
	if actions[0][0].session == actions[1][0].session || actions[0][1].session == actions[1][1].session {
		t.Fatal("native session reused by another action")
	}
}

func TestRunnerKeepsSourceCriticContextAndRefutationPaths(t *testing.T) {
	requireActionProcessGroup(t)
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 3
	var mainDir, criticDir, contextDir, historyDir string
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls++
		if len(request.readOnlyDirs) == 0 {
			t.Fatal("context grant missing")
		}
		currentContext := request.readOnlyDirs[0]
		if contextDir == "" {
			contextDir = currentContext
		}
		if currentContext != contextDir {
			t.Fatal("context path changed between rounds")
		}
		if len(request.refutations) != 0 {
			currentHistory := request.readOnlyDirs[len(request.readOnlyDirs)-1]
			if historyDir == "" {
				historyDir = currentHistory
			}
			if currentHistory != historyDir {
				t.Fatal("refutation grant path changed")
			}
			if _, err := os.ReadFile(filepath.Join(currentHistory, "refutations.json")); err != nil {
				t.Fatal(err)
			}
		}
		if request.role == roleMain {
			if mainDir == "" {
				mainDir = request.dir
			}
			if request.dir != mainDir {
				t.Fatal("main workspace changed")
			}
			writeFixtureFile(t, request.dir, "source.txt", []byte(fmt.Sprintf("revision%d", request.round)))
			notes := ""
			if request.round == 2 {
				notes = "Refuted unrelated preference: task requires source.txt; retain the supported path."
			}
			return aicli.Result{Answer: proposalJSON(fmt.Sprintf("proposal%d", request.round), notes)}, nil
		}
		if criticDir == "" {
			criticDir = request.dir
		}
		if request.dir != criticDir || criticDir == mainDir {
			t.Fatal("critic snapshot moved or aliases main workspace")
		}
		data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
		if err != nil || string(data) != fmt.Sprintf("revision%d", request.round) {
			t.Fatalf("critic saw stale candidate=%q error=%v", data, err)
		}
		if request.reviewPhase == reviewFull {
			if len(request.refutations) != 0 || (historyDir != "" && strings.Contains(strings.Join(request.readOnlyDirs, "\n"), historyDir)) {
				t.Fatal("independent full review received the private refutation grant")
			}
			if request.round < 3 {
				return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Revise source.txt."}`}, nil
			}
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if err != nil || calls != 8 || result.Answer != "proposal3" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "revision3" {
		t.Fatalf("result=%+v calls=%d error=%v", result, calls, err)
	}
	if len(result.Refutations) != 1 || result.Refutations[0].Round != 2 {
		t.Fatalf("refutations=%+v", result.Refutations)
	}
	for _, dir := range []string{mainDir, criticDir, contextDir, historyDir} {
		if dir == "" {
			t.Fatal("expected workspace was not exercised")
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("action-owned path remains %q: %v", dir, err)
		}
	}
}

func TestReviewSnapshotRefreshKeepsAuditPathsAndRemovesOldArtifacts(t *testing.T) {
	job := testJob(command.SecurityAudit)
	source, output, root := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, job.Source.Files); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, output, "REPORT.md", []byte("first"))
	writeFixtureFile(t, output, "FINDINGS-DETAIL.md", []byte("old detail"))
	snapshot := mustFreeze(t, t.Context(), root, job, source, output, nil)
	frozenReports := snapshot.outputDir
	writeFixtureFile(t, output, "REPORT.md", []byte("second"))
	if err := os.Remove(filepath.Join(output, "FINDINGS-DETAIL.md")); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if snapshot.source != source || snapshot.outputDir != frozenReports {
		t.Fatal("audit paths changed")
	}
	data, err := os.ReadFile(filepath.Join(frozenReports, "REPORT.md"))
	if err != nil || string(data) != "second" {
		t.Fatalf("stale report=%q error=%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(frozenReports, "FINDINGS-DETAIL.md")); !os.IsNotExist(err) {
		t.Fatalf("removed artifact survives: %v", err)
	}
	if err := snapshot.verify(t.Context()); err != nil {
		t.Fatal(err)
	}
}
