package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
			job := portableJob(command.Review)
			job.Main.Provider = provider
			job.Source.History, job.Source.HistoryComplete = []byte("bounded supplied history"), false
			job.Source.HistoryRequested = true
			job.Source.Omitted = []string{"submodule"}
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "called")
			var bins []string
			for _, name := range []string{"git", "gh"} {
				bins = append(bins, fakeOnPath(t, name, "#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 77\n"))
			}
			t.Setenv("PATH", strings.Join(bins, string(os.PathListSeparator)))
			runner := Runner{
				WorkRoot: root,
				Codex:    config.CodexConfig{APIKey: "local credential"},
				Claude:   config.ClaudeConfig{APIKey: "other local credential"},
			}
			calls := 0
			runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.provider != provider || request.writable || request.outputDir != "" || request.role != "main" || request.round != 1 {
					t.Fatalf("wrong model request: %+v", request)
				}
				if _, err := os.Lstat(filepath.Join(request.dir, ".git")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("worker received Git metadata: %v", err)
				}
				if len(request.readOnlyDirs) != 1 {
					t.Fatalf("missing read-only context grant: %v", request.readOnlyDirs)
				}
				contextDir := request.readOnlyDirs[0]
				for file, want := range map[string]string{"comparison.diff": "exact supplied comparison\n", "history.txt": "bounded supplied history", "base/deleted.txt": "deleted base context\n"} {
					data, err := os.ReadFile(filepath.Join(contextDir, file))
					if err != nil || string(data) != want {
						t.Fatalf("context %s = %q, %v", file, data, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(request.dir, "source.txt"))
				if err != nil || string(data) != "source\n" {
					t.Fatalf("source = %q, %v", data, err)
				}
				for _, want := range []string{job.Source.Head, job.Source.Base, job.Source.MergeBase, contextDir, "complete=false", quotedContext(job.Title), quotedContext(job.Notes), "Report missing context or dependencies rather than fetching them"} {
					if !strings.Contains(request.prompt, want) {
						t.Fatalf("prompt lacks %q: %s", want, request.prompt)
					}
				}
				return aicli.Result{Answer: "No findings", Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
			}
			result, err := runner.Run(context.Background(), job)
			if err != nil || calls != 1 || result.Answer != "No findings" || len(result.Calls) != 1 || !result.Calls[0].Complete {
				t.Fatalf("Run = %+v, %v; calls %d", result, err, calls)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("execution workspace retained: %v, %v", entries, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("executor invoked git or gh: %v", err)
			}
		})
	}
}

