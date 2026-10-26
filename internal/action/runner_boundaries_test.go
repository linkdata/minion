package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
)

func TestPartialAuditMetadataPathListsStayPortable(t *testing.T) {
	job := testJob(command.SecurityAudit)
	skill := filepath.Join(t.TempDir(), "installed", "SKILL.md")
	installed := filepath.Dir(skill)
	for _, field := range []string{"scope_paths", "prior_run_paths"} {
		for _, path := range []string{".", "relative/report.md", "/worker/source", "../outside", `bad\path`} {
			data, err := json.Marshal(map[string]any{field: []string{path}})
			if err != nil {
				t.Fatal(err)
			}
			files, err := normalizeAuditArtifacts([]workspace.File{{Path: "run-metadata.json", Mode: workspace.Regular, Data: data}}, job, skill)
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
	for _, test := range []struct {
		name, path, want, wantErr string
	}{
		{"inside installed skill", filepath.Join(installed, "companion.md"), "companion.md", ""},
		{"portable relative", "extras/companion.md", "extras/companion.md", ""},
		{"outside installed skill", filepath.Join(filepath.Dir(installed), "outside.md"), "", "outside the installed skill"},
		{"absolute escape", installed + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "outside", "", "outside the installed skill"},
		{"relative escape", "../outside", "", "path is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"selected_companion_files": []string{test.path}})
			if err != nil {
				t.Fatal(err)
			}
			files, err := normalizeAuditArtifacts([]workspace.File{{Path: "run-metadata.json", Mode: workspace.Regular, Data: data}}, job, skill)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("companion %q error = %v; want %q", test.path, err, test.wantErr)
				}
				return
			}
			var metadata struct {
				SelectedCompanionFiles []string `json:"selected_companion_files"`
			}
			if err != nil || len(files) != 1 || json.Unmarshal(files[0].Data, &metadata) != nil || !slices.Equal(metadata.SelectedCompanionFiles, []string{test.want}) {
				t.Fatalf("companion %q normalization = %+v, %v", test.path, files, err)
			}
		})
	}
}

func TestPartialAuditMetadataRetainsIncompleteReason(t *testing.T) {
	job := testJob(command.SecurityAudit)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run-metadata.json"), []byte(`{"run_status":"incomplete","incomplete_reason":"validation_budget_exhausted","output_dir":"/worker/local"}`), 0600); err != nil {
		t.Fatal(err)
	}
	files, diagnostics := collectPartialAuditArtifacts(root, job, "/installed/SKILL.md")
	if len(diagnostics) != 0 || len(files) != 1 || files[0].Path != "run-metadata.json" {
		t.Fatalf("partial metadata = %+v, diagnostics = %+v", files, diagnostics)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(files[0].Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if string(metadata["run_status"]) != `"incomplete"` || string(metadata["incomplete_reason"]) != `"validation_budget_exhausted"` {
		t.Fatalf("incomplete reason was lost: %s", files[0].Data)
	}
	if _, present := metadata["output_dir"]; present {
		t.Fatalf("worker-local path retained: %s", files[0].Data)
	}
}

func TestRunnerRejectsBlankNoChangeFix(t *testing.T) {
	for _, test := range []struct{ name, answer string }{{"empty", ""}, {"whitespace", " \n\t"}, {"NUL", "\x00"}} {
		t.Run(test.name, func(t *testing.T) {
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				return aicli.Result{Answer: test.answer}, nil
			}}
			result, err := runner.Run(t.Context(), testJob(command.Fix))
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "action answer is blank") || result.Answer != "" || len(result.Changes) != 0 || len(result.Calls) != 1 {
				t.Fatalf("blank no-change fix accepted: %+v, %v", result, err)
			}
		})
	}
}

func TestRunnerAuditCollectionAndNormalizationFailuresAreTerminal(t *testing.T) {
	for _, stage := range []string{"collection", "normalization"} {
		t.Run(stage, func(t *testing.T) {
			job := auditRunnerJob(t)
			expected := make(map[string]workspace.File)
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
						var decoded map[string]json.RawMessage
						if err := json.Unmarshal(file.Data, &decoded); err != nil {
							t.Fatal(err)
						}
						if expanded, err := json.Marshal(decoded); err != nil || len(expanded) <= maxAuditArtifactBytes {
							t.Fatalf("metadata fixture does not exceed normalized cap: bytes=%d error=%v", len(expanded), err)
						}
					} else {
						expected[file.Path] = file
					}
					writeFixtureFile(t, request.outputDir, file.Path, file.Data)
				}
				return aicli.Result{Answer: "Audit complete."}, nil
			}}
			result, err := runner.Run(t.Context(), job)
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
			if stage == "normalization" {
				if len(result.Artifacts) != len(expected) || len(result.Artifacts) != len(auditArtifacts)-1 {
					t.Fatalf("normalization discarded unrelated artifacts: %d", len(result.Artifacts))
				}
				for _, file := range result.Artifacts {
					want, present := expected[file.Path]
					if !present || file.Mode != want.Mode || !bytes.Equal(file.Data, want.Data) {
						t.Fatalf("normalization changed retained artifact %q", file.Path)
					}
				}
				metadataFailures := 0
				for _, diagnostic := range result.Diagnostics {
					if diagnostic.Code == "artifact_metadata_invalid" {
						metadataFailures++
					}
				}
				if metadataFailures != 1 {
					t.Fatalf("missing metadata failure: %+v", result.Diagnostics)
				}
			}
		})
	}
}

