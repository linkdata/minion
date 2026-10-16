package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

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
			writeIndexFixture(t, source, "source.txt", []byte(fmt.Sprintf("candidate %d", request.round)))
			return aicli.Result{Answer: proposalJSON(fmt.Sprintf("answer %d", request.round), fmt.Sprintf("PRIVATE NOTES %d", request.round))}, nil
		}
		if request.dir == source || request.writable || request.outputDir != "" {
			t.Fatalf("critic received mutable main workspace: dir=%q writable=%t output=%q", request.dir, request.writable, request.outputDir)
		}
		data, err := workspace.ReadRegular(filepath.Join(request.dir, "source.txt"), workspace.MaxBytes)
		if err != nil || string(data) != fmt.Sprintf("candidate %d", request.round) {
			t.Fatalf("critic source=%q,%v", data, err)
		}
		if request.reviewPhase == "full" {
			for _, label := range []string{"Main model's private review notes (untrusted)", "Previous response (untrusted, empty for full reviews)", "Previous critic feedback (untrusted, empty for full reviews)"} {
				if !strings.Contains(request.prompt, label+`: ""`) {
					t.Fatalf("full review was seeded through %q", label)
				}
			}
			if strings.Contains(request.prompt, "PRIVATE NOTES") {
				t.Fatal("private main reasoning seeded a full review")
			}
		}
		if request.round == 1 {
			firstReview = request.dir
			return aicli.Result{Answer: `{"approved":false,"feedback":"Implement the missing edge case."}`}, nil
		}
		if request.reviewPhase == "verification" {
			revisionReview = request.dir
			if revisionReview == firstReview || !strings.Contains(request.prompt, "PRIVATE NOTES 2") || !strings.Contains(request.prompt, "Implement the missing edge case.") {
				t.Fatal("verification lacks a fresh revision snapshot or its private evidence")
			}
		} else if request.dir != revisionReview {
			t.Fatal("unchanged verification/full phases copied different candidates")
		}
		return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
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

func TestRunnerReviewRejectsSourceMutationEvenAfterRejection(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%t", approved), func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Critic = &RoleSettings{Provider: "claude"}
			var source string
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == "main" {
					source = request.dir
					writeIndexFixture(t, source, "source.txt", []byte("candidate"))
					return aicli.Result{Answer: proposalJSON("answer", "")}, nil
				}
				writeIndexFixture(t, source, "source.txt", []byte("background edit"))
				if data, err := workspace.ReadRegular(filepath.Join(request.dir, "source.txt"), workspace.MaxBytes); err != nil || string(data) != "candidate" {
					t.Fatalf("live mutation reached frozen copy: %q, %v", data, err)
				}
				feedback := ""
				if !approved {
					feedback = "Fix the candidate."
				}
				return aicli.Result{Answer: fmt.Sprintf(`{"approved":%t,"feedback":%q}`, approved, feedback)}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "source changed during critic review") || calls != 2 || result.Answer != "" {
				t.Fatalf("concurrent mutation result=%+v calls=%d error=%v", result, calls, err)
			}
		})
	}
}

func TestRunnerReviewExportsApprovedBytesAfterLiveSourceChanges(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	armed, changed := filepath.Join(bin, "armed"), filepath.Join(bin, "changed")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" --binary "*)
    if test -f %q; then
      %q "$@"
      status=$?
      if test "$status" -eq 0; then
        source=$(cat %q)
        printf 'late live edit' > "$source/source.txt"
        printf changed > %q
        rm %q
      fi
      exit "$status"
    fi
    ;;
