package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHostileHTTPResultCannotPublishConfiguredProviderKey(t *testing.T) {
	const codexKey = "fixture-codex-secret"
	const claudeKey = "fixture-claude-secret"
	for _, tc := range []struct {
		name, content string
		audit         bool
		answerOnly    bool
	}{
		{name: "answer with other provider key", content: "public " + claudeKey},
		{name: "encoded answer key", content: `public fixture-claude-\u0073ecret`},
		{name: "audit answer key preserves safe artifacts", content: "public " + claudeKey, audit: true, answerOnly: true},
		{name: "audit mixed safe and NUL-split artifact", content: "public fixture-codex-\x00secret", audit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.audit {
				installActionTestSkills(t, testAuditSkillURL)
			}
			if tc.audit {
				gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
			} else {
				gitFixtureRemote(t)
			}
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveIssueFixture(t, w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			workRoot, usageDir := t.TempDir(), filepath.Join(t.TempDir(), "usage")
			name, action, skill := "comment", command.Comment, skills.BuiltinAntiPromptInjection
			if tc.audit {
				name, action, skill = "security-audit", command.SecurityAudit, testAuditSkillURL
			}
			e := &Orchestrator{WorkRoot: workRoot, Usage: &usage.Store{Dir: usageDir}, GitHub: testGitHub(server, "token"), BotLogin: "bot",
				Runner:   Runner{Codex: config.CodexConfig{APIKey: codexKey}, Claude: config.ClaudeConfig{APIKey: claudeKey}},
				Commands: map[string]config.CommandMapping{name: {Action: string(action), Skills: []string{skill}}},
			}
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				if tc.audit {
					job.Source.BaseFiles = nil // The issue audit has no base; preserve that across the HTTP JSON round trip.
				}
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				before := f.call(1)
				f.ack(t, "/before", before, http.StatusOK, 1)
				call := before.Checkpoint.Call
				call.Tokens, call.Complete = usage.Tokens{Input: 3, Output: 2}, true
				f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
				outcome := f.outcome(3, nil)
				outcome.Result = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Safe result", Calls: []CallUsage{call}}
				if tc.audit {
					outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), outcome.Result.Answer)
					if tc.answerOnly {
						outcome.Result.Answer = tc.content
					} else {
						for i := range outcome.Result.Artifacts {
							if outcome.Result.Artifacts[i].Path == "architecture.md" {
								outcome.Result.Artifacts[i].Data = []byte(tc.content)
							}
						}
					}
				} else {
					outcome.Result.Answer = tc.content
				}
				if err := outcome.Result.Validate(job); tc.answerOnly {
					if err == nil || !strings.Contains(err.Error(), "answer differs from REPORT.md") {
						t.Fatalf("answer-only fixture lost its mismatch: %v", err)
					}
				} else if err != nil {
					t.Fatalf("hostile result was not otherwise publishable: %v", err)
				}
				f.ack(t, "/outcome", outcome, http.StatusOK, 3)
				got := f.result(t)
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: name, Action: action}
			request.Repository.Actions = []string{name}
			err := e.Execute(t.Context(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || !tc.answerOnly && !strings.Contains(err.Error(), "configured provider key") || writes.Load() != 0 {
				t.Fatalf("hostile result crossed publication boundary: error=%v writes=%d", err, writes.Load())
			}
			records, err := usage.ReadAll(usageDir)
			if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].Tokens != (usage.Tokens{Input: 3, Output: 2}) {
				t.Fatalf("failure usage evidence missing: records=%+v error=%v", records, err)
			}
			if tc.audit {
				if paths, err := filepath.Glob(filepath.Join(workRoot, "audits", "complete-*")); err != nil || len(paths) != 0 {
					t.Fatalf("hostile result retained as complete audit: paths=%v error=%v", paths, err)
				}
				paths, err := filepath.Glob(filepath.Join(workRoot, "audits", "incomplete-*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("private incomplete audit evidence missing: paths=%v error=%v", paths, err)
				}
				if marker, err := os.ReadFile(filepath.Join(paths[0], "INCOMPLETE.txt")); err != nil || !strings.Contains(string(marker), "must not be published") {
					t.Fatalf("private incomplete audit marker missing: %q error=%v", marker, err)
				}
				files, err := os.ReadDir(paths[0])
				wantFiles := len(auditArtifacts) + 1
				if !tc.answerOnly {
					wantFiles--
				}
				if err != nil || len(files) != wantFiles {
					t.Fatalf("safe partial audit evidence lost: files=%v error=%v want=%d", files, err, wantFiles)
				}
				for _, file := range files {
					data, err := os.ReadFile(filepath.Join(paths[0], file.Name()))
					if err != nil || containsConfiguredKey(data, codexKey) || containsConfiguredKey(data, claudeKey) {
						t.Fatalf("private audit retained a configured key in %q: %v", file.Name(), err)
					}
				}
				if _, err := os.Stat(filepath.Join(paths[0], "REPORT.md")); err != nil {
					t.Fatalf("safe report was lost: %v", err)
				}
				if _, err := os.Stat(filepath.Join(paths[0], "architecture.md")); tc.answerOnly && err != nil || !tc.answerOnly && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("safe/unsafe architecture retention differs: %v", err)
				}
			}
		})
	}
}