func TestRunnerInvalidReviewedAuditMetadataSkipsCritic(t *testing.T) {
	job := withClaudeCritic(auditRunnerJob(t))
	criticCalls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleCritic {
			criticCalls++
			return aicli.Result{Answer: approvedCritiqueJSON}, nil
		}
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			if file.Path == "run-metadata.json" {
				file.Data = bytes.Replace(file.Data, []byte(`"run_id":"test-run"`), []byte(`"run_id":"`+strings.Repeat("<", maxAuditArtifactBytes/5)+`"`), 1)
			}
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{Answer: proposalJSON("main response", "")}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "normalized audit metadata exceeds byte limit") || criticCalls != 0 || len(result.Calls) != 1 {
		t.Fatalf("invalid audit metadata spent a critic call: calls=%d receipts=%d error=%v", criticCalls, len(result.Calls), err)
	}
}

func TestRunnerInvalidSecondAuditMetadataSkipsVerification(t *testing.T) {
	job := withClaudeCritic(auditRunnerJob(t))
	criticCalls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleCritic {
			criticCalls++
			return aicli.Result{Answer: `{"review_complete":true,"approved":false,"has_blockers":true,"feedback":"Revise the report."}`}, nil
		}
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			if request.round == 2 && file.Path == "run-metadata.json" {
				file.Data = bytes.Replace(file.Data, []byte(`"run_id":"test-run"`), []byte(`"run_id":"`+strings.Repeat("<", maxAuditArtifactBytes/5)+`"`), 1)
			}
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{Answer: proposalJSON("main response", "")}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "normalized audit metadata exceeds byte limit") || criticCalls != 1 || len(result.Calls) != 3 {
		t.Fatalf("invalid round-two audit spent verification: critic calls=%d receipts=%d error=%v", criticCalls, len(result.Calls), err)
	}
}

func TestRunnerRejectsHostReadGrantBeforeModels(t *testing.T) {
	for _, scenario := range []struct{ provider, role string }{{"codex", "main"}, {"claude", "critic"}} {
		provider, role := scenario.provider, scenario.role
		t.Run(provider+"/"+role, func(t *testing.T) {
			job := testJob(command.Comment)
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
			result, err := runner.Run(t.Context(), job)
			if errors.Is(err, workroot.ErrUnsafe) || !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "model evidence directories must be selected by Minion") || calls != 0 || len(result.Calls) != 0 {
				t.Fatalf("unsupported host grant result: native calls=%d recorded calls=%d error=%v", calls, len(result.Calls), err)
			}
		})
	}
}

func TestRunnerStripsNULBeforeCriticAndResult(t *testing.T) {
	for _, critic := range []bool{false, true} {
		t.Run(fmt.Sprintf("critic=%t", critic), func(t *testing.T) {
			job := testJob(command.Comment)
			if critic {
				job = withClaudeCritic(job)
			}
			mainCalls, criticCalls := 0, 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == "main" {
					mainCalls++
					if !critic {
						return aicli.Result{Answer: "answer\x00hidden"}, nil
					}
					return aicli.Result{Answer: proposalJSON("answer\x00hidden", "")}, nil
				}
				criticCalls++
				if !strings.Contains(request.prompt, "Main model's proposed response (untrusted): "+quotedContext("answerhidden")) {
					t.Fatal("critic did not receive the normalized publication candidate")
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}}
			result, err := runner.Run(t.Context(), job)
			wantCritic := 0
			if critic {
				wantCritic = 1
			}
			if err != nil || mainCalls != 1 || criticCalls != wantCritic || len(result.Calls) != 1+wantCritic || result.Answer != "answerhidden" {
				t.Fatalf("normalized proposal: main=%d critic=%d recorded=%d answer=%q error=%v", mainCalls, criticCalls, len(result.Calls), result.Answer, err)
			}
		})
	}
}

