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
	}{
		{name: "answer with other provider key", content: "public " + claudeKey},
		{name: "encoded answer key", content: `public fixture-claude-\u0073ecret`},
		{name: "audit artifact with NUL-split key", content: "public fixture-codex-\x00secret", audit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.audit {
				installActionTestSkills(t, testAuditSkillURL)
			}
			gitFixtureRemote(t)
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
					for i := range outcome.Result.Artifacts {
						if outcome.Result.Artifacts[i].Path == "architecture.md" {
							outcome.Result.Artifacts[i].Data = []byte(tc.content)
						}
					}
				} else {
					outcome.Result.Answer = tc.content
				}
				if err := outcome.Result.Validate(job); err != nil {
					t.Fatalf("hostile result was not otherwise publishable: %v", err)
				}
				f.ack(t, "/outcome", outcome, http.StatusOK, 3)
				got := f.result(t)
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: name, Action: action}
			request.Repository.Actions = []string{name}
			err := e.Execute(context.Background(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "configured provider key") || writes.Load() != 0 {
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
				if files, err := os.ReadDir(paths[0]); err != nil || len(files) != 1 || files[0].Name() != "INCOMPLETE.txt" {
					t.Fatalf("secret-bearing artifacts retained in private audit: files=%v error=%v", files, err)
				}
			}
		})
	}
}

func TestRejectConfiguredKeyResultFields(t *testing.T) {
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
			if err := rejectConfiguredKeyResult(tc.result, "", key); err == nil || !strings.Contains(err.Error(), "configured provider key") {
				t.Fatalf("secret reached publication boundary: %v", err)
			}
		})
	}
}
