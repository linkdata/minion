package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func auditPreflightArtifacts(job Job, target string) []workspace.File {
	return auditArtifactsFixture(job, target, "Audit report\n")
}

func TestRunnerChecksAuditPrerequisitesBeforeModel(t *testing.T) {
	for _, scenario := range []struct{ name, script, node, want string }{
		{"missing findings validator", "validate-findings.cjs", "valid", "audit validator"},
		{"missing ledger validator", "validate-coverage-ledger.cjs", "valid", "audit validator"},
		{"nonregular validator", "directory", "valid", "not a regular file"},
		{"missing node", "", "missing", "audit requires node"},
		{"unsupported node", "", "unsupported", "permission model is unavailable"},
		{"relative node", "", "relative", "absolute path"},
		{"valid tools", "", "valid", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			skillDir := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit")
			if scenario.script != "" {
				name := scenario.script
				if name == "directory" {
					name = "validate-findings.cjs"
				}
				path := filepath.Join(skillDir, name)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if scenario.script == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			marker := filepath.Join(t.TempDir(), "node-calls")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(marker) + "\nexit 0\n"
			if scenario.node == "unsupported" {
				script = "#!/bin/sh\nexit 1\n"
			}
			bin := fakeOnPath(t, "node", script)
			if scenario.node == "missing" {
				t.Setenv("PATH", t.TempDir())
			} else if scenario.node == "relative" {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(cwd, bin)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", relative)
				t.Setenv("GODEBUG", "execerrdot=0")
				if binary, err := exec.LookPath("node"); err != nil || filepath.IsAbs(binary) {
					t.Fatalf("relative executable fixture failed: binary=%q error=%v", binary, err)
				}
			}
			job := portableJob(command.SecurityAudit)
			job.Main.Skills = []string{testAuditSkillURL}
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				for _, file := range auditPreflightArtifacts(job, request.dir) {
					if err := os.WriteFile(filepath.Join(request.outputDir, file.Path), file.Data, 0600); err != nil {
						return aicli.Result{}, err
					}
				}
				return aicli.Result{Answer: "Audit report\n", UsageComplete: true}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if scenario.want == "" {
				if err != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "Audit report\n" {
					t.Fatalf("valid tools refused: calls=%d result=%+v error=%v", calls, result, err)
				}
				data, err := os.ReadFile(marker)
				if err != nil || !strings.Contains(string(data), "validate-findings.cjs") || !strings.Contains(string(data), "validate-coverage-ledger.cjs") {
					t.Fatalf("valid audit did not run both validators: calls=%q error=%v", data, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.want) || errors.Is(err, monitor.ErrTerminalJob) || calls != 0 || len(result.Calls) != 0 {
				t.Fatalf("invalid tools reached models or were misclassified: calls=%d recorded=%d error=%v", calls, len(result.Calls), err)
			}
			if scenario.script != "" || scenario.node == "relative" {
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused validator tooling executed node: %v", err)
				}
			}
		})
	}
}

func TestAuditMissingInputsFailBeforeValidatorExecution(t *testing.T) {
	for _, name := range auditArtifacts {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%t", name, empty), func(t *testing.T) {
				job := portableJob(command.SecurityAudit)
				files := auditPreflightArtifacts(job, "/source")
				var supplied []workspace.File
				for _, file := range files {
					if file.Path == name {
						if !empty {
							continue
						}
						file.Data = nil
					}
					supplied = append(supplied, file)
				}
				marker := filepath.Join(t.TempDir(), "node-called")
				fakeOnPath(t, "node", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 0\n")
				report, err := validateAuditFiles(context.Background(), supplied, t.TempDir(), "/installed/SKILL.md", job.Identity.Repository, "/source", job.Source.Head)
				allowed := empty && auditArtifactMayBeEmpty(name)
				if allowed {
					if err != nil || report != "Audit report\n" {
						t.Fatalf("allowed empty %s rejected: %q,%v", name, report, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), name+" is missing or empty") || report != "" {
					t.Fatalf("missing input accepted: report=%q error=%v", report, err)
				}
				_, statErr := os.Stat(marker)
				if allowed {
					if statErr != nil {
						t.Fatalf("positive control never invoked validator: %v", statErr)
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("missing input invoked node: %v", statErr)
				}
			})
		}
	}
	t.Run("whitespace report", func(t *testing.T) {
		job := portableJob(command.SecurityAudit)
		files := auditPreflightArtifacts(job, "/source")
		for i := range files {
			if files[i].Path == "REPORT.md" {
				files[i].Data = []byte(" \t\r\n")
			}
		}
		marker := filepath.Join(t.TempDir(), "node-called")
		fakeOnPath(t, "node", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 99\n")
		_, err := validateAuditFiles(context.Background(), files, t.TempDir(), "/installed/SKILL.md", job.Identity.Repository, "/source", job.Source.Head)
		if err == nil || !strings.Contains(err.Error(), "REPORT.md is missing or empty") {
			t.Fatalf("blank report accepted: %v", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("blank report invoked node: %v", err)
		}
	})
}

func TestAuditValidatorDiagnosticsAreBoundedAndQuoted(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	diagnostic := "discarded-prefix\n" + strings.Repeat("x", 10000) + "\nforged journal line \"quoted\"\n"
	script := "#!/bin/sh\nif [ \"$2\" = -e ]; then exit 0; fi\nprintf '%s' " + shellQuote(diagnostic) + " >&2\nexit 23\n"
	fakeOnPath(t, "node", script)
	report, err := validateAuditFiles(context.Background(), auditPreflightArtifacts(job, "/source"), t.TempDir(), "/installed/SKILL.md", job.Identity.Repository, "/source", job.Source.Head)
	var exit *exec.ExitError
	wantTail := fmt.Sprintf("%q", diagnostic[len(diagnostic)-(8<<10):])
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 23 || report != "" || !strings.Contains(err.Error(), "audit validate-findings.cjs failed") || !strings.HasSuffix(err.Error(), wantTail) || strings.Contains(err.Error(), "discarded-prefix") || strings.Contains(err.Error(), "\nforged journal") {
		t.Fatalf("unsafe or unbounded validator diagnostic: report=%q error=%v", report, err)
	}
}
