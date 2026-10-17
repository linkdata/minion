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
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestOversizedNormalizedMetadataPreservesOtherPartialArtifacts(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	metadata := []byte(fmt.Sprintf(`{"run_id":"expansion-test","repo":%q,"target":"/worker/source","source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete","payload":"%s"}`, job.Identity.Repository, job.Source.Head, auditTestOwners, strings.Repeat("<", maxAuditArtifactBytes/5)))
	if _, err := validateAuditMetadata(metadata, []byte("report content"), job.Identity.Repository, "/worker/source", job.Source.Head); err != nil {
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
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		metadata := fmt.Sprintf(`{"run_id":"cancel-test","repo":%q,"target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, job.Identity.Repository, request.dir, job.Source.Head, auditTestOwners)
		for _, name := range auditArtifacts {
			data := []byte("report content")
			switch name {
			case "run-metadata.json":
				data = []byte(metadata)
			case "findings.json":
				data = []byte("[]")
			case "coverage-ledger.json":
				data = []byte(`[{"status":"covered"}]`)
			}
			if err := os.WriteFile(filepath.Join(request.outputDir, name), data, 0600); err != nil {
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
	for _, provider := range []string{"codex", "claude"} {
		for _, role := range []string{"main", "critic"} {
			t.Run(provider+"/"+role, func(t *testing.T) {
				job := portableJob(command.Comment)
				job.Main.Provider = provider
				if role == "critic" {
					job.Critic = &RoleSettings{Provider: provider}
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
}

func TestRunnerNULProposalFailsBeforeCritic(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	mainCalls, criticCalls := 0, 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == "main" {
			mainCalls++
			return aicli.Result{Answer: proposalJSON("answer\x00hidden", "")}, nil
		}
		criticCalls++
		return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "answer must not contain NUL") || mainCalls != 1 || criticCalls != 0 || len(result.Calls) != 1 || result.Answer != "" {
		t.Fatalf("NUL proposal reached critic: main=%d critic=%d recorded=%d answer=%q error=%v", mainCalls, criticCalls, len(result.Calls), result.Answer, err)
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

func TestRunnerMetadataAliasSourceIsTerminalBeforeModels(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "GIT~1/x", Mode: workspace.Regular}}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		t.Fatal("model ran for unsupported fix source")
		return aicli.Result{}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 0 {
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
				job.Critic = &RoleSettings{Provider: "claude"}
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
					return aicli.Result{Answer: `{"approved":true,"feedback":""}`}, nil
				}
				return aicli.Result{Answer: "Done."}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if err != nil {
				t.Fatal(err)
			}
			workspaceFailed, criticFailed := false, false
			for _, diagnostic := range result.Diagnostics {
				if diagnostic.Code == "cleanup_failed" {
					workspaceFailed = workspaceFailed || strings.Contains(diagnostic.Message, "Execution workspace")
					criticFailed = criticFailed || strings.Contains(diagnostic.Message, "Critic snapshot")
				}
			}
			if !workspaceFailed || critic && !criticFailed {
				t.Fatalf("cleanup failure not recorded: %+v", result.Diagnostics)
			}
		})
	}
}