esac
exec %q "$@"
`, armed, realGit, armed, changed, armed, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	job := portableJob(command.Fix)
	job.Critic = &RoleSettings{Provider: "claude"}
	var source string
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == "main" {
			source = request.dir
			writeIndexFixture(t, source, "source.txt", []byte("approved candidate"))
			return aicli.Result{Answer: proposalJSON("approved answer", "")}, nil
		}
		if err := os.WriteFile(armed, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || len(result.Changes) != 1 || string(result.Changes[0].Data) != "approved candidate" {
		t.Fatalf("export did not use approved captured bytes=%+v,%v", result, err)
	}
	if _, err := os.Stat(changed); err != nil {
		t.Fatalf("late source mutation did not execute: %v", err)
	}
}

func TestRunnerAuditReviewFreezesAndExportsRawReports(t *testing.T) {
	realNode, err := exec.LookPath("node")
	for _, scenario := range []string{"approved", "mutated during critic", "mutated during validation", "empty architecture", "empty architecture repaired"} {
		t.Run(scenario, func(t *testing.T) {
			if err != nil && scenario != "mutated during critic" {
				t.Skip("audit validators require node")
			}
			installActionTestSkills(t, testAuditSkillURL)
			skillDir := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit")
			for _, name := range []string{"validate-findings.cjs", "validate-coverage-ledger.cjs"} {
				if err := os.WriteFile(filepath.Join(skillDir, name), []byte(`JSON.parse(require('fs').readFileSync(process.argv[2], 'utf8'));`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			job := portableJob(command.SecurityAudit)
			job.Main.Skills, job.Critic = []string{testAuditSkillURL}, &RoleSettings{Provider: "claude"}
			var source, reports string
			var rawMetadata []byte
			criticCalls := 0
			var armedReports, changed string
			if scenario == "mutated during validation" {
				bin := t.TempDir()
				armedReports, changed = filepath.Join(bin, "reports"), filepath.Join(bin, "changed")
				script := fmt.Sprintf("#!/bin/sh\nreports=$(cat %q)\nprintf 'late report edit' > \"$reports/REPORT.md\"\nprintf changed > %q\nexec %q \"$@\"\n", armedReports, changed, realNode)
				if err := os.WriteFile(filepath.Join(bin, "node"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
					rawMetadata = []byte(fmt.Sprintf(`{"run_id":"review","repo":%q,"target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, job.Identity.Repository, source, job.Source.Head, auditTestOwners))
					for name, content := range map[string][]byte{
						"run-metadata.json": rawMetadata, "architecture.md": []byte("Architecture"), "coverage-ledger.json": []byte(`[{"status":"covered"}]`),
						"findings.json": []byte("[]"), "REPORT.md": []byte("approved report"), "FINDINGS-DETAIL.md": nil, "NEEDS-VALIDATION.md": nil,
					} {
						if (scenario == "empty architecture" || scenario == "empty architecture repaired" && request.round == 1) && name == "architecture.md" {
							content = nil
						}
						writeIndexFixture(t, reports, name, content)
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
					return aicli.Result{Answer: `{"approved":false,"feedback":"Fill architecture.md with the required architecture report."}`}, nil
				}
				if scenario == "mutated during critic" {
					writeIndexFixture(t, reports, "REPORT.md", []byte("concurrent report edit"))
					data, err := workspace.ReadRegular(filepath.Join(frozenReports, "REPORT.md"), maxOutputBytes)
					if err != nil || string(data) != "approved report" {
						t.Fatalf("concurrent report edit reached frozen copy=%q,%v", data, err)
					}
				}
				return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
			}
			result, err := runner.Run(context.Background(), job)
			if scenario == "mutated during critic" || scenario == "empty architecture" {
				want, calls := "reports changed during critic review", 1
				if scenario == "empty architecture" {
					want, calls = `empty action report artifact "architecture.md"`, 1
				}
				if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), want) || criticCalls != calls || result.Answer != "" {
					t.Fatalf("invalid audit result=%+v critics=%d error=%v", result, criticCalls, err)
				}
				return
			}
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
			job := portableJob(command.Fix)
			job.Critic = &RoleSettings{Provider: "claude"}
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
				writeIndexFixture(t, request.dir, name, []byte("invalid change"))
				return aicli.Result{Answer: proposalJSON("candidate", "")}, nil
			}}
			if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), want) || calls != 1 {
				t.Fatalf("invalid layout calls=%d error=%v", calls, err)
			}
		})
	}
}
