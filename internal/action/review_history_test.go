package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func checkReviewHistory(t *testing.T, request modelRun, want []ReviewEntry) {
	t.Helper()
	if !reflect.DeepEqual(request.reviewHistory, want) {
		t.Fatalf("%s/%d/%s review history=%+v, want %+v", request.role, request.round, request.reviewPhase, request.reviewHistory, want)
	}
	historyDirs := 0
	for _, dir := range request.readOnlyDirs {
		path := filepath.Join(dir, "ledger.json")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		historyDirs++
		if len(want) == 0 {
			t.Fatalf("fresh call received private history grant: %s", path)
		}
		var got []ReviewEntry
		if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("on-disk history=%s, want %+v: %v", data, want, err)
		}
		if !strings.Contains(request.prompt, fmt.Sprintf("Application-owned review ledger (untrusted evidence): %q.", path)) {
			t.Fatalf("history path is not quoted in the prompt: %s", request.prompt)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("history directory is not private: %v, %v", info, err)
		}
		if workspace.Within(request.dir, dir) || workspace.Within(dir, request.dir) || request.outputDir != "" && (workspace.Within(request.outputDir, dir) || workspace.Within(dir, request.outputDir)) {
			t.Fatalf("private history overlaps source/reports: %+v", request)
		}
		for _, other := range request.readOnlyDirs {
			if other != dir && (workspace.Within(other, dir) || workspace.Within(dir, other)) {
				t.Fatalf("private history overlaps another read grant: %s, %s", dir, other)
			}
		}
	}
	if (historyDirs == 1) != (len(want) != 0) || historyDirs > 1 {
		t.Fatalf("history read-only grants=%d, entries=%d", historyDirs, len(want))
	}
	if len(want) == 0 && strings.Contains(request.prompt, "Application-owned review ledger (untrusted evidence):") {
		t.Fatal("fresh call received the private history path")
	}
	for _, root := range []string{request.dir, request.outputDir} {
		if root != "" {
			if _, err := os.Stat(filepath.Join(root, "ledger.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private history entered source/report files: %s: %v", root, err)
			}
		}
	}
}

