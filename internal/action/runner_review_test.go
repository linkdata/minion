package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

type cleanupRetryLog struct {
	bytes.Buffer
	root string
	err  error
}

func (w *cleanupRetryLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("cleanup retry scheduled")) {
		w.err = os.Chmod(w.root, 0700)
	}
	return w.Buffer.Write(p)
}

func TestRunnerCleanupRetryRecoveryKeepsValidatedResult(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	for _, kind := range []string{"history", "snapshot"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Cleanup(func() { _ = os.Chmod(root, 0700) })
			journal := &cleanupRetryLog{root: root}
			job := withClaudeCritic(portableJob(command.Comment))
			job.CriticMaxRounds = 2
			if kind == "snapshot" {
				job.Action, job.Command = command.Fix, "fix"
			}
			calls := 0
			runner := Runner{WorkRoot: root, Logger: log.New(journal, "", 0), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == roleMain {
					if kind == "history" && request.round == 2 {
						return aicli.Result{Answer: proposalJSON("Done.", "Counterevidence.")}, nil
					}
					return aicli.Result{Answer: proposalJSON("Done.", "")}, nil
				}
				if kind == "history" && request.round == 1 {
					return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"Check the concern."}`}, nil
				}
				if kind == "snapshot" || request.reviewPhase == reviewVerification {
					if err := os.Chmod(root, 0500); err != nil {
						t.Fatal(err)
					}
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantCalls := 5
			if kind == "snapshot" {
				wantCalls = 2
			}
			if err != nil || journal.err != nil || calls != wantCalls || result.Answer != "Done." || len(result.Diagnostics) != 0 || strings.Count(journal.String(), "cleanup retry scheduled") != 1 {
				t.Fatalf("recovered cleanup: calls=%d result=%+v err=%v restore=%v log=%q", calls, result, err, journal.err, journal.String())
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("retry leaked workspaces: %v,%v", entries, err)
			}
		})
	}
}

func TestRunnerAuditValidationCleanupRestoresRestrictedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\ncase \"$*\" in *validate-coverage-ledger.cjs*) /bin/chmod 000 .;; esac\nexit 0\n")
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	root := t.TempDir()
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{Answer: "Audit report\n"}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "Audit report\n" || len(result.Artifacts) != len(auditArtifacts) || len(result.Diagnostics) != 0 {
		t.Fatalf("cleanup discarded validated audit: %+v,%v", result, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("validation directory survived cleanup: %v,%v", entries, err)
	}
}

func TestRunnerReviewRevisionsFreezeCleanIndependentInputs(t *testing.T) {
	job := portableJob(command.Fix)
	job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 2
	root := t.TempDir()
	var source, firstReview, revisionReview string
	var events []string
	runner := Runner{WorkRoot: root}
	runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
		events = append(events, fmt.Sprintf("%s:%d:%s", request.role, request.round, request.reviewPhase))
		if request.role == "main" {
			if source == "" {
				source = request.dir
			} else {
				if request.dir != source {
					t.Fatal("revision lost the original writable source")
				}
				if _, err := os.Stat(firstReview); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("old critic snapshot survived into main revision: %v", err)
				}
			}
			writeFixtureFile(t, source, "source.txt", []byte(fmt.Sprintf("candidate %d", request.round)))
			notes := ""
			if request.round > 1 {
				notes = fmt.Sprintf("PRIVATE REFUTATION %d", request.round)
			}
			return aicli.Result{Answer: proposalJSON(fmt.Sprintf("answer %d", request.round), notes)}, nil
		}
		if request.dir == source || request.writable || request.outputDir != "" {
			t.Fatalf("critic received mutable main workspace: dir=%q writable=%t output=%q", request.dir, request.writable, request.outputDir)
		}
		data, err := workspace.ReadRegular(filepath.Join(request.dir, "source.txt"), workspace.MaxBytes)
		if err != nil || string(data) != fmt.Sprintf("candidate %d", request.round) {
			t.Fatalf("critic source=%q,%v", data, err)
		}
		if request.round == 1 {
			firstReview = request.dir
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"Implement the missing edge case."}`}, nil
		}
		if request.reviewPhase == "verification" {
			revisionReview = request.dir
			checkRefutations(t, request, []Refutation{{Round: 2, Text: "PRIVATE REFUTATION 2"}})
			if revisionReview == firstReview || !strings.Contains(request.prompt, "Implement the missing edge case.") {
				t.Fatal("verification lacks a fresh revision snapshot or its private evidence")
			}
		} else if request.dir != revisionReview {
			t.Fatal("unchanged verification/full phases copied different candidates")
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "answer 2" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "candidate 2" {
		t.Fatalf("revised result=%+v,%v", result, err)
	}
	want := []string{"main:1:", "critic:1:full", "main:2:", "critic:2:verification", "critic:2:full"}
	if !slices.Equal(events, want) || len(result.Calls) != len(want) {
		t.Fatalf("review sequence=%q calls=%+v", events, result.Calls)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("review workspaces leaked=%v,%v", entries, err)
	}
}

func TestRunnerReviewExportsFrozenSourceDespiteLiveChanges(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%t", approved), func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Fix))
			job.CriticMaxRounds = 2
			var source string
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == "main" {
					source = request.dir
					writeFixtureFile(t, source, "source.txt", []byte(fmt.Sprintf("candidate %d", request.round)))
					return aicli.Result{Answer: proposalJSON("answer", "")}, nil
				}
				writeFixtureFile(t, source, "source.txt", []byte("background edit"))
				if data, err := workspace.ReadRegular(filepath.Join(request.dir, "source.txt"), workspace.MaxBytes); err != nil || string(data) != fmt.Sprintf("candidate %d", request.round) {
					t.Fatalf("live mutation reached frozen copy: %q, %v", data, err)
				}
				feedback := ""
				accepted := approved || request.round > 1
				if !accepted {
					feedback = "Fix the candidate."
				}
				return aicli.Result{Answer: fmt.Sprintf(`{"approved":%t,"has_blockers":%t,"feedback":%q}`, accepted, !accepted, feedback)}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantCalls, round := 2, 1
			if !approved {
				wantCalls, round = 5, 2
			}
			if err != nil || calls != wantCalls || result.Answer != "answer" || len(result.Changes) != 1 || string(result.Changes[0].Data) != fmt.Sprintf("candidate %d", round) {
				t.Fatalf("concurrent mutation result=%+v calls=%d error=%v", result, calls, err)
			}
		})
	}
}