func TestPreparationFailureDistinguishesCapacityFromInvalidInput(t *testing.T) {
	for _, test := range []struct {
		cause    error
		terminal bool
	}{{syscall.ENOSPC, false}, {syscall.EDQUOT, false}, {syscall.EACCES, false}, {os.ErrPermission, false}, {syscall.EROFS, false}, {syscall.ENAMETOOLONG, true}, {exec.ErrNotFound, false}} {
		t.Run(test.cause.Error(), func(t *testing.T) {
			native := &os.PathError{Op: "write", Path: "source", Err: test.cause}
			err := preparationFailure("materialize source", native)
			unsafeRoot := test.cause == syscall.EACCES || test.cause == os.ErrPermission || test.cause == syscall.EROFS
			if !errors.Is(err, test.cause) || errors.Is(err, monitor.ErrTerminalJob) != test.terminal || errors.Is(err, workroot.ErrUnsafe) != unsafeRoot || !strings.Contains(err.Error(), "materialize source") {
				t.Fatalf("preparation classification=%v, want terminal=%t unsafeRoot=%t with cause%v", err, test.terminal, unsafeRoot, test.cause)
			}
		})
	}
}

func TestRunnerCancellationBeforeDispatchRecordsNoCall(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job := testJob(command.Comment)
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) { calls++; return aicli.Result{}, nil }}
	runner.aroundModel = func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
		cancel()
		return next(ctx, request)
	}
	result, err := runner.Run(ctx, job)
	if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || calls != 0 || len(result.Calls) != 0 || result.Answer != "" {
		t.Fatalf("undispatched cancellation: native=%d recorded=%d answer=%q error=%v", calls, len(result.Calls), result.Answer, err)
	}
}

func TestRunnerJobValidationRejectsMetadataAliasBeforeModels(t *testing.T) {
	job := testJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "GIT~1/x", Mode: workspace.Regular}}
	runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
	result, err := runner.Run(t.Context(), job)
	preflight := workspace.ValidateSnapshot(job.Source)
	if !errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 ||
		(preflight == nil && (!errors.Is(err, gitwork.ErrUnsupportedSource) || !strings.Contains(err.Error(), "unsafe fix source path"))) ||
		(preflight != nil && !strings.Contains(err.Error(), preflight.Error())) {
		t.Fatalf("unsupported source was retryable: %v", err)
	}
}

func TestRunnerRemovesRestrictedModelDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires effective POSIX directory permissions and a non-root user")
	}
	root := t.TempDir()
	var restricted string
	t.Cleanup(func() { _ = os.Chmod(restricted, 0700) })
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		restricted = filepath.Join(request.dir, "restricted")
		if err := os.Mkdir(restricted, 0700); err != nil {
			return aicli.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(restricted, "file"), []byte("fixture"), 0600); err != nil {
			return aicli.Result{}, err
		}
		if err := os.Chmod(restricted, 0000); err != nil {
			return aicli.Result{}, err
		}
		return aicli.Result{Answer: "Done."}, nil
	}}
	result, err := runner.Run(t.Context(), testJob(command.Comment))
	if err != nil || result.Answer != "Done." {
		t.Fatalf("restricted workspace result=%+v error=%v", result, err)
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "cleanup_failed" {
			t.Fatalf("restricted directory prevented cleanup: %+v", result.Diagnostics)
		}
	}
	assertEmptyDir(t, root)
}

func TestRunnerCleansWorkspaceAfterModelError(t *testing.T) {
	root := t.TempDir()
	var source string
	modelErr := errors.New("model failed")
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		source = request.dir
		return aicli.Result{}, modelErr
	}}
	result, err := runner.Run(t.Context(), testJob(command.Comment))
	if !errors.Is(err, modelErr) || source == "" {
		t.Fatalf("model result: source=%q err=%v", source, err)
	}
	if _, statErr := os.Stat(source); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace remains after model error: %v", statErr)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("model error invented cleanup diagnostics: %+v", result.Diagnostics)
	}
	assertEmptyDir(t, root)
}

func TestTerminalUnlessCanceledPrioritizesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := terminalUnlessCanceled(ctx, syscall.ENOSPC)
	if !errors.Is(err, context.Canceled) || errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("cancellation did not take precedence: %v", err)
	}
}

