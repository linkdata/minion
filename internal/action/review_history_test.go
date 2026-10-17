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

func checkRefutations(t *testing.T, request modelRun, want []Refutation) {
	t.Helper()
	if !reflect.DeepEqual(request.refutations, want) {
		t.Fatalf("%s/%d/%s refutations=%+v, want %+v", request.role, request.round, request.reviewPhase, request.refutations, want)
	}
	grants := 0
	for _, dir := range request.readOnlyDirs {
		path := filepath.Join(dir, "refutations.json")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		grants++
		var got []Refutation
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if len(want) == 0 || decoder.Decode(&got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected private refutations grant %s=%s, want %+v", path, data, want)
		}
		if !strings.Contains(request.prompt, fmt.Sprintf("%q", path)) || !strings.Contains(request.prompt, "untrusted evidence") {
			t.Fatalf("unquoted or unclassified refutations path: %s", request.prompt)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("refutations directory privacy: %v,%v", info, err)
		}
		for _, other := range append([]string{request.dir, request.outputDir}, request.readOnlyDirs...) {
			if other != "" && other != dir && (workspace.Within(other, dir) || workspace.Within(dir, other)) {
				t.Fatalf("private refutations overlap another grant: %s,%s", dir, other)
			}
		}
	}
	if (grants == 1) != (len(want) != 0) || grants > 1 {
		t.Fatalf("refutation grants=%d entries=%d", grants, len(want))
	}
	if len(want) == 0 && strings.Contains(request.prompt, "refutations.json") {
		t.Fatal("fresh call received private refutations path")
	}
	for _, root := range []string{request.dir, request.outputDir} {
		if root != "" {
			if _, err := os.Stat(filepath.Join(root, "refutations.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refutations entered source/report directory: %s,%v", root, err)
			}
		}
	}
}

func TestRefutationsSurviveOmissionWithoutRememberingImplementedChanges(t *testing.T) {
	const refutation = "PRIVATE_REFUTATION_A: source.txt is application data; this undeployed project requires no migration."
	const findingA = "PRIVATE_FINDING_A: add a migration."
	const findingB = "PRIVATE_BLOCKER_B: source.txt must contain fixed B."
	const answer = "Fixed the requested source behavior."
	job := withClaudeCritic(portableJob(command.Fix))
	job.CriticMaxRounds, job.CriticNonblockingRounds = 4, 0
	root := t.TempDir()
	var expected []Refutation
	var sequence, paths []string
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		sequence = append(sequence, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
		for _, path := range paths {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("previous refutations grant retained: %s,%v", path, err)
			}
		}
		want := expected
		if request.reviewPhase == reviewFull {
			want = nil
		}
		checkRefutations(t, request, want)
		for _, dir := range request.readOnlyDirs {
			if _, err := os.Stat(filepath.Join(dir, "refutations.json")); err == nil {
				paths = append(paths, dir)
			}
		}
		if request.role == roleMain {
			text, content := "", "initial source\n"
			switch request.round {
			case 1:
				text = " \n\t"
			case 2:
				text = refutation
			case 3:
				content = "still broken B\n"
			case 4:
				content = "fixed B\n"
			}
			writeFixtureFile(t, request.dir, "source.txt", []byte(content))
			if strings.TrimSpace(text) != "" {
				expected = append(expected, Refutation{Round: request.round, Text: text})
			}
			return aicli.Result{Answer: proposalJSON(answer, text)}, nil
		}
		approved, blockers, feedback := false, false, ""
		switch {
		case request.round == 1:
			feedback = findingA
		case request.round == 2 && request.reviewPhase == reviewVerification:
			approved = true
		case request.round == 2 && request.reviewPhase == reviewFull:
			blockers, feedback = true, findingB
		case request.round == 3:
			data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
			if err != nil || string(data) != "still broken B\n" {
				t.Fatalf("wrong candidate: %q,%v", data, err)
			}
			blockers, feedback = true, findingB+" The candidate still needs correction."
		case request.round == 4:
			data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
			if err != nil || string(data) != "fixed B\n" {
				t.Fatalf("blocker not corrected: %q,%v", data, err)
			}
			approved = true
		default:
			t.Fatalf("unexpected critic call: %+v", request)
		}
		return aicli.Result{Answer: fmt.Sprintf(`{"approved":%t,"has_blockers":%t,"feedback":%q}`, approved, blockers, feedback)}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	wantSequence := []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification", "critic/2/full", "main/3/", "critic/3/verification", "main/4/", "critic/4/verification", "critic/4/full"}
	if err != nil || result.Answer != answer || !reflect.DeepEqual(sequence, wantSequence) || len(result.Calls) != 10 || !reflect.DeepEqual(result.Refutations, []Refutation{{Round: 2, Text: refutation}}) {
		t.Fatalf("refutation-only memory/correction lost: %+v sequence=%v error=%v", result, sequence, err)
	}
	if !result.Calls[3].Approved || result.Calls[4].Approved || !result.Calls[4].HasBlockers || result.Calls[6].Approved || !result.Calls[6].HasBlockers || !result.Calls[9].Approved {
		t.Fatalf("refutations suppressed independent findings: %+v", result.Calls)
	}
	if len(result.Changes) != 1 || string(result.Changes[0].Data) != "fixed B\n" || strings.Contains(result.Answer, "PRIVATE_") || len(result.Artifacts) != 0 {
		t.Fatalf("private memory entered publication: %+v", result)
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("owned per-call files retained: %v,%v", entries, err)
	}
}

