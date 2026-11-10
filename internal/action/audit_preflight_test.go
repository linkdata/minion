package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

func TestAuditPublisherChecksBundledDependencies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("security audits are unsupported on Windows")
	}
	for _, name := range []string{"report-schema.json", "ATTACK-CLASSES.md", "WEB-PROTOCOL-AND-AUTH.md"} {
		t.Run(name, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			if err := skills.InstallFor(t.Context(), testAuditSkillURL, "codex"); err != nil {
				t.Fatal(err)
			}
			fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
			root := t.TempDir()
			cfg := config.Config{Main: "codex", Commands: map[string]config.CommandMapping{
				"audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
			}, Repositories: []config.Repository{{Actions: []string{"audit"}}}}
			if err := CheckAuditPublisher(t.Context(), root, cfg); err != nil {
				t.Fatalf("installed publisher rejected: %v", err)
			}
			path := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit", name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := CheckAuditPublisher(t.Context(), root, cfg); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing validator dependency accepted: %v", err)
			}
			if dirs, err := filepath.Glob(filepath.Join(root, ".minion-audit-preflight-*")); err != nil || len(dirs) != 0 {
				t.Fatalf("publisher preflight left scratch: dirs=%v error=%v", dirs, err)
			}
		})
	}
}

func TestAuditPublisherCancelsHungNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("security audits are unsupported on Windows")
	}
	installActionTestSkills(t, testAuditSkillURL)
	if err := skills.InstallFor(t.Context(), testAuditSkillURL, "codex"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "node-started")
	fakeOnPath(t, "node", "#!/bin/sh\nprintf started > "+shellQuote(marker)+"\nexec /bin/sleep 30\n")
	root := t.TempDir()
	cfg := config.Config{Main: "codex", Commands: map[string]config.CommandMapping{
		"audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
	}, Repositories: []config.Repository{{Actions: []string{"audit"}}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := CheckAuditPublisher(ctx, root, cfg); err == nil {
		t.Fatal("hung node passed publisher preflight")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("node was not started: %v", err)
	}
	if dirs, err := filepath.Glob(filepath.Join(root, ".minion-audit-preflight-*")); err != nil || len(dirs) != 0 {
		t.Fatalf("publisher preflight left scratch: dirs=%v error=%v", dirs, err)
	}
}

func TestAuditPublisherChecksFindingsSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("security audits are unsupported on Windows")
	}
	node, err := aicli.AbsoluteExecutable("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	if _, err := auditNodePermissionFlag(t.Context(), t.TempDir(), node); err != nil {
		t.Skipf("node permission model is unavailable: %v", err)
	}
	for _, test := range []struct{ name, schema string }{
		{"malformed", "{"},
		{"unsupported", `{"unknownKeyword": true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			if err := skills.InstallFor(t.Context(), testAuditSkillURL, "codex"); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			cfg := config.Config{Main: "codex", Commands: map[string]config.CommandMapping{
				"audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
			}, Repositories: []config.Repository{{Actions: []string{"audit"}}}}
			if err := CheckAuditPublisher(t.Context(), root, cfg); err != nil {
				t.Fatalf("valid findings schema rejected: %v", err)
			}
			path := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit", "report-schema.json")
			if err := os.WriteFile(path, []byte(test.schema), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckAuditPublisher(t.Context(), root, cfg); err == nil || !strings.Contains(err.Error(), "check audit findings schema") {
				t.Fatalf("invalid findings schema passed publisher preflight: %v", err)
			}
			if dirs, err := filepath.Glob(filepath.Join(root, ".minion-audit-preflight-*")); err != nil || len(dirs) != 0 {
				t.Fatalf("publisher preflight left scratch: dirs=%v error=%v", dirs, err)
			}
		})
	}
}

func TestAuditPublisherChecksCoverageModule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("audit validators cannot run on Windows")
	}
	node, err := aicli.AbsoluteExecutable("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	if _, err := auditNodePermissionFlag(t.Context(), t.TempDir(), node); err != nil {
		t.Skipf("node permission model is unavailable: %v", err)
	}
	installActionTestSkills(t, testAuditSkillURL)
	if err := skills.InstallFor(t.Context(), testAuditSkillURL, "codex"); err != nil {
		t.Fatal(err)
	}
	skill, installed, err := skills.InstalledPathFor(testAuditSkillURL, "codex")
	if err != nil || !installed {
		t.Fatalf("installed audit skill: installed=%t error=%v", installed, err)
	}
	script := filepath.Join(filepath.Dir(skill), auditValidators[1].script)
	original, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	modified := strings.Replace(string(original), "#!/usr/bin/env node\n", "#!/usr/bin/env node\nrequire('./extra.cjs');\n", 1)
	if err := os.WriteFile(script, []byte(modified), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := config.Config{Main: "codex", Commands: map[string]config.CommandMapping{
		"audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
	}, Repositories: []config.Repository{{Actions: []string{"audit"}}}}
	if err := CheckAuditPublisher(t.Context(), root, cfg); err == nil || !strings.Contains(err.Error(), "validate-coverage-ledger.cjs dependencies") {
		t.Fatalf("missing local module passed publisher preflight: %v", err)
	}
	if dirs, err := filepath.Glob(filepath.Join(root, ".minion-audit-preflight-*")); err != nil || len(dirs) != 0 {
		t.Fatalf("publisher preflight left scratch: dirs=%v error=%v", dirs, err)
	}
}

func TestAuditModulePreflightBeforeLocalModelAndRemotePull(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("audit validators cannot run on Windows")
	}
	node, err := aicli.AbsoluteExecutable("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	if _, err := auditNodePermissionFlag(t.Context(), t.TempDir(), node); err != nil {
		t.Skipf("node permission model is unavailable: %v", err)
	}
	for _, validator := range auditValidators {
		t.Run(validator.script, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			script := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit", validator.script)
			original, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(script, append([]byte("require('./extra.cjs');\n"), original...), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			job := testJob(command.SecurityAudit)
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				calls++
				return aicli.Result{}, nil
			}}
			if _, err := runner.Run(t.Context(), job); err == nil || !strings.Contains(err.Error(), validator.script+" dependencies") || calls != 0 {
				t.Fatalf("missing module reached local model: calls=%d error=%v", calls, err)
			}
			remote := selectedPreflightRunner(t, map[string]config.CommandMapping{
				"audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
			})
			err = remote.PreflightSelection(t.Context())
			requirePreflightMessage(t, err, preflightAuditToolchainUnavailable)
			if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), validator.script+" dependencies") {
				t.Fatalf("remote preflight missed module failure: %v", cause)
			}
			assertEmptyDir(t, remote.WorkRoot)
		})
	}
}

func TestAuditPublisherNeedsOnlyValidatorSkill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("audit validators cannot run on Windows")
	}
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	const workerOnly = "https://github.com/example/skills/tree/main/skills/worker-only"
	cfg := config.Config{Main: "codex", Commands: map[string]config.CommandMapping{
		"audit-with-extra": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL, workerOnly}},
		"audit":            {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
	}, Repositories: []config.Repository{{Actions: []string{"audit-with-extra", "audit"}}}}
	if err := CheckAuditPublisher(t.Context(), t.TempDir(), cfg); err != nil {
		t.Fatalf("worker-only skill disabled publishing host: %v", err)
	}
	job := testJob(command.SecurityAudit)
	job.Main.Skills = append(job.Main.Skills, workerOnly)
	job.Identity.Repository = "o/r"
	store, _, request := savedRecoveryFixture(t, job, completeAuditResult(job))
	var posts atomic.Int32
	server := auditRecoveryServer(t, &posts)
	orchestrator := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8, WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 {
		t.Fatalf("worker-only skill blocked saved audit: posts=%d error=%v", posts.Load(), err)
	}
}

func TestSelfContainedAuditValidatorsNeedNoBundledSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("security audits are unsupported on Windows")
	}
	dir := t.TempDir()
	for _, validator := range auditValidators {
		if err := os.WriteFile(filepath.Join(dir, validator.script), []byte("// report-schema.json ATTACK-CLASSES.md safe-input.cjs strict-json-keys.cjs\nprocess.exit(0)\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	node := filepath.Join(fakeOnPath(t, "node", "#!/bin/sh\ncase \" $* \" in *' --check '*) exit 0;; esac\nexit 1\n"), "node")
	if err := checkAuditValidatorPrerequisites(t.Context(), t.TempDir(), filepath.Join(dir, "SKILL.md"), auditTools{binary: node, permissionFlag: "--permission"}); err != nil {
		t.Fatalf("self-contained validators were probed for a bundled schema: %v", err)
	}
}

func TestRunnerChecksAuditPrerequisitesBeforeModel(t *testing.T) {
	for _, scenario := range []struct{ name, script, node, want string }{
		{"missing findings validator", "validate-findings.cjs", "valid", "audit validator"},
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
			gitPath := gitOnlyPath(t)
			bin := fakeOnPath(t, "node", script)
			switch scenario.node {
			case "missing":
				t.Setenv("PATH", gitPath)
			case "relative":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(cwd, bin)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", relative+string(os.PathListSeparator)+gitPath)
				t.Setenv("GODEBUG", "execerrdot=0")
				if binary, err := exec.LookPath("node"); err != nil || filepath.IsAbs(binary) {
					t.Fatalf("relative executable fixture failed: binary=%q error=%v", binary, err)
				}
			}
			job := testJob(command.SecurityAudit)
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
					if err := os.WriteFile(filepath.Join(request.outputDir, file.Path), file.Data, 0600); err != nil {
						return aicli.Result{}, err
					}
				}
				return aicli.Result{Answer: "Audit report\n", UsageComplete: true}, nil
			}}
			result, err := runner.Run(t.Context(), job)
			if scenario.want == "" {
				if err != nil || calls != 1 || len(result.Calls) != 1 || result.Answer != "" {
					t.Fatalf("valid tools refused: calls=%d result=%+v error=%v", calls, result, err)
				}
				data, err := os.ReadFile(marker)
				if err != nil || !strings.Contains(string(data), "validate-findings.cjs") || !strings.Contains(string(data), "validate-coverage-ledger.cjs") {
					t.Fatalf("valid audit did not run both validators: calls=%q error=%v", data, err)
				}
				if strings.Count(string(data), " -e ") != 1 {
					t.Fatalf("audit repeated the permission probe: %q", data)
				}
				if strings.Count(string(data), " --source-only") != 2 {
					t.Fatalf("audit did not pass source-only mode to findings validator: %q", data)
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

func TestRunnerChecksAuditSchemaBeforeModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("security audits are unsupported on Windows")
	}
	node, err := aicli.AbsoluteExecutable("node")
	if err != nil {
		t.Skip("audit validator requires node")
	}
	if _, err := auditNodePermissionFlag(t.Context(), t.TempDir(), node); err != nil {
		t.Skipf("node permission model is unavailable: %v", err)
	}
	installActionTestSkills(t, testAuditSkillURL)
	if err := skills.InstallFor(t.Context(), testAuditSkillURL, "codex"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit", "report-schema.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		calls++
		return aicli.Result{}, nil
	}}
	result, err := runner.Run(t.Context(), testJob(command.SecurityAudit))
	if err == nil || !strings.Contains(err.Error(), "check audit findings schema") || errors.Is(err, monitor.ErrTerminalJob) || calls != 0 || len(result.Calls) != 0 {
		t.Fatalf("invalid schema reached model: calls=%d recorded=%d error=%v", calls, len(result.Calls), err)
	}
}

func TestAuditMissingInputsFailBeforeValidatorExecution(t *testing.T) {
	for _, name := range auditArtifacts {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%t", name, empty), func(t *testing.T) {
				job := testJob(command.SecurityAudit)
				files := auditArtifactsFixture(job, "/source", "Audit report\n")
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
				allowed := empty && auditArtifactMayBeEmpty(name)
				marker := filepath.Join(t.TempDir(), "node-called")
				nodeDir := fakeOnPath(t, "node", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 0\n")
				err := validateAuditFiles(t.Context(), supplied, t.TempDir(), "/installed/SKILL.md", auditTools{binary: filepath.Join(nodeDir, "node"), permissionFlag: "--permission"}, job.Identity.Repository, "/source", job.Source.Head, nil)
				if allowed {
					if err != nil {
						t.Fatalf("allowed empty %s rejected: %v", name, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), name+" is missing or empty") {
					t.Fatalf("missing input accepted: error=%v", err)
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
		job := testJob(command.SecurityAudit)
		files := auditArtifactsFixture(job, "/source", "Audit report\n")
		for i := range files {
			if files[i].Path == "REPORT.md" {
				files[i].Data = []byte(" \t\r\n")
			}
		}
		marker := filepath.Join(t.TempDir(), "node-called")
		nodeDir := fakeOnPath(t, "node", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 99\n")
		err := validateAuditFiles(t.Context(), files, t.TempDir(), "/installed/SKILL.md", auditTools{binary: filepath.Join(nodeDir, "node"), permissionFlag: "--permission"}, job.Identity.Repository, "/source", job.Source.Head, nil)
		if err == nil || !strings.Contains(err.Error(), "REPORT.md is missing or empty") {
			t.Fatalf("blank report accepted: %v", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("blank report invoked node: %v", err)
		}
	})
}

func TestRunnerMissingAuditArtifactSkipsValidator(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	marker := filepath.Join(t.TempDir(), "validator-called")
	fakeOnPath(t, "node", "#!/bin/sh\ncase \" $* \" in *' -e '*|*' --check '*|*validator-probe.json*) exit 0;; esac\nprintf called > "+shellQuote(marker)+"\nexit 99\n")
	job := testJob(command.SecurityAudit)
	runner := Runner{
		WorkRoot: t.TempDir(),
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
				if file.Path == "REPORT.md" {
					continue
				}
				if err := os.WriteFile(filepath.Join(request.outputDir, file.Path), file.Data, 0600); err != nil {
					return aicli.Result{}, err
				}
			}
			return aicli.Result{Answer: "Audit report\n"}, nil
		},
	}
	result, err := runner.Run(t.Context(), job)
	if err == nil || !strings.Contains(err.Error(), "audit REPORT.md is missing or empty") || len(result.Calls) != 1 {
		t.Fatalf("missing artifact was accepted before validation: calls=%d error=%v", len(result.Calls), err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing report invoked validator: %v", err)
	}
}

func TestAuditValidatorDiagnosticsStayOutOfErrors(t *testing.T) {
	job := testJob(command.SecurityAudit)
	const diagnostic = "PRIVATE_VALIDATOR_FINDING\nforged journal line \"quoted\"\n"
	script := "#!/bin/sh\nprintf '%s' " + shellQuote(diagnostic) + " >&2\nexit 23\n"
	nodeDir := fakeOnPath(t, "node", script)
	err := validateAuditFiles(t.Context(), auditArtifactsFixture(job, "/source", "Audit report\n"), t.TempDir(), "/installed/SKILL.md", auditTools{binary: filepath.Join(nodeDir, "node"), permissionFlag: "--permission"}, job.Identity.Repository, "/source", job.Source.Head, nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "audit validate-findings.cjs failed") || strings.Contains(err.Error(), "PRIVATE_VALIDATOR_FINDING") || strings.Contains(err.Error(), "forged journal") {
		t.Fatalf("private diagnostic leaked or native failure lost: error=%v", err)
	}
}
