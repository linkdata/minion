package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestOversizedNormalizedMetadataPreservesOtherPartialArtifacts(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	metadata := []byte(fmt.Sprintf(`{"run_id":"%s","repo":%q,"target":"/worker/source","source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, strings.Repeat("<", maxAuditArtifactBytes/5), job.Identity.Repository, job.Source.Head, auditTestOwners))
	if _, err := validateAuditMetadata(metadata, job.Identity.Repository, "/worker/source", job.Source.Head); err != nil {
		t.Fatalf("invalid complete metadata fixture: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatal(err)
	}
	expanded, err := json.Marshal(decoded)
	if err != nil || len(metadata) > maxAuditArtifactBytes || len(expanded) <= maxAuditArtifactBytes {
		t.Fatalf("invalid expansion fixture: raw=%d normalized=%d error=%v", len(metadata), len(expanded), err)
	}
	root := t.TempDir()
	var files []workspace.File
	for _, name := range auditArtifacts {
		data := []byte("retain " + name)
		if name == "run-metadata.json" {
			data = metadata
		}
		files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	const skill = "/installed/security-audit/SKILL.md"
	if normalized, err := normalizeAuditArtifacts(files, job, skill); normalized != nil || err == nil || !strings.Contains(err.Error(), "normalized audit metadata exceeds byte limit") {
		t.Fatalf("oversized normalized metadata accepted: files=%d error=%v", len(normalized), err)
	}
	partial, diagnostics := collectPartialAuditArtifacts(root, job, skill)
	if len(partial) != len(auditArtifacts)-1 || len(diagnostics) != 1 || diagnostics[0].Code != "artifact_metadata_invalid" {
		t.Fatalf("partial collection discarded unrelated artifacts: files=%d diagnostics=%+v", len(partial), diagnostics)
	}
	for _, file := range partial {
		if file.Path == "run-metadata.json" || !bytes.Equal(file.Data, []byte("retain "+file.Path)) {
			t.Fatalf("partial collection changed retained artifact %q", file.Path)
		}
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Artifacts: partial, Diagnostics: diagnostics}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("salvaged partial artifacts cannot cross the result boundary: %v", err)
	}
}

func TestPartialAuditMetadataPathListsStayPortable(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	for _, field := range []string{"scope_paths", "prior_run_paths"} {
		for _, path := range []string{".", "relative/report.md", "/worker/source", "../outside", `bad\path`} {
			data, err := json.Marshal(map[string]any{field: []string{path}})
			if err != nil {
				t.Fatal(err)
			}
			files, err := normalizeAuditArtifacts([]workspace.File{{Path: "run-metadata.json", Mode: workspace.Regular, Data: data}}, job, "/installed/SKILL.md")
			portable := path == "." || path == "relative/report.md"
			if (err == nil) != portable {
				t.Fatalf("partial %s path %q: %v", field, path, err)
			}
			if portable {
				var normalized map[string]json.RawMessage
				if err := json.Unmarshal(files[0].Data, &normalized); err != nil || len(normalized) != 2 {
					t.Fatalf("partial normalization invented missing fields: %s, %v", files[0].Data, err)
				}
			}
		}
	}
}

func TestRunnerRejectsBlankNoChangeFix(t *testing.T) {
	for _, test := range []struct{ name, answer string }{{"empty", ""}, {"whitespace", " \n\t"}, {"NUL", "\x00"}} {
		t.Run(test.name, func(t *testing.T) {
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				return aicli.Result{Answer: test.answer}, nil
			}}
			result, err := runner.Run(context.Background(), portableJob(command.Fix))
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "action answer is blank") || result.Answer != "" || len(result.Changes) != 0 || len(result.Calls) != 1 {
				t.Fatalf("blank no-change fix accepted: %+v, %v", result, err)
			}
		})
	}
}

func TestRunnerAuditCollectionAndNormalizationFailuresAreTerminal(t *testing.T) {
	for _, stage := range []string{"collection", "normalization"} {
		t.Run(stage, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
			job := portableJob(command.SecurityAudit)
			job.Main.Skills = []string{testAuditSkillURL}
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
					if stage == "collection" && file.Path == "REPORT.md" {
						if err := os.Mkdir(filepath.Join(request.outputDir, file.Path), 0700); err != nil {
							t.Fatal(err)
						}
						continue
					}
					if stage == "normalization" && file.Path == "run-metadata.json" {
						original := file.Data
						file.Data = bytes.Replace(file.Data, []byte(`"run_id":"test-run"`), []byte(`"run_id":"`+strings.Repeat("<", maxAuditArtifactBytes/5)+`"`), 1)
						if bytes.Equal(file.Data, original) || len(file.Data) > maxAuditArtifactBytes {
							t.Fatal("normalization fixture must replace run_id and fit the raw collection cap")
						}
					}
					writeFixtureFile(t, request.outputDir, file.Path, file.Data)
				}
				return aicli.Result{Answer: "Audit complete."}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			want := "invalid audit REPORT.md"
			if stage == "normalization" {
				want = "normalized audit metadata exceeds byte limit"
			}
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), want) || result.Answer != "" || len(result.Calls) != 1 {
				t.Fatalf("audit %s error lost terminal boundary: calls=%d answer=%q artifacts=%d error=%v", stage, len(result.Calls), result.Answer, len(result.Artifacts), err)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("failure discarded valid partial artifacts: %v", err)
			}
		})
	}
}

func TestRunnerCancellationDuringInstalledAuditValidatorPreservesCause(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	bin := t.TempDir()
	startedPath, holdPath := filepath.Join(bin, "started"), filepath.Join(bin, "hold")
	for _, name := range []string{startedPath, holdPath} {
		if err := syscall.Mkfifo(name, 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := os.OpenFile(startedPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer marker.Close()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = -e ]; then exit 0; fi\nprintf started > %q\nexec /bin/cat < %q\n", startedPath, holdPath)
	fakeOnPath(t, "node", script)
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			if err := os.WriteFile(filepath.Join(request.outputDir, file.Path), file.Data, 0600); err != nil {
				return aicli.Result{}, err
			}
		}
		return aicli.Result{Answer: "completed model answer"}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		var signal [7]byte
		_, err := io.ReadFull(marker, signal[:])
		if err == nil && string(signal[:]) != "started" {
			err = fmt.Errorf("unexpected validator marker %q", signal)
		}
		started <- err
	}()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runner.Run(ctx, job)
		done <- outcome{result, err}
	}()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case early := <-done:
		t.Fatalf("runner stopped before validator entry: %v", early.err)
	case <-guard.C:
		t.Fatal("validator did not start")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrTerminalJob) || !strings.Contains(got.err.Error(), "audit validate-findings.cjs failed") || len(got.result.Calls) != 1 || got.result.Answer != "" {
			t.Fatalf("validator cancellation lost cause or publishable boundary: calls=%d answer=%q error=%v", len(got.result.Calls), got.result.Answer, got.err)
		}
	case <-guard.C:
		t.Fatal("cancelled validator did not stop")
	}
}

func TestRunnerMissingConfiguredReadGrantIsRetryableBeforeModels(t *testing.T) {
	for _, scenario := range []struct{ provider, role string }{{"codex", "main"}, {"claude", "critic"}} {
		provider, role := scenario.provider, scenario.role
		t.Run(provider+"/"+role, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Main.Provider = provider
			if role == "critic" {
				job.Critic = &RoleSettings{Provider: provider}
				job.CriticMaxRounds = config.DefaultCriticMaxRounds
				job.Main.Provider = "claude"
				if provider == "claude" {
					job.Main.Provider = "codex"
				}
			}
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				calls++
				return aicli.Result{}, nil
			}}
			missing := filepath.Join(t.TempDir(), "missing-grant")
			if provider == "codex" {
				runner.Codex.ReadOnlyDirs = []string{missing}
			} else {
				runner.Claude.ReadOnlyDirs = []string{missing}
			}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, os.ErrNotExist) || errors.Is(err, monitor.ErrTerminalJob) || calls != 0 || len(result.Calls) != 0 {
				t.Fatalf("missing host grant result: native calls=%d recorded calls=%d error=%v", calls, len(result.Calls), err)
			}
		})
	}
}

func TestRunnerReviewsAndPublishesNormalizedAnswer(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	mainCalls, criticCalls := 0, 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == "main" {
			mainCalls++
			return aicli.Result{Answer: proposalJSON("answer\x00hidden", "")}, nil
		}
		criticCalls++
		if !strings.Contains(request.prompt, "Main model's proposed response (untrusted): "+quotedContext("answerhidden")) || strings.Contains(request.prompt, `answer\u0000hidden`) {
			t.Fatal("critic did not receive the normalized publication candidate")
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || mainCalls != 1 || criticCalls != 1 || len(result.Calls) != 2 || result.Answer != "answerhidden" {
		t.Fatalf("normalized proposal: main=%d critic=%d recorded=%d answer=%q error=%v", mainCalls, criticCalls, len(result.Calls), result.Answer, err)
	}
}

func TestPreparationFailureDistinguishesCapacityFromInvalidInput(t *testing.T) {
	for _, test := range []struct {
		cause    syscall.Errno
		terminal bool
	}{{syscall.ENOSPC, false}, {syscall.EDQUOT, false}, {syscall.ENAMETOOLONG, true}} {
		t.Run(test.cause.Error(), func(t *testing.T) {
			native := &os.PathError{Op: "write", Path: "source", Err: test.cause}
			err := preparationFailure("materialize source", native)
			if !errors.Is(err, test.cause) || errors.Is(err, monitor.ErrTerminalJob) != test.terminal || !strings.Contains(err.Error(), "materialize source") {
				t.Fatalf("preparation classification=%v, want terminal=%t with cause%v", err, test.terminal, test.cause)
			}
		})
	}
}

func TestRunnerDropsNULFromUnreviewedAnswer(t *testing.T) {
	job := portableJob(command.Comment)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		return aicli.Result{Answer: "before\x00after"}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "beforeafter" || len(result.Calls) != 1 {
		t.Fatalf("unreviewed NUL answer=%q error=%v", result.Answer, err)
	}
}

func TestRunnerCancellationBeforeDispatchRecordsNoCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := portableJob(command.Comment)
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) { calls++; return aicli.Result{}, nil }}
	runner.aroundModel = func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
		cancel()
		return next(ctx, request)
	}
	result, err := runner.Run(ctx, job)
	if !errors.Is(err, context.Canceled) || calls != 0 || len(result.Calls) != 0 || result.Answer != "" {
		t.Fatalf("undispatched cancellation: native=%d recorded=%d answer=%q error=%v", calls, len(result.Calls), result.Answer, err)
	}
}

func TestRunnerJobValidationRejectsMetadataAliasBeforeModels(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "GIT~1/x", Mode: workspace.Regular}}
	runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "unsafe fix source path") || len(result.Calls) != 0 {
		t.Fatalf("unsupported source was retryable: %v", err)
	}
}

func TestRunnerReportsOwnedAndCriticCleanupFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	for _, critic := range []bool{false, true} {
		t.Run(fmt.Sprint(critic), func(t *testing.T) {
			job := portableJob(command.Comment)
			if critic {
				job.Action = command.Fix
				job.Command = "fix"
				job = withClaudeCritic(job)
			}
			root := t.TempDir()
			defer func() {
				if err := os.Chmod(root, 0700); err != nil {
					t.Error(err)
				}
			}()
			runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == "main" && critic {
					return aicli.Result{Answer: proposalJSON("Done.", "")}, nil
				}
				if err := os.Chmod(root, 0500); err != nil {
					return aicli.Result{}, err
				}
				if critic {
					return aicli.Result{Answer: approvedCritiqueJSON}, nil
				}
				return aicli.Result{Answer: "Done."}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if err != nil {
				t.Fatal(err)
			}
			workspaceFailures, criticFailures := 0, 0
			for _, diagnostic := range result.Diagnostics {
				if diagnostic.Code == "cleanup_failed" {
					if strings.Contains(diagnostic.Message, "Execution workspace") {
						workspaceFailures++
					}
					if strings.Contains(diagnostic.Message, "Critic snapshot") {
						criticFailures++
					}
				}
			}
			if workspaceFailures != 1 || critic && criticFailures != 1 {
				t.Fatalf("cleanup failure not recorded: %+v", result.Diagnostics)
			}
		})
	}
}