func TestRunnerAuditReviewFreezesAndExportsRawReports(t *testing.T) {
	realNode, err := exec.LookPath("node")
	for _, scenario := range []string{"approved", "mutated during critic", "mutated during validation", "empty architecture repaired"} {
		t.Run(scenario, func(t *testing.T) {
			if err != nil {
				t.Skip("audit validators require node")
			}
			installActionTestSkills(t, testAuditSkillURL)
			job := portableJob(command.SecurityAudit)
			job.Main.Skills, job.Critic = []string{testAuditSkillURL}, &RoleSettings{Provider: "claude"}
			job.CriticMaxRounds = config.DefaultCriticMaxRounds
			var source, reports string
			var rawMetadata []byte
			criticCalls := 0
			var armedReports, changed string
			if scenario == "mutated during validation" {
				markers := t.TempDir()
				armedReports, changed = filepath.Join(markers, "reports"), filepath.Join(markers, "changed")
				script := fmt.Sprintf("#!/bin/sh\nreports=$(cat %s)\nprintf 'late report edit' > \"$reports/REPORT.md\"\nprintf changed > %s\nexec %s \"$@\"\n", shellQuote(armedReports), shellQuote(changed), shellQuote(realNode))
				fakeOnPath(t, "node", script)
			}
			runner := Runner{WorkRoot: t.TempDir()}
			runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == "main" {
					if armedReports != "" {
						if err := os.WriteFile(armedReports, []byte(request.outputDir), 0600); err != nil {
							t.Fatal(err)
						}
					}
					source, reports = request.dir, request.outputDir
					for _, file := range auditArtifactsFixture(job, source, "approved report") {
						if file.Path == "run-metadata.json" {
							rawMetadata = file.Data
						}
						if scenario == "empty architecture repaired" && request.round == 1 && file.Path == "architecture.md" {
							file.Data = nil
						}
						writeFixtureFile(t, reports, file.Path, file.Data)
					}

					return aicli.Result{Answer: proposalJSON("main response is not the published report", "")}, nil
				}
				criticCalls++
				var frozenReports string
				for _, dir := range request.readOnlyDirs {
					if strings.HasPrefix(filepath.Base(dir), ".minion-review-reports-") {
						frozenReports = dir
					}
				}
				if request.dir != source || request.outputDir != "" || request.writable || frozenReports == "" || frozenReports == reports || slices.Contains(request.readOnlyDirs, reports) {
					t.Fatalf("audit critic received mutable grants: dir=%q writable=%t output=%q grants=%q", request.dir, request.writable, request.outputDir, request.readOnlyDirs)
				}
				data, err := workspace.ReadRegular(filepath.Join(frozenReports, "run-metadata.json"), maxAuditArtifactBytes)
				if err != nil || !bytes.Equal(data, rawMetadata) {
					t.Fatalf("critic did not receive raw metadata=%q,%v", data, err)
				}
				if scenario == "empty architecture repaired" && request.round == 1 {
					if data, err := os.ReadFile(filepath.Join(frozenReports, "architecture.md")); err != nil || len(data) != 0 {
						t.Fatalf("critic could not inspect the empty artifact: %q,%v", data, err)
					}
					return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"Fill architecture.md with the required architecture report."}`}, nil
				}
				if scenario == "mutated during critic" {
					writeFixtureFile(t, reports, "REPORT.md", []byte("concurrent report edit"))
					data, err := workspace.ReadRegular(filepath.Join(frozenReports, "REPORT.md"), maxOutputBytes)
					if err != nil || string(data) != "approved report" {
						t.Fatalf("concurrent report edit reached frozen copy=%q,%v", data, err)
					}
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}
			result, err := runner.Run(context.Background(), job)
			wantCritics := 1
			if scenario == "empty architecture repaired" {
				wantCritics = 3
			}
			if err != nil || result.Answer != "approved report" || criticCalls != wantCritics {
				t.Fatalf("approved audit result=%+v critics=%d error=%v", result, criticCalls, err)
			}
			if changed != "" {
				if _, err := os.Stat(changed); err != nil {
					t.Fatalf("late report mutation did not execute: %v", err)
				}
			}
			for _, artifact := range result.Artifacts {
				if artifact.Path == "REPORT.md" && string(artifact.Data) != "approved report" {
					t.Fatalf("exported mutable live report: %q", artifact.Data)
				}
			}
		})
	}
}

func TestRunnerReviewRejectsInvalidFixBeforeCritic(t *testing.T) {
	for _, scenario := range []string{"omitted path", "directory spelling collision"} {
		t.Run(scenario, func(t *testing.T) {
			job := withClaudeCritic(portableJob(command.Fix))
			name, want := "omitted", "omitted source path"
			if scenario == "omitted path" {
				job.Source.Omitted = []string{name}
			} else {
				job.Source.Files = []workspace.File{{Path: "Docs/stay", Mode: workspace.Regular}}
				name, want = "docs/new", "directory spelling collision"
			}
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role != "main" {
					t.Fatal("critic ran on an invalid result layout")
				}
				writeFixtureFile(t, request.dir, name, []byte("invalid change"))
				return aicli.Result{Answer: proposalJSON("candidate", "")}, nil
			}}
			if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), want) || calls != 1 {
				t.Fatalf("invalid layout calls=%d error=%v", calls, err)
			}
		})
	}
}