func TestRunnerReturnsFileChangesAndPartialUsage(t *testing.T) {
	job := portableJob(command.Fix)
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
	want := []workspace.Change{{Path: "binary", Mode: workspace.Executable, Data: []byte{0, 1, 2}}, {Path: "source.txt", Delete: true}}
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

func TestRunnerDeclaresIncompleteComparisonAndMetadataHistory(t *testing.T) {
	job := portableJob(command.Review)
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
			for _, want := range []string{"report incomplete comparison coverage", "commit metadata only, without historical patches", "Comparison diff incomplete=true", "History requested=false"} {
				if !strings.Contains(request.prompt, want) {
					t.Fatalf("prompt lacks %q", want)
				}
			}
			return aicli.Result{Answer: "Incomplete comparison disclosed", UsageComplete: true}, nil
		},
	}
	if _, err := runner.Run(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerBoundsCriticDisabledAnswer(t *testing.T) {
	job := portableJob(command.Comment)
	runner := Runner{
		WorkRoot: t.TempDir(),
		runModel: func(context.Context, modelRun) (aicli.Result, error) {
			return aicli.Result{Answer: strings.Repeat("€", maxOutputBytes), UsageComplete: true}, nil
		},
	}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || len(result.Calls) != 1 {
		t.Fatalf("oversized answer was not refused: %+v, %v", result, err)
	}
}

func TestRunnerKeepsCriticReadOnlyAndNotesPrivate(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = append(job.Source.Files, workspace.File{Path: "deleted.txt", Mode: workspace.Regular, Data: []byte("original deleted file")})
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
		baseline := filepath.Join(request.readOnlyDirs[0], "base")
		if !strings.Contains(request.prompt, baseline) || !strings.Contains(request.prompt, "original input baseline") || !strings.Contains(request.prompt, "executable modes") {
			t.Fatal("critic lacks original baseline instructions")
		}
		if !strings.Contains(request.prompt, "Do not create, change or delete omitted paths or anything beneath them, or replace their ancestor directories with files") {
			t.Fatal("omitted-path constraint missing")
		}
		for name, want := range map[string]string{"source.txt": "source\n", "deleted.txt": "original deleted file"} {
			data, err := os.ReadFile(filepath.Join(baseline, name))
			if err != nil || string(data) != want {
				t.Fatalf("baseline %s = %q, %v", name, data, err)
			}
		}
		if info, err := os.Stat(filepath.Join(baseline, "source.txt")); err != nil || info.Mode().Perm()&0111 != 0 {
			t.Fatalf("baseline executable mode changed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(baseline, "added.bin")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("added file entered baseline: %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(request.dir, "source.txt")); err != nil || string(data) != "changed source" {
			t.Fatalf("critic cannot read modified source: %q, %v", data, err)
		}
		if info, err := os.Stat(filepath.Join(request.dir, "source.txt")); err != nil || info.Mode().Perm()&0111 == 0 {
			t.Fatalf("critic cannot inspect changed executable mode: %v", err)
		}
		if _, err := os.Stat(filepath.Join(request.dir, "deleted.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("critic cannot inspect deleted file: %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(request.dir, "added.bin")); err != nil || !reflect.DeepEqual(data, []byte{0, 1, 2}) {
			t.Fatalf("critic cannot inspect added binary: %v, %v", data, err)
		}
		return aicli.Result{Answer: `{"approved":true,"feedback":""}`, UsageComplete: true}, nil
	}
	result, err := runner.Run(context.Background(), job)
	if err != nil || calls != 2 || result.Answer != "public answer" || len(result.Calls) != 2 || result.Calls[1].Role != "critic" || len(result.Changes) != 3 {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestRunnerFixUsesOnlyOfflineGit(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "network-command")
	script := fmt.Sprintf("#!/bin/sh\nfor arg do\n case \"$arg\" in fetch|clone|push|remote|ls-remote) printf forbidden > %s; exit 77;; esac\ndone\nexec %s \"$@\"\n", shellQuote(marker), shellQuote(realGit))
	fakeOnPath(t, "git", script)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if err := os.WriteFile(filepath.Join(request.dir, "source.txt"), []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		return aicli.Result{Answer: "Done."}, nil
	}}
	result, err := runner.Run(context.Background(), portableJob(command.Fix))
	if err != nil || len(result.Changes) != 1 {
		t.Fatalf("offline fix failed: %+v, %v", result, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runner attempted remote Git: %v", err)
	}
}

func TestRunnerRejectsMissingSkillsAndReadOnlyWrites(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := Runner{WorkRoot: t.TempDir()}
	job := portableJob(command.Comment)
	job.Main.Skills = []string{"https://github.com/owner/repo/tree/pinned/skills/missing"}
	runner.runModel = func(_ context.Context, _ modelRun) (aicli.Result, error) {
		t.Fatal("model ran with missing required skill")
		return aicli.Result{}, nil
	}
	if _, err := runner.Run(context.Background(), job); err == nil || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing skill = %v", err)
	}
	job.Main.Skills = []string{skills.BuiltinAntiPromptInjection}
	runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
		if err := os.WriteFile(filepath.Join(request.dir, "source.txt"), []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		return aicli.Result{Answer: "answer"}, nil
	}
	if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only write = %v", err)
	}
	job.Version++
	if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("invalid job was not terminal: %v", err)
	}
}