func TestRejectConfiguredKeyPublicationFields(t *testing.T) {
	const key = "fixture-secret"
	for _, tc := range []struct {
		name   string
		result Result
	}{
		{name: "fix data", result: Result{Changes: []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("fixture-\x00secret")}}}},
		{name: "fix path", result: Result{Changes: []workspace.Change{{Path: "fixture-secret", Mode: workspace.Regular}}}},
		{name: "audit path", result: Result{Artifacts: []workspace.File{{Path: "fixture-secret", Mode: workspace.Regular}}}},
		{name: "audit JSON data", result: Result{Artifacts: []workspace.File{{Path: "findings.json", Mode: workspace.Regular, Data: []byte(`{"value":"fixture-\u0073ecret"}`)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := rejectConfiguredKeyPublication(publication{artifacts: tc.result.Artifacts}, tc.result.Changes, "", key); err == nil || !strings.Contains(err.Error(), "configured provider key") {
				t.Fatalf("secret reached publication boundary: %v", err)
			}
		})
	}
}

func TestChangedFixUnusedSummaryDoesNotRejectConfiguredKey(t *testing.T) {
	const key = "fixture-secret"
	changes := []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("safe change\n")}}
	published, err := projectPublication(Job{Action: command.Fix}, Result{Answer: key, Changes: changes})
	if err != nil || published.answer != "" {
		t.Fatalf("changed Fix retained unused summary: publication=%+v error=%v", published, err)
	}
	if err := rejectConfiguredKeyPublication(published, changes, key); err != nil {
		t.Fatalf("unused summary blocked safe change: %v", err)
	}
	changes[0].Data = []byte(key)
	if err := rejectConfiguredKeyPublication(published, changes, key); err == nil {
		t.Fatal("configured key in published change was accepted")
	}
}

func TestIncompleteAuditRetainsMarkerWhenEveryArtifactContainsKey(t *testing.T) {
	const key = "fixture-secret"
	job := testJob(command.SecurityAudit)
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head,
		Artifacts: []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: []byte(key)}},
	}
	root := t.TempDir()
	e := Orchestrator{WorkRoot: root, Runner: Runner{Codex: config.CodexConfig{APIKey: key}}}
	if err := e.retainIncompleteAudit(&preparedAction{job: job}, result); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "audits", "incomplete-*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("incomplete audit marker missing: paths=%v error=%v", paths, err)
	}
	files, err := os.ReadDir(paths[0])
	if err != nil || len(files) != 1 || files[0].Name() != "INCOMPLETE.txt" {
		t.Fatalf("unsafe artifact retained: files=%v error=%v", files, err)
	}
}