func TestRunnerReportsOwnedAndCriticCleanupFailures(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires effective POSIX directory permissions and a non-root user")
	}
	for _, critic := range []bool{false, true} {
		t.Run(fmt.Sprintf("critic=%t", critic), func(t *testing.T) {
			job := testJob(command.Comment)
			if critic {
				job.Action = command.Fix
				job.Command = "fix"
				job = withClaudeCritic(job)
			}
			root := t.TempDir()
			var journal bytes.Buffer
			defer func() {
				if err := os.Chmod(root, 0700); err != nil {
					t.Error(err)
				}
			}()
			runner := Runner{WorkRoot: root, Logger: log.New(&journal, "", 0), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
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
			result, err := runner.Run(t.Context(), job)
			if err != nil || result.Answer != "Done." {
				t.Fatalf("cleanup changed accepted result: answer=%q error=%v", result.Answer, err)
			}
			cleanupFailures := 0
			recoveryCount := 0
			var recovery string
			for _, diagnostic := range result.Diagnostics {
				switch diagnostic.Code {
				case "cleanup_failed":
					cleanupFailures++
					if !strings.Contains(diagnostic.Message, "Execution workspace") {
						t.Fatalf("cleanup reported before final retry: %+v", diagnostic)
					}
				case "cleanup_recovery":
					recoveryCount++
					recovery = diagnostic.Message
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) < 2 {
				t.Fatalf("failed cleanup did not retain source and context: entries=%v error=%v", entries, err)
			}
			for _, entry := range entries {
				if !strings.Contains(recovery, entry.Name()) || strings.Contains(journal.String(), entry.Name()) {
					t.Fatalf("retained workspace lacks a private recovery reference: %q diagnostics=%+v journal=%q", entry.Name(), result.Diagnostics, journal.String())
				}
			}
			if cleanupFailures != 1 || recoveryCount != 1 || !strings.Contains(recovery, "under the configured work root") || len(recovery) > maxDiagnosticMessageBytes || validDiagnostics(result.Diagnostics) != nil || strings.Contains(journal.String(), "critic snapshot cleanup retry scheduled") != critic {
				t.Fatalf("cleanup failure not recorded: %+v", result.Diagnostics)
			}
		})
	}
}

func TestRunnerPrivateIndexPreparationFailuresKeepTheirClass(t *testing.T) {
	for _, test := range []struct {
		name, diagnostic string
		cause            error
		terminal         bool
	}{
		{"native refusal", "fatal: deterministic private-index refusal", nil, true},
		{"missing git after materialization", "", exec.ErrNotFound, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			realGit, err := aicli.AbsoluteExecutable("git")
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "private-index-stage")
			inject := `case "$PWD" in */.minion-index-*)
printf '%s\n' "$PWD" "$*" > ` + shellQuote(marker) + `
printf '%s\n' ` + shellQuote(test.diagnostic) + ` >&2
exit 77;;
esac
`
			if errors.Is(test.cause, exec.ErrNotFound) {
				// Materialization keeps its absolute executable; only the next
				// constructor's PATH lookup fails after the final refresh.
				inject = `case " $* " in *" update-index -q --refresh "*)
` + shellQuote(realGit) + ` "$@"
printf '%s\n' "$PWD" > ` + shellQuote(marker) + `
/bin/rm -- "$0"
exit 0;;
esac
`
			}
			bin := fakeOnPath(t, "git", "#!/bin/sh\nset -eu\n"+inject+"exec "+shellQuote(realGit)+` "$@"`+"\n")
			if errors.Is(test.cause, exec.ErrNotFound) {
				t.Setenv("PATH", bin)
			}
			root := t.TempDir()
			result, err := (&Runner{WorkRoot: root, runModel: forbidModel(t)}).Run(t.Context(), testJob(command.Fix))
			if err == nil || errors.Is(err, monitor.ErrTerminalJob) != test.terminal || !strings.Contains(err.Error(), "prepare private index") || len(result.Calls) != 0 || result.Answer != "" {
				t.Fatalf("private index classification: %+v,%v; want terminal=%t", result, err, test.terminal)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("private index cause lost: %v; want %v", err, test.cause)
			}
			stage, markerErr := os.ReadFile(marker)
			if markerErr != nil || len(stage) == 0 {
				t.Fatalf("private index injection did not run: %q,%v", stage, markerErr)
			}
			if errors.Is(test.cause, exec.ErrNotFound) {
				if !strings.HasPrefix(filepath.Base(strings.TrimSpace(string(stage))), "minion-") || !strings.Contains(err.Error(), "offline fix index requires git") {
					t.Fatalf("missing git occurred before private-index lookup: %q,%v", stage, err)
				}
			} else {
				var exit *exec.ExitError
				if !strings.Contains(string(stage), "/.minion-index-") || !strings.Contains(string(stage), " init --quiet --template= --object-format=sha1") || !strings.Contains(err.Error(), test.diagnostic) || !errors.As(err, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("private-index native refusal lost evidence: %q,%v", stage, err)
				}
			}
			assertEmptyDir(t, root)
		})
	}
}