func TestRunnerIgnoresWorkerGitMetadata(t *testing.T) {
	for _, name := range []string{".git", ".gIt", "nested/.GiT"} {
		t.Run(name, func(t *testing.T) {
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				writeIndexFixture(t, request.dir, name+"/config", []byte("untrusted worker metadata"))
				writeIndexFixture(t, request.dir, "source.txt", []byte("changed"))
				return aicli.Result{Answer: "Changed source", UsageComplete: true}, nil
			}}
			result, err := runner.Run(context.Background(), portableJob(command.Fix))
			if err != nil || len(result.Changes) != 1 || result.Changes[0].Path != "source.txt" {
				t.Fatalf("metadata result = %+v, %v", result, err)
			}
		})
	}
}

func TestRunnerCanonicalizesTrustedWorkRoot(t *testing.T) {
	root := t.TempDir()
	actual, link := filepath.Join(root, "actual"), filepath.Join(root, "linked")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	runner := Runner{WorkRoot: link, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if filepath.Dir(request.dir) != actual {
			t.Fatalf("worker source path is not canonical: %s", request.dir)
		}
		return aicli.Result{Answer: "Read source", UsageComplete: true}, nil
	}}
	if _, err := runner.Run(context.Background(), portableJob(command.Comment)); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerClearsHostSkillsAndRejectsMissingCLI(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Main = RoleSettings{Provider: provider}
			runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{Skills: []string{"/stale/codex/SKILL.md"}}, Claude: config.ClaudeConfig{Skills: []string{"/stale/claude/SKILL.md"}}}
			if err := runner.configureRole(job.Main); err != nil {
				t.Fatal(err)
			}
			if provider == "codex" && len(runner.Codex.Skills) != 0 || provider == "claude" && len(runner.Claude.Skills) != 0 {
				t.Fatal("Job did not clear host skills")
			}
			job.Main.Skills = []string{skills.BuiltinAntiPromptInjection}
			t.Setenv("PATH", "")
			t.Setenv("HOME", t.TempDir())
			if result, err := runner.Run(context.Background(), job); err == nil || len(result.Calls) != 1 {
				t.Fatalf("missing CLI result = %+v, %v", result, err)
			}
		})
	}
}

