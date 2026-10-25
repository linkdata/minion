package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRunnerUsesSuppliedSourceAndContext(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			forbidNetworkGit(t)
			job := testJob(command.Review)
			job.Main.Provider = provider
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "called")
			fakeOnPath(t, "gh", "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 77\n")
			runner := Runner{
				WorkRoot: root,
			}
			calls := 0
			runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				checkRefutations(t, request, nil)
				if request.provider != provider || request.writable || request.outputDir != "" || request.role != "main" || request.round != 1 {
					t.Fatalf("wrong model request: %+v", request)
				}
				assertModelRepository(t, request.dir, job.Source.Head)
				if got := strings.TrimSpace(modelGit(t, request.dir, "rev-parse", "refs/tags/fixture-tag")); got != job.Source.Head {
					t.Fatalf("fixture tag points to %q, want %q", got, job.Source.Head)
				}
				if len(request.readOnlyDirs) != 1 {
					t.Fatalf("missing read-only context grant: %v", request.readOnlyDirs)
				}
				contextDir := request.readOnlyDirs[0]
				for file, want := range map[string]string{"comparison.diff": "exact supplied comparison\n", "base/deleted.txt": "deleted base context\n"} {
					data, err := os.ReadFile(filepath.Join(contextDir, file))
					if err != nil || string(data) != want {
						t.Fatalf("context %s = %q, %v", file, data, err)
					}
				}

				metadataBytes, err := os.ReadFile(filepath.Join(contextDir, "source.json"))
				if err != nil {
					t.Fatal(err)
				}
				var metadata struct {
					Head      string `json:"head"`
					Base      string `json:"base"`
					MergeBase string `json:"merge_base"`
				}
				if err := json.Unmarshal(metadataBytes, &metadata); err != nil || metadata.Head != job.Source.Head || metadata.Base != job.Source.Base || metadata.MergeBase != job.Source.MergeBase {
					t.Fatalf("source identity missing: %s,%v", metadataBytes, err)
				}
				data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
				if err != nil || string(data) != "source\n" {
					t.Fatalf("source = %q, %v", data, err)
				}
				for _, want := range []string{job.Source.Head, fmt.Sprintf("Compare the supplied %q.", filepath.Join(contextDir, "comparison.diff")), fmt.Sprintf("under %q where needed", filepath.Join(contextDir, "base")), "complete history, including historical file contents, branches and tags", "no remotes or GitHub credentials", "Use local Git commands to investigate history", quotedContext(job.Title), quotedContext(job.Notes), "Report missing context or dependencies rather than fetching them"} {
					if !strings.Contains(request.prompt, want) {
						t.Fatalf("prompt lacks %q: %s", want, request.prompt)
					}
				}
				return aicli.Result{Answer: "No findings", Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
			}
			result, err := runner.Run(context.Background(), job)
			if err != nil || len(result.Refutations) != 0 || calls != 1 || result.Answer != "No findings" || len(result.Calls) != 1 || !result.Calls[0].Complete {
				t.Fatalf("Run = %+v, %v; calls %d", result, err, calls)
			}
			assertEmptyDir(t, root)
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("executor invoked gh: %v", err)
			}
		})
	}
}