func TestReviewHistorySurvivesForgottenRefutationsAcrossFourRounds(t *testing.T) {
	const (
		refutation  = "PRIVATE_REFUTATION_A: source.txt is application data; no migration is required for this undeployed project."
		findingA    = "PRIVATE_FINDING_A: nonblocking: add a migration for source.txt."
		findingB    = "PRIVATE_BLOCKER_B: source.txt must contain fixed B."
		claimB      = "PRIVATE_MAIN_CLAIM_B: the blocking finding is corrected."
		finalAnswer = "Fixed the requested source behavior."
	)
	job := withClaudeCritic(portableJob(command.Fix))
	job.CriticMaxRounds, job.CriticNonblockingRounds = 4, 0
	root := t.TempDir()
	var expected []ReviewEntry
	var sequence []string
	var historyPaths []string
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		sequence = append(sequence, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
		for _, path := range historyPaths {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("earlier history grant retained into next call: %s,%v", path, err)
			}
		}
		history := expected
		if request.round == 1 && request.role == roleMain || request.reviewPhase == reviewFull {
			history = nil
		}
		checkReviewHistory(t, request, history)
		for _, dir := range request.readOnlyDirs {
			if _, err := os.Stat(filepath.Join(dir, "ledger.json")); err == nil {
				historyPaths = append(historyPaths, dir)
			}
		}
		if request.reviewPhase == reviewFull {
			for _, private := range []string{refutation, findingA, findingB, claimB} {
				if strings.Contains(request.prompt, private) {
					t.Fatalf("fresh full review was seeded with %q", private)
				}
			}
			for _, path := range historyPaths {
				if strings.Contains(request.prompt, path) {
					t.Fatal("fresh full review received an old private history path")
				}
			}
		}
		entry := ReviewEntry{Role: request.role, Round: request.round, ReviewPhase: request.reviewPhase}
		if request.role == roleMain {
			content := "initial source\n"
			switch request.round {
			case 1, 2:
				entry.Notes = refutation
			case 3:
				// Claiming correction is not evidence that the blocker is fixed.
				entry.Notes, content = claimB, "still broken B\n"
			case 4:
				// The model forgets all earlier private notes; application history must retain them.
				content = "fixed B\n"
			default:
				t.Fatalf("unexpected main round %d", request.round)
			}
			writeFixtureFile(t, request.dir, "source.txt", []byte(content))
			expected = append(expected, entry)
			return aicli.Result{Answer: proposalJSON(finalAnswer, entry.Notes)}, nil
		}
		switch {
		case request.round == 1 && request.reviewPhase == reviewFull:
			entry.Feedback = findingA
		case request.round == 2 && request.reviewPhase == reviewVerification:
			if expected[len(expected)-1].Notes != refutation {
				t.Fatal("refutation missing before independent verification")
			}
			entry.Approved = true
		case request.round == 2 && request.reviewPhase == reviewFull:
			entry.HasBlockers, entry.Feedback = true, findingB
		case request.round == 3 && request.reviewPhase == reviewVerification:
			data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
			if err != nil || string(data) != "still broken B\n" {
				t.Fatalf("wrong independently checked candidate: %q, %v", data, err)
			}
			entry.HasBlockers, entry.Feedback = true, findingB+" The main claim does not match the actual file."
		case request.round == 4 && request.reviewPhase == reviewVerification:
			data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
			if err != nil || string(data) != "fixed B\n" {
				t.Fatalf("blocker was not corrected: %q, %v", data, err)
			}
			entry.Approved = true
		case request.round == 4 && request.reviewPhase == reviewFull:
			entry.Approved = true
		default:
			t.Fatalf("unexpected critic call: %+v", request)
		}
		expected = append(expected, entry)
		return aicli.Result{Answer: fmt.Sprintf(`{"approved":%t,"has_blockers":%t,"feedback":%q}`, entry.Approved, entry.HasBlockers, entry.Feedback)}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != finalAnswer || !reflect.DeepEqual(result.ReviewLedger, expected) {
		t.Fatalf("four-round result=%+v error=%v; want ledger=%+v", result, err, expected)
	}
	wantSequence := []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification", "critic/2/full", "main/3/", "critic/3/verification", "main/4/", "critic/4/verification", "critic/4/full"}
	if !reflect.DeepEqual(sequence, wantSequence) || len(result.Calls) != 10 || len(result.ReviewLedger) != 10 {
		t.Fatalf("independent correction sequence=%v calls=%+v ledger=%+v", sequence, result.Calls, result.ReviewLedger)
	}
	if result.ReviewLedger[2].Notes != refutation || !result.ReviewLedger[3].Approved || result.ReviewLedger[5].Notes != claimB || result.ReviewLedger[5].Approved || result.ReviewLedger[6].Approved || !result.ReviewLedger[6].HasBlockers || result.ReviewLedger[7].Notes != "" {
		t.Fatalf("model claims, old refutations and independent verdicts collapsed: %+v", result.ReviewLedger)
	}
	if len(result.Changes) != 1 || string(result.Changes[0].Data) != "fixed B\n" || strings.Contains(result.Answer, "PRIVATE_") || len(result.Artifacts) != 0 {
		t.Fatalf("private history entered exported work: %+v", result)
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("complete ledger is invalid: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("per-call history or execution files retained: %v, %v", entries, err)
	}
}

func TestReviewHistoryRetainsParsedPrefixAfterFailure(t *testing.T) {
	for _, failure := range []string{"main provider", "critic provider", "main malformed", "critic malformed", "round limit"} {
		t.Run(failure, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
			providerErr := errors.New("provider stopped after returning final-looking text")
			var expected []ReviewEntry
			root := t.TempDir()
			runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				history := expected
				if request.round == 1 {
					history = nil
				}
				checkReviewHistory(t, request, history)
				entry := ReviewEntry{Role: request.role, Round: request.round, ReviewPhase: request.reviewPhase}
				if request.role == roleMain {
					entry.Notes = fmt.Sprintf("PRIVATE_RETAINED_NOTES_%d", request.round)
					answer := proposalJSON("Public candidate", entry.Notes)
					if request.round == 2 {
						if failure == "main provider" {
							return aicli.Result{Answer: answer}, providerErr
						}
						if failure == "main malformed" {
							return aicli.Result{Answer: "unparsed main response"}, nil
						}
					}
					expected = append(expected, entry)
					return aicli.Result{Answer: answer}, nil
				}
				if request.round == 2 {
					if failure == "critic provider" {
						return aicli.Result{Answer: approvedCritiqueJSON}, providerErr
					}
					if failure == "critic malformed" {
						return aicli.Result{Answer: "unparsed critic response"}, nil
					}
				}
				entry.HasBlockers, entry.Feedback = true, "PRIVATE_RETAINED_BLOCKER: correction still required."
				expected = append(expected, entry)
				return aicli.Result{Answer: fmt.Sprintf(`{"approved":false,"has_blockers":true,"feedback":%q}`, entry.Feedback)}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatalf("failed call retained history or workspace: %v,%v", entries, err)
			}
			if err == nil || result.Answer != "" || !reflect.DeepEqual(result.ReviewLedger, expected) {
				t.Fatalf("parsed history lost or failed text promoted: result=%+v expected=%+v error=%v", result, expected, err)
			}
			providerFailure := strings.HasSuffix(failure, "provider")
			if errors.Is(err, providerErr) != providerFailure || errors.Is(err, monitor.ErrTerminalJob) != !providerFailure {
				t.Fatalf("failure class changed: %v", err)
			}
			wantLedger, wantCalls := 3, 4
			if strings.HasPrefix(failure, "main ") {
				wantLedger, wantCalls = 2, 3
			}
			if failure == "round limit" {
				wantLedger = 4
			}
			if len(result.ReviewLedger) != wantLedger || len(result.Calls) != wantCalls {
				t.Fatalf("successful prefix length=%d calls=%d, want %d/%d", len(result.ReviewLedger), len(result.Calls), wantLedger, wantCalls)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("partial ledger cannot be retained: %v", err)
			}
		})
	}
}