func TestRefutationsRetainSuccessfulMainPrefixAfterFailure(t *testing.T) {
	for _, failure := range []string{"main provider", "critic provider", "main malformed", "critic malformed", "round limit"} {
		t.Run(failure, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
			providerErr := errors.New("provider stopped after final-looking text")
			var expected []Refutation
			root := t.TempDir()
			runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				want := expected
				if request.reviewPhase == reviewFull {
					want = nil
				}
				checkRefutations(t, request, want)
				if request.role == roleMain {
					text := fmt.Sprintf("PRIVATE_REFUTATION_%d", request.round)
					answer := proposalJSON("Public candidate", text)
					if request.round == 2 {
						if failure == "main provider" {
							return aicli.Result{Answer: answer}, providerErr
						}
						if failure == "main malformed" {
							return aicli.Result{Answer: "unparsed main"}, nil
						}
					}
					expected = append(expected, Refutation{Round: request.round, Text: text})
					return aicli.Result{Answer: answer}, nil
				}
				if request.round == 2 {
					if failure == "critic provider" {
						return aicli.Result{Answer: approvedCritiqueJSON}, providerErr
					}
					if failure == "critic malformed" {
						return aicli.Result{Answer: "unparsed critic"}, nil
					}
				}
				return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"PRIVATE_BLOCKER: correction required."}`}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if err == nil || result.Answer != "" || !reflect.DeepEqual(result.Refutations, expected) {
				t.Fatalf("successful refutations lost/failed text promoted: %+v expected=%+v error=%v", result, expected, err)
			}
			providerFailure := strings.HasSuffix(failure, "provider")
			if errors.Is(err, providerErr) != providerFailure || errors.Is(err, monitor.ErrTerminalJob) != !providerFailure {
				t.Fatalf("failure class changed: %v", err)
			}
			wantRefs, wantCalls := 2, 4
			if strings.HasPrefix(failure, "main ") {
				wantRefs, wantCalls = 1, 3
			}
			if len(result.Refutations) != wantRefs || len(result.Calls) != wantCalls {
				t.Fatalf("prefix length refs=%d calls=%d", len(result.Refutations), len(result.Calls))
			}
			if err := result.validate(job, false); err != nil {
				t.Fatal(err)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatalf("failed execution retained owned files: %v,%v", entries, err)
			}
		})
	}
}

func TestRefutationsRemainPrivateFromAuditArtifacts(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := withClaudeCritic(portableJob(command.SecurityAudit))
	job.Main.Skills = []string{testAuditSkillURL}
	job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
	var expected []Refutation
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		want := expected
		if request.reviewPhase == reviewFull {
			want = nil
		}
		checkRefutations(t, request, want)
		if request.role == roleMain {
			text := ""
			if request.round == 2 {
				text = "PRIVATE_AUDIT_REFUTATION: installed validators and report evidence meet the requirement."
				expected = append(expected, Refutation{Round: 2, Text: text})
			}
			for _, artifact := range auditArtifactsFixture(job, request.dir, "Verified audit report\n") {
				writeFixtureFile(t, request.outputDir, artifact.Path, artifact.Data)
			}
			return aicli.Result{Answer: proposalJSON("Public model summary", text)}, nil
		}
		if request.round == 1 {
			return aicli.Result{Answer: `{"approved":false,"has_blockers":false,"feedback":"PRIVATE_AUDIT_FINDING: report concern."}`}, nil
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || len(result.Refutations) != 1 || !reflect.DeepEqual(result.Refutations, expected) || len(result.Artifacts) != len(auditArtifacts) {
		t.Fatalf("audit refutations=%+v error=%v", result, err)
	}
	if strings.Contains(result.Answer, "PRIVATE_") || result.Answer == "Public model summary" {
		t.Fatalf("private/model-only answer published: %q", result.Answer)
	}
	for _, file := range result.Artifacts {
		if file.Path == "refutations.json" || bytes.Contains(file.Data, []byte("PRIVATE_")) {
			t.Fatalf("private refutations entered artifact %s", file.Path)
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
}

func TestRefutationCheckpointFailureStopsBeforeNextCall(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Comment))
			job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
			checkpointErr := errors.New("private refutation checkpoint unavailable")
			calls, checkpoints := 0, 0
			var expected, lastCheckpoint []Refutation
			runner := Runner{WorkRoot: t.TempDir(), recordReview: func(progress Result) error {
				checkpoints++
				if len(progress.Calls) != checkpoints-1 || !reflect.DeepEqual(progress.Refutations, expected) || progress.Answer != "" {
					t.Fatalf("checkpoint evidence lost: %+v", progress)
				}
				lastCheckpoint = append([]Refutation(nil), progress.Refutations...)
				if checkpoints == failAt+1 {
					return checkpointErr
				}
				return nil
			}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == roleMain {
					expected = append(expected, Refutation{Round: request.round, Text: "PRIVATE_CHECKPOINT_REFUTATION"})
					return aicli.Result{Answer: proposalJSON("Public answer", expected[len(expected)-1].Text)}, nil
				}
				return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"PRIVATE_BLOCKER"}`}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, checkpointErr) || !errors.Is(err, monitor.ErrTerminalJob) || calls != failAt || checkpoints != failAt+1 || result.Answer != "" || !reflect.DeepEqual(result.Refutations, expected) || !reflect.DeepEqual(lastCheckpoint, expected) {
				t.Fatalf("checkpoint failure lost evidence/continued: calls=%d checkpoints=%d result=%+v error=%v", calls, checkpoints, result, err)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefutationObserversCannotMutateApplicationEvidence(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 0
	var expected []Refutation
	checkpoints := 0
	tokens := usage.Tokens{Input: 7, Output: 2}
	runner := Runner{WorkRoot: t.TempDir(), recordReview: func(progress Result) error {
		checkpoints++
		if !reflect.DeepEqual(progress.Refutations, expected) || len(progress.Calls) != checkpoints-1 {
			t.Fatalf("observer changed next checkpoint: %+v", progress)
		}
		for i := range progress.Calls {
			if progress.Calls[i].Tokens != tokens || progress.Calls[i].Round > 2 {
				t.Fatalf("observer changed calls: %+v", progress.Calls)
			}
			progress.Calls[i].Tokens.Input = 999
			progress.Calls[i].Round = 99
		}
		for i := range progress.Refutations {
			progress.Refutations[i].Text = "OBSERVER_CHANGED"
			progress.Refutations[i].Round = 99
		}
		return nil
	}, aroundModel: func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
		for i := range request.refutations {
			request.refutations[i].Text = "OBSERVER_CHANGED"
		}
		return next(ctx, request)
	}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			text := fmt.Sprintf("PRIVATE_REFUTATION_%d", request.round)
			expected = append(expected, Refutation{Round: request.round, Text: text})
			return aicli.Result{Answer: proposalJSON("Public answer", text), Tokens: tokens, UsageComplete: true}, nil
		}
		answer := approvedCritiqueJSON
		if request.round == 1 {
			answer = `{"approved":false,"has_blockers":true,"feedback":"PRIVATE_BLOCKER"}`
		}
		return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: true}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || checkpoints != 6 || !reflect.DeepEqual(result.Refutations, expected) || len(result.Diagnostics) != 0 || result.Answer != "Public answer" {
		t.Fatalf("observer mutated private evidence: checkpoints=%d result=%+v error=%v", checkpoints, result, err)
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
}