func TestReviewContextPreservesDistinctRevisionIdentities(t *testing.T) {
	job := testJob(command.Review)
	job.Source.Head = strings.Repeat("a", 40)
	job.Source.Base = strings.Repeat("b", 40)
	job.Source.MergeBase = strings.Repeat("c", 40)
	job.Source.Omitted = []string{"base-link", "head-link"}
	job.Source.HeadOmitted = []string{"head-link"}
	root := t.TempDir()
	if err := materializeContext(root, job.Source); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Head        string
		Base        string
		MergeBase   string   `json:"merge_base"`
		Omitted     []string `json:"omitted"`
		HeadOmitted []string `json:"head_omitted"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.Head != job.Source.Head || metadata.Base != job.Source.Base || metadata.MergeBase != job.Source.MergeBase || !slices.Equal(metadata.Omitted, job.Source.Omitted) || !slices.Equal(metadata.HeadOmitted, job.Source.HeadOmitted) {
		t.Fatalf("revision identity projection: %s, %v", data, err)
	}
	prompt, err := actionPrompt(context.Background(), job, "/source", root, "", "")
	want := fmt.Sprintf("head %s (merge base %s, base tip %s)", job.Source.Head, job.Source.MergeBase, job.Source.Base)
	if err != nil || !strings.Contains(prompt, want) {
		t.Fatalf("revision identity prompt: %q, %v; want %q", prompt, err, want)
	}
}

func TestAuditPromptDescribesPublicSummary(t *testing.T) {
	job := testJob(command.SecurityAudit)
	prompt, err := actionPrompt(t.Context(), job, "/source", "/context", "/reports", "/installed/security-audit/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"summary rendered by the parent from validated findings.json and coverage-ledger.json", "retained privately as executor narrative", "Every reported finding must have its complete structured record and linked candidate/verifier coverage evidence"} {
		if !strings.Contains(prompt, field) {
			t.Fatalf("audit prompt omits public-summary requirement %q", field)
		}
	}
}

func TestRunnerReturnsFileChangesAndPartialUsage(t *testing.T) {
	job := testJob(command.Fix)
	runner := Runner{WorkRoot: t.TempDir()}
	runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
		if !request.writable {
			t.Fatal("fix has read-only source")
		}
		if err := os.Remove(filepath.Join(request.dir, "source.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(request.dir, "binary"), []byte{0, 1, 2}, 0700); err != nil {
			t.Fatal(err)
		}
		return aicli.Result{Answer: "Changed two files", UsageComplete: true}, nil
	}
	result, err := runner.Run(context.Background(), job)
	mode := workspace.Executable
	if runtime.GOOS == "windows" {
		mode = workspace.Regular
	}
	want := []workspace.Change{{Path: "binary", Mode: mode, Data: []byte{0, 1, 2}}, {Path: "source.txt", Delete: true}}
	if err != nil || !reflect.DeepEqual(result.Changes, want) {
		t.Fatalf("changes = %+v, %v", result.Changes, err)
	}
	failure := errors.New("provider stopped")
	runner.runModel = func(_ context.Context, _ modelRun) (aicli.Result, error) {
		return aicli.Result{Tokens: usage.Tokens{Input: 12, Output: 3}}, failure
	}
	result, err = runner.Run(context.Background(), job)
	if !errors.Is(err, failure) || errors.Is(err, monitor.ErrTerminalJob) || len(result.Calls) != 1 || result.Calls[0].Tokens.Input != 12 || result.Calls[0].Complete || result.Answer != "" {
		t.Fatalf("failure lost partial usage: %+v, %v", result, err)
	}
}

func TestRunnerRedactsBothConfiguredKeysInReturnedError(t *testing.T) {
	const codexKey, claudeKey = "fixture-codex-key", "fixture-claude-key"
	cause := errors.New("provider failed with " + codexKey + " and " + claudeKey)
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: codexKey}, Claude: config.ClaudeConfig{APIKey: claudeKey}}
	runner.runModel = func(context.Context, modelRun) (aicli.Result, error) {
		return aicli.Result{}, fmt.Errorf("model call: %w", cause)
	}
	result, err := runner.Run(context.Background(), testJob(command.Comment))
	if !errors.Is(err, cause) || err.Error() != "model call: provider failed with [redacted] and [redacted]" || len(result.Calls) != 1 {
		t.Fatalf("returned error lost redaction or cause: %+v, %v", result, err)
	}
}

func TestRunnerUsesLocalHistoryWhenComparisonDiffIsIncomplete(t *testing.T) {
	job := testJob(command.Review)
	job.Source.Diff, job.Source.DiffIncomplete = nil, true
	job.Source.ChangedPaths = []string{"source.txt", "deleted.txt"}
	runner := Runner{
		WorkRoot: t.TempDir(),
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			data, err := os.ReadFile(filepath.Join(request.readOnlyDirs[0], "source.json"))
			if err != nil {
				t.Fatal(err)
			}
			var metadata struct {
				DiffIncomplete bool     `json:"diff_incomplete"`
				ChangedPaths   []string `json:"changed_paths"`
			}
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			if !metadata.DiffIncomplete || !reflect.DeepEqual(metadata.ChangedPaths, job.Source.ChangedPaths) {
				t.Fatalf("comparison coverage metadata missing: %s", data)
			}
			if !strings.Contains(request.prompt, "The text diff exceeds its limit.") || !strings.Contains(request.prompt, "Comparison diff incomplete=true") || strings.Contains(request.prompt, filepath.Join(request.readOnlyDirs[0], "comparison.diff")) {
				t.Fatalf("incomplete comparison guidance wrong: %s", request.prompt)
			}
			return aicli.Result{Answer: "Compared using local Git", UsageComplete: true}, nil
		},
	}
	if _, err := runner.Run(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerKeepsCriticReadOnlyAndPrivateReasoning(t *testing.T) {
	forbidNetworkGit(t)
	job := testJob(command.Fix)
	job.Source.Files = append(job.Source.Files, workspace.File{Path: "deleted.txt", Mode: workspace.Regular, Data: []byte("original deleted file")})
	attachRepository(t, &job)
	job.Critic = &RoleSettings{Provider: "claude", AlwaysOnSkills: []string{skills.BuiltinAntiPromptInjection}}
	job.CriticMaxRounds = config.DefaultCriticMaxRounds
	runner := Runner{WorkRoot: t.TempDir()}
	calls := 0
	var mainDir string
	runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls++
		if request.role == "main" {
			mainDir = request.dir
			if request.outputSchema != mainOutputSchema || !request.writable {
				t.Fatal("main lost proposal schema or write access")
			}
			if err := os.WriteFile(filepath.Join(request.dir, "source.txt"), []byte("changed source"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(request.dir, "source.txt"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(request.dir, "deleted.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(request.dir, "added.bin"), []byte{0, 1, 2}, 0600); err != nil {
				t.Fatal(err)
			}
			return aicli.Result{Answer: proposalJSON("public answer", "UNPUBLISHED_AUTHOR_REASON"), UsageComplete: true}, nil
		}
		if request.role != "critic" || request.reviewPhase != "full" || request.dir == mainDir || request.writable || request.outputSchema != criticOutputSchema || strings.Contains(request.prompt, "UNPUBLISHED_AUTHOR_REASON") || len(request.readOnlyDirs) != 1 {
			t.Fatalf("wrong critic boundary: %+v", request)
		}
		assertModelRepository(t, request.dir, job.Source.Head)
		for _, want := range []string{
			"Use git status and git diff against HEAD to compare the current source against the original input baseline, including added and deleted files, binary content, and executable modes.",
			"For fix actions, compare current source with the original input at Git HEAD using git status and git diff, including additions, deletions, binary content and executable modes.",
		} {
			if !strings.Contains(request.prompt, want) {
				t.Fatalf("critic lacks local baseline instruction %q", want)
			}
		}
		if !strings.Contains(request.prompt, "Do not create, change or delete omitted paths or anything beneath them, or replace their ancestor directories with files") {
			t.Fatal("omitted-path constraint missing")
		}
		for name, want := range map[string]string{"source.txt": "source\n", "deleted.txt": "original deleted file"} {
			if got := modelGit(t, request.dir, "show", "HEAD:"+name); got != want {
				t.Fatalf("Git baseline %s = %q, want %q", name, got, want)
			}
		}
		if entry := modelGit(t, request.dir, "ls-tree", "HEAD", "--", "source.txt"); !strings.HasPrefix(entry, "100644 ") {
			t.Fatalf("baseline executable mode changed: %q", entry)
		}
		if entry := modelGit(t, request.dir, "ls-tree", "HEAD", "--", "added.bin"); entry != "" {
			t.Fatalf("added file entered Git baseline: %q", entry)
		}
		if status := modelGit(t, request.dir, "status", "--porcelain", "--untracked-files=all"); status != " D deleted.txt\n M source.txt\n?? added.bin\n" {
			t.Fatalf("critic Git status lost candidate changes: %q", status)
		}
		return aicli.Result{Answer: approvedCritiqueJSON, UsageComplete: true}, nil
	}
	result, err := runner.Run(context.Background(), job)
	if err != nil || calls != 2 || result.Answer != "public answer" || len(result.Refutations) != 0 || len(result.Calls) != 2 || result.Calls[1].Role != "critic" || len(result.Changes) != 3 {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestRunnerIgnoresWorkerGitMetadata(t *testing.T) {
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		writeFixtureFile(t, request.dir, "nested/.GiT/config", []byte("untrusted worker metadata"))
		writeFixtureFile(t, request.dir, "source.txt", []byte("changed"))
		return aicli.Result{Answer: "Changed source", UsageComplete: true}, nil
	}}
	result, err := runner.Run(context.Background(), testJob(command.Fix))
	if err != nil || len(result.Changes) != 1 || result.Changes[0].Path != "source.txt" || string(result.Changes[0].Data) != "changed" {
		t.Fatalf("metadata result=%+v,%v", result, err)
	}
}

func TestRunnerCanonicalizesTrustedWorkRoot(t *testing.T) {
	root := t.TempDir()
	actual, link := filepath.Join(root, "actual"), filepath.Join(root, "linked")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, actual, link)
	actual, err := filepath.EvalSymlinks(actual)
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{WorkRoot: link, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if filepath.Dir(request.dir) != actual {
			t.Fatalf("worker source path is not canonical: %s", request.dir)
		}
		return aicli.Result{Answer: "Read source", UsageComplete: true}, nil
	}}
	if _, err := runner.Run(context.Background(), testJob(command.Comment)); err != nil {
		t.Fatal(err)
	}
}

func TestPortableAuditMetadataUsesSourceIdentity(t *testing.T) {
	job := testJob(command.SecurityAudit)
	root := t.TempDir()
	skill := filepath.Join(t.TempDir(), "installed", "security-audit", "SKILL.md")
	companion := filepath.Join(filepath.Dir(skill), "WEB-PROTOCOL-AND-AUTH.md")
	for _, name := range auditArtifacts {
		data := []byte("content")
		switch name {
		case "coverage-ledger.json":
			data = []byte(auditCoveredLedgerFixture)
		case "findings.json":
			data = []byte("[]")
		}
		if name == "run-metadata.json" {
			owners := strings.TrimSuffix(auditTestOwners, "}") + `,"/worker/local/extra-owner":"/worker/local/child"}`
			data = []byte(fmt.Sprintf(`{"run_id":"test-run","repo":"owner/repo","target":"/worker/local/source","source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[%q],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete","skill_source":"worker-claim","output_dir":"/worker/local/reports","extra":{"run_dir":"/worker/local/runtime"}}`, job.Source.Head, companion, owners))
			if _, err := validateAuditMetadata(data, job.Identity.Repository, "/worker/local/source", job.Source.Head); err != nil {
				t.Fatalf("invalid raw metadata control: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, _, err := readAuditArtifacts(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	raw := slices.Clone(files)
	files, err = normalizeAuditArtifacts(files, job, skill)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResultArtifacts(job, files, true); err != nil {
		t.Fatal(err)
	}
	for i, file := range files {
		if file.Path != "run-metadata.json" && string(file.Data) != string(raw[i].Data) {
			t.Fatalf("normalization changed approved report %q", file.Path)
		}
		if file.Path == "run-metadata.json" && (strings.Contains(string(file.Data), "/worker/") || !strings.Contains(string(file.Data), sourceIdentity(job))) {
			t.Fatalf("metadata is not portable: %s", file.Data)
		}
		if file.Path == "run-metadata.json" {
			var metadata struct {
				RunID                  string            `json:"run_id"`
				Owners                 map[string]string `json:"shared_file_owners"`
				SelectedCompanionFiles []string          `json:"selected_companion_files"`
			}
			if err := json.Unmarshal(file.Data, &metadata); err != nil || metadata.RunID != "test-run" || len(metadata.Owners) != len(auditArtifacts) || !slices.Equal(metadata.SelectedCompanionFiles, []string{"WEB-PROTOCOL-AND-AUTH.md"}) || strings.Contains(string(file.Data), `"skill_source"`) || strings.Contains(string(file.Data), `"output_dir"`) || strings.Contains(string(file.Data), `"extra"`) || !strings.Contains(string(raw[i].Data), `"output_dir"`) {
				t.Fatalf("metadata projection changed supported evidence or retained extensions: %+v, %v", metadata, err)
			}
		}
	}
	for i := range files {
		if files[i].Path == "run-metadata.json" {
			var metadata map[string]json.RawMessage
			if err := json.Unmarshal(files[i].Data, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata["target"] = json.RawMessage(`"other"`)
			var err error
			files[i].Data, err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if validateResultArtifacts(job, files, true) == nil {
		t.Fatal("accepted mismatched audit metadata")
	}
}

func TestRunnerReturnsPartialAuditArtifactsOnFailure(t *testing.T) {
	for _, field := range []string{"", "scope_paths"} {
		name := "host_path_in_" + field
		if field == "" {
			name = "no_path_list"
		}
		t.Run(name, func(t *testing.T) {
			job := auditRunnerJob(t)
			failure := errors.New("provider stopped after writing partial report")
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if err := os.WriteFile(filepath.Join(request.outputDir, "REPORT.md"), []byte("Incomplete source findings"), 0600); err != nil {
					t.Fatal(err)
				}
				metadata := `{"target":"/worker/local/source","selected_companion_files":[]}`
				if field != "" {
					metadata = fmt.Sprintf(`{"target":"/worker/local/source","selected_companion_files":[],%q:["/worker/local/source"]}`, field)
				}
				if err := os.WriteFile(filepath.Join(request.outputDir, "run-metadata.json"), []byte(metadata), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(request.outputDir, "findings.json"), make([]byte, (5<<20)+1), 0600); err != nil {
					t.Fatal(err)
				}
				return aicli.Result{Tokens: usage.Tokens{Input: 9}}, failure
			}}
			result, err := runner.Run(context.Background(), job)
			wantArtifacts := 2
			if field != "" {
				wantArtifacts = 1
			}
			if !errors.Is(err, failure) || len(result.Calls) != 1 || len(result.Artifacts) != wantArtifacts || result.Answer != "" {
				t.Fatalf("partial audit = %+v, %v", result, err)
			}
			var incomplete, unavailable, invalidMetadata bool
			for _, diagnostic := range result.Diagnostics {
				incomplete = incomplete || diagnostic.Code == "audit_incomplete"
				unavailable = unavailable || diagnostic.Code == "artifact_unavailable"
				invalidMetadata = invalidMetadata || diagnostic.Code == "artifact_metadata_invalid"
			}
			if !incomplete || !unavailable || invalidMetadata != (field != "") {
				t.Fatalf("partial audit diagnostics missing: %+v", result.Diagnostics)
			}
			for _, file := range result.Artifacts {
				if file.Path == "run-metadata.json" && (strings.Contains(string(file.Data), "/worker/") || !strings.Contains(string(file.Data), sourceIdentity(job))) {
					t.Fatalf("partial metadata is not portable: %s", file.Data)
				}
			}
		})
	}
}

func TestRunnerRejectsGitMetadataMutation(t *testing.T) {
	mutations := []string{"HEAD contents", "config contents", "added file", "permissions", "root file", "root symlink", "hard link"}
	for _, test := range []struct {
		action    command.Action
		role      string
		mutations []string
	}{
		{command.Fix, roleMain, mutations},
		{command.Fix, roleCritic, mutations},
		{command.Comment, roleCritic, []string{"HEAD contents"}},
	} {
		role := test.role
		for _, mutation := range test.mutations {
			t.Run(string(test.action)+"/"+role+"/"+mutation, func(t *testing.T) {
				if mutation == "permissions" && runtime.GOOS == "windows" {
					t.Skip("os.Chmod cannot change Git metadata ACLs on native Windows")
				}
				job := withClaudeCritic(testJob(test.action))
				calls := 0
				var mainDir string
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					assertModelRepository(t, request.dir, job.Source.Head)
					if request.role == role {
						dir := filepath.Join(request.dir, ".git")
						switch mutation {
						case "HEAD contents", "config contents":
							name := "HEAD"
							if mutation == "config contents" {
								name = "config"
							}
							writeFixtureFile(t, dir, name, []byte("forged metadata"))
						case "added file":
							writeFixtureFile(t, dir, "injected", []byte("added metadata"))
						case "permissions":
							name := filepath.Join(dir, "HEAD")
							info, err := os.Stat(name)
							if err != nil {
								t.Fatal(err)
							}
							if err := os.Chmod(name, info.Mode().Perm()^0100); err != nil {
								t.Fatal(err)
							}
						case "root file", "root symlink":
							moved := filepath.Join(t.TempDir(), "metadata")
							if err := os.Rename(dir, moved); err != nil {
								t.Fatal(err)
							}
							if mutation == "root file" {
								if err := os.WriteFile(dir, []byte("replaced metadata"), 0600); err != nil {
									t.Fatal(err)
								}
							} else {
								symlinkOrSkip(t, moved, dir)
							}
						case "hard link":
							if err := os.Link(filepath.Join(dir, "HEAD"), filepath.Join(t.TempDir(), "linked-head")); err != nil {
								t.Fatal(err)
							}
						}
					}
					if request.role == roleMain {
						mainDir = request.dir
						if test.action == command.Fix {
							writeFixtureFile(t, request.dir, "source.txt", []byte("candidate"))
						}
						return aicli.Result{Answer: proposalJSON("candidate", "")}, nil
					}
					if test.action == command.Comment && (request.dir != mainDir || request.writable || request.outputDir != "") {
						t.Fatal("read-only critic lost the shared source boundary")
					}
					return aicli.Result{Answer: approvedCritiqueJSON}, nil
				}}
				wantCalls, wantError := 1, "supplied repository changed"
				if role == roleCritic {
					wantCalls = 2
					if test.action == command.Fix {
						wantError = "frozen candidate repository changed"
						if mutation == "root file" || mutation == "root symlink" {
							wantError = "frozen candidate source changed"
						}
					}
				}
				result, err := runner.Run(context.Background(), job)
				if !errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || len(result.Changes) != 0 || !strings.Contains(err.Error(), wantError) || calls != wantCalls || len(result.Calls) != wantCalls || result.Calls[len(result.Calls)-1].Role != role {
					t.Fatalf("metadata mutation accepted or wrong role failed: calls=%d want=%d result=%+v error=%v want=%q", calls, wantCalls, result, err, wantError)
				}
			})
		}
	}
}