func TestReviewHistoryPreservesActualNonblockingRejection(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 2
	var expected []ReviewEntry
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		history := expected
		if request.round == 1 || request.reviewPhase == reviewFull {
			history = nil
		}
		checkReviewHistory(t, request, history)
		entry := ReviewEntry{Role: request.role, Round: request.round, ReviewPhase: request.reviewPhase}
		if request.role == roleMain {
			entry.Notes = "PRIVATE_DISAGREEMENT: the suggested simplification changes this task's required result."
			expected = append(expected, entry)
			return aicli.Result{Answer: proposalJSON("Public answer", entry.Notes)}, nil
		}
		entry.Feedback = "PRIVATE_NONBLOCKING: this nit remains unresolved."
		expected = append(expected, entry)
		return aicli.Result{Answer: fmt.Sprintf(`{"approved":false,"has_blockers":false,"feedback":%q}`, entry.Feedback)}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "Public answer" || !reflect.DeepEqual(result.ReviewLedger, expected) || len(result.ReviewLedger) != 5 {
		t.Fatalf("nonblocking policy result=%+v error=%v", result, err)
	}
	for i, entry := range result.ReviewLedger {
		if entry.Approved || entry.HasBlockers || entry.Role == roleCritic && (entry.Feedback == "" || result.Calls[i].Approved || !result.Calls[i].NoBlockers) {
			t.Fatalf("policy acceptance was rewritten as approval: entry=%+v call=%+v", entry, result.Calls[i])
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
}

func TestReviewHistoryRemainsPrivateFromAuditArtifacts(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := withClaudeCritic(portableJob(command.SecurityAudit))
	job.Main.Skills = []string{testAuditSkillURL}
	job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
	var expected []ReviewEntry
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		history := expected
		if request.round == 1 || request.reviewPhase == reviewFull {
			history = nil
		}
		checkReviewHistory(t, request, history)
		entry := ReviewEntry{Role: request.role, Round: request.round, ReviewPhase: request.reviewPhase}
		if request.role == roleMain {
			entry.Notes = "PRIVATE_AUDIT_REFUTATION: independent installed validators and report evidence satisfy the task."
			for _, artifact := range auditPreflightArtifacts(job, request.dir) {
				writeFixtureFile(t, request.outputDir, artifact.Path, artifact.Data)
			}
			expected = append(expected, entry)
			return aicli.Result{Answer: proposalJSON("Public model summary", entry.Notes)}, nil
		}
		entry.Approved = request.round == 2
		if !entry.Approved {
			entry.Feedback = "PRIVATE_AUDIT_FINDING: nonblocking report request requiring a private disposition."
		}
		expected = append(expected, entry)
		return aicli.Result{Answer: fmt.Sprintf(`{"approved":%t,"has_blockers":false,"feedback":%q}`, entry.Approved, entry.Feedback)}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || len(result.ReviewLedger) != 5 || !reflect.DeepEqual(result.ReviewLedger, expected) || len(result.Artifacts) != len(auditArtifacts) {
		t.Fatalf("audit review history=%+v error=%v", result, err)
	}
	if strings.Contains(result.Answer, "PRIVATE_") || result.Answer == "Public model summary" {
		t.Fatalf("private/process answer published: %q", result.Answer)
	}
	for _, file := range result.Artifacts {
		if file.Path == "ledger.json" || bytes.Contains(file.Data, []byte("PRIVATE_")) {
			t.Fatalf("private ledger entered audit artifact %s", file.Path)
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
}

func TestReviewHistoryCheckpointFailureStopsBeforeNextCall(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
			checkpointErr := errors.New("private review checkpoint unavailable")
			calls, checkpoints := 0, 0
			var expected, lastCheckpoint []ReviewEntry
			runner := Runner{WorkRoot: t.TempDir(), recordReview: func(progress Result) error {
				checkpoints++
				if len(progress.Calls) != checkpoints-1 || len(progress.ReviewLedger) != checkpoints-1 || !reflect.DeepEqual(progress.ReviewLedger, expected) || progress.Answer != "" {
					t.Fatalf("checkpoint did not preserve the parsed call prefix: %+v", progress)
				}
				lastCheckpoint = append([]ReviewEntry(nil), progress.ReviewLedger...)
				if checkpoints == failAt+1 {
					return checkpointErr
				}
				return nil
			}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				entry := ReviewEntry{Role: request.role, Round: request.round, ReviewPhase: request.reviewPhase}
				answer := ""
				if request.role == roleMain {
					entry.Notes = "PRIVATE_CHECKPOINT_NOTES"
					answer = proposalJSON("Public answer", entry.Notes)
				} else {
					entry.HasBlockers, entry.Feedback = true, "PRIVATE_CHECKPOINT_BLOCKER"
					answer = fmt.Sprintf(`{"approved":false,"has_blockers":true,"feedback":%q}`, entry.Feedback)
				}
				expected = append(expected, entry)
				return aicli.Result{Answer: answer}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, checkpointErr) || calls != failAt || checkpoints != failAt+1 || result.Answer != "" || !reflect.DeepEqual(result.ReviewLedger, expected) || !reflect.DeepEqual(lastCheckpoint, expected) {
				t.Fatalf("checkpoint failure did not stop execution with retained evidence: calls=%d checkpoints=%d result=%+v error=%v", calls, checkpoints, result, err)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("checkpoint failure lost retainable evidence: %v", err)
			}
		})
	}
}

func TestReviewHistoryObserverCannotMutateApplicationEvidence(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
	var expected []ReviewEntry
	checkpoints := 0
	tokens := usage.Tokens{Input: 7, Output: 2}
	runner := Runner{WorkRoot: t.TempDir(), recordReview: func(progress Result) error {
		checkpoints++
		if !reflect.DeepEqual(progress.ReviewLedger, expected) || len(progress.Calls) != checkpoints-1 {
			t.Fatalf("earlier observer changed the next checkpoint: %+v", progress)
		}
		for i := range progress.Calls {
			if progress.Calls[i].Tokens != tokens || progress.Calls[i].Round > 2 {
				t.Fatalf("observer changed accounted calls: %+v", progress.Calls)
			}
			progress.Calls[i].Tokens.Input = 999
			progress.Calls[i].Round = 99
		}
		for i := range progress.ReviewLedger {
			progress.ReviewLedger[i].Notes = "OBSERVER_CHANGED_PRIVATE_EVIDENCE"
			progress.ReviewLedger[i].Approved = !progress.ReviewLedger[i].Approved
		}
		progress.Diagnostics = append(progress.Diagnostics, Diagnostic{Code: "observer_changed", Message: "Observer-local diagnostic"})
		progress.Diagnostics[0].Message = "OBSERVER_CHANGED_DIAGNOSTIC"
		return nil
	}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		entry := ReviewEntry{Role: request.role, Round: request.round, ReviewPhase: request.reviewPhase}
		var answer string
		if request.role == roleMain {
			entry.Notes = fmt.Sprintf("PRIVATE_MAIN_EVIDENCE_%d", request.round)
			answer = proposalJSON("Public answer", entry.Notes)
		} else {
			entry.Approved = request.round > 1
			entry.HasBlockers = !entry.Approved
			if !entry.Approved {
				entry.Feedback = "PRIVATE_BLOCKER_REQUIRING_REVISION"
			}
			answer = fmt.Sprintf(`{"approved":%t,"has_blockers":%t,"feedback":%q}`, entry.Approved, entry.HasBlockers, entry.Feedback)
		}
		expected = append(expected, entry)
		return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: true}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || checkpoints != 6 || !reflect.DeepEqual(result.ReviewLedger, expected) || len(result.Diagnostics) != 0 || result.Answer != "Public answer" {
		t.Fatalf("observer changed authoritative result: checkpoints=%d result=%+v error=%v", checkpoints, result, err)
	}
	for _, call := range result.Calls {
		if call.Tokens != tokens || call.Round > 2 {
			t.Fatalf("final observer changed accounted call: %+v", call)
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("observer changed validated evidence: %v", err)
	}
}