func TestPortableAuditMetadataUsesSourceIdentity(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	root := t.TempDir()
	for _, name := range auditArtifacts {
		data := []byte("content")
		if name == "run-metadata.json" {
			owners := strings.TrimSuffix(auditTestOwners, "}") + `,"/worker/local/extra-owner":"/worker/local/child"}`
			data = []byte(fmt.Sprintf(`{"run_id":"test-run","repo":"owner/repo","target":"/worker/local/source","source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":["/installed/security-audit/WEB-PROTOCOL-AND-AUTH.md"],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete","output_dir":"/worker/local/reports","extra":{"run_dir":"/worker/local/runtime"}}`, job.Source.Head, owners))
			if _, err := validateAuditMetadata(data, []byte("content"), job.Identity.Repository, "/worker/local/source", job.Source.Head); err != nil {
				t.Fatalf("invalid raw metadata control: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, _, err := readAuditArtifacts(root, auditForReview)
	if err != nil {
		t.Fatal(err)
	}
	raw := slices.Clone(files)
	files, err = normalizeAuditArtifacts(files, job, "/installed/security-audit/SKILL.md")
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
		if file.Path == "run-metadata.json" && (strings.Contains(string(file.Data), "/installed/") || !strings.Contains(string(file.Data), "WEB-PROTOCOL-AND-AUTH.md")) {
			t.Fatalf("companion path is not portable: %s", file.Data)
		}
		if file.Path == "run-metadata.json" {
			var metadata struct {
				RunID       string            `json:"run_id"`
				SkillSource string            `json:"skill_source"`
				Owners      map[string]string `json:"shared_file_owners"`
			}
			if err := json.Unmarshal(file.Data, &metadata); err != nil || metadata.RunID != "test-run" || metadata.SkillSource != testAuditSkillURL || len(metadata.Owners) != len(auditArtifacts) || strings.Contains(string(file.Data), `"output_dir"`) || strings.Contains(string(file.Data), `"extra"`) || !strings.Contains(string(raw[i].Data), `"output_dir"`) {
				t.Fatalf("metadata projection changed supported evidence or retained extensions: %+v, %v", metadata, err)
			}
		}
	}
	files[0].Data = []byte(`{"repo":"owner/repo","target":"other","source_ref":"` + job.Source.Head + `"}`)
	if validateResultArtifacts(job, files, true) == nil {
		t.Fatal("accepted mismatched audit metadata")
	}
}

func TestAuditPromptSelectsDomainCompanionsOnly(t *testing.T) {
	for _, critic := range []bool{false, true} {
		job := portableJob(command.SecurityAudit)
		if critic {
			job.Critic = &RoleSettings{Provider: "claude"}
			job.CriticMaxRounds = config.DefaultCriticMaxRounds
		}
		prompt, err := actionPrompt(context.Background(), job, "/source", "/context", "/reports", "/installed/security-audit/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Do not execute repository code, builds, tests, or fixtures", "Copy this source-only execution prohibition verbatim into every delegated agent prompt", "it overrides the skill's local-execution allowance", "selected security-audit domain companion files", `"When to use" conditions apply`, "Exclude its main SKILL.md and independently configured skills", "Use [] when no domain companion applies", "installed validation and publication"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("audit prompt lacks %q: %s", want, prompt)
			}
		}
		if strings.Contains(prompt, "subsequent critic reviews") != critic {
			t.Fatalf("critic time requested without matching critic setting: %t", critic)
		}
	}
}

func TestRunnerReturnsPartialAuditArtifactsOnFailure(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	failure := errors.New("provider stopped after writing partial report")
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if err := os.WriteFile(filepath.Join(request.outputDir, "REPORT.md"), []byte("Incomplete source findings"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(request.outputDir, "run-metadata.json"), []byte(`{"target":"/worker/local/source","selected_companion_files":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(request.outputDir, "findings.json"), make([]byte, (5<<20)+1), 0600); err != nil {
			t.Fatal(err)
		}
		return aicli.Result{Tokens: usage.Tokens{Input: 9}}, failure
	}}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, failure) || len(result.Calls) != 1 || len(result.Artifacts) != 2 || result.Answer != "" {
		t.Fatalf("partial audit = %+v, %v", result, err)
	}
	var incomplete, unavailable bool
	for _, diagnostic := range result.Diagnostics {
		incomplete = incomplete || diagnostic.Code == "audit_incomplete"
		unavailable = unavailable || diagnostic.Code == "artifact_unavailable"
	}
	if !incomplete || !unavailable {
		t.Fatalf("partial audit diagnostics missing: %+v", result.Diagnostics)
	}
	for _, file := range result.Artifacts {
		if file.Path == "run-metadata.json" && (strings.Contains(string(file.Data), "/worker/") || !strings.Contains(string(file.Data), sourceIdentity(job))) {
			t.Fatalf("partial metadata is not portable: %s", file.Data)
		}
	}
}

func TestCommentPromptUsesPortablePaths(t *testing.T) {
	job := portableJob(command.Comment)
	prompt, err := actionPrompt(context.Background(), job, "/host/private/source", "/context", "", "")
	if err != nil || strings.Contains(prompt, "/host/private/source") || !strings.Contains(prompt, "Use repository-relative paths and source identity") {
		t.Fatalf("comment prompt contains host path or lacks path guidance: %s, %v", prompt, err)
	}
}
