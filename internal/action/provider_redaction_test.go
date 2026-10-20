package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func TestNativeProposalRedactsEncodedConfiguredKey(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, tc := range []struct{ name, key string }{
			{"quote", `fixture"secret`},
			{"backslash", `fixture\secret`},
			{"JSON HTML escape", "fixture<secret"},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("CODEX_HOME", t.TempDir())
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
					t.Setenv(key, "")
				}
				proposal := proposalJSON("public "+tc.key, "private "+tc.key)
				var event map[string]any
				script := "#!/bin/sh\ncat >/dev/null\n"
				if provider == "codex" {
					event = map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": proposal}}
					script += "printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}'\n"
				} else {
					event = map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(proposal), "modelUsage": map[string]any{"main": map[string]int{"inputTokens": 3, "cacheCreationInputTokens": 5, "cacheReadInputTokens": 7, "outputTokens": 11}}}
					script += `minion_session_flag=false
for minion_arg do
  if $minion_session_flag; then
    printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$minion_arg"
    break
  fi
  case "$minion_arg" in --session-id|--resume) minion_session_flag=true ;; esac
done
`
				}
				encoded, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				script += "printf '%s\\n' " + shellQuote(string(encoded)) + "\n"
				if provider == "codex" {
					script += "printf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":15,\"cached_input_tokens\":7,\"output_tokens\":11}}'\n"
				}
				fakeOnPath(t, provider, script)
				var session modelSession
				t.Cleanup(func() {
					if err := session.close(); err != nil {
						t.Error(err)
					}
				})
				runner := Runner{Codex: config.CodexConfig{APIKey: tc.key}, Claude: config.ClaudeConfig{APIKey: tc.key}}
				result, err := runner.runNative(context.Background(), modelRun{session: &session, provider: provider, dir: t.TempDir(), prompt: mainPrompt("inert fixture"), outputSchema: mainOutputSchema})
				if err != nil {
					t.Fatal(err)
				}
				answer, refutations, err := parseProposal(result.Answer, false)
				if err != nil || answer != "public [redacted]" || refutations != "private [redacted]" {
					t.Fatalf("decoded answer=%q refutations=%q error=%v", answer, refutations, err)
				}
			})
		}
	}
}

func TestAuditReportRedactsConfiguredKeyBeforeCriticAndPublication(t *testing.T) {
	job := withClaudeCritic(auditRunnerJob(t))
	job.CriticMaxRounds = 1
	const key = "fixture-audit-secret"
	report := "Audit evidence includes " + key + " and remains readable.\n"
	want := "Audit evidence includes [redacted] and remains readable.\n"
	criticSawReports := false
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: key}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			for _, file := range auditArtifactsFixture(job, request.dir, report) {
				if file.Path == "architecture.md" {
					file.Data = []byte("Architecture includes " + key)
				}
				writeFixtureFile(t, request.outputDir, file.Path, file.Data)
			}
			return aicli.Result{Answer: proposalJSON("Model summary", "")}, nil
		}
		for _, dir := range request.readOnlyDirs {
			data, err := os.ReadFile(filepath.Join(dir, "REPORT.md"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			criticSawReports = true
			if string(data) != want || strings.Contains(request.prompt, key) {
				t.Fatalf("critic received secret: report=%q prompt=%q", data, request.prompt)
			}
			architecture, err := os.ReadFile(filepath.Join(dir, "architecture.md"))
			if err != nil || string(architecture) != "Architecture includes [redacted]" {
				t.Fatalf("critic received raw architecture artifact: %q, %v", architecture, err)
			}
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || !criticSawReports || result.Answer != want || result.Validate(job) != nil {
		t.Fatalf("audit result: critic saw reports=%t answer=%q error=%v", criticSawReports, result.Answer, err)
	}
	for _, file := range result.Artifacts {
		if strings.Contains(string(file.Data), key) {
			t.Fatalf("published artifact contains configured key: %s", file.Path)
		}
		if file.Path == "REPORT.md" && string(file.Data) != want {
			t.Fatalf("published report contains configured key: %q", file.Data)
		}
		if file.Path == "architecture.md" && string(file.Data) != "Architecture includes [redacted]" {
			t.Fatalf("published architecture artifact contains configured key: %q", file.Data)
		}
	}
}

func TestIncompleteAuditReportRedactsConfiguredKey(t *testing.T) {
	job := auditRunnerJob(t)
	const key = "fixture-audit-secret"
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: key}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		for _, file := range auditArtifactsFixture(job, request.dir, "Partial "+key) {
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{}, errors.New("fixture main failure")
	}}
	result, err := runner.Run(context.Background(), job)
	if err == nil {
		t.Fatal("audit failure was not reported")
	}
	for _, file := range result.Artifacts {
		if file.Path == "REPORT.md" {
			if string(file.Data) != "Partial [redacted]" {
				t.Fatalf("incomplete report contains configured key: %q", file.Data)
			}
			return
		}
	}
	t.Fatal("safe partial report was not retained")
}

func TestAuditReportRedactionRechecksByteLimit(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "REPORT.md", []byte(strings.Repeat("x", maxOutputBytes)))
	if _, _, err := readAuditArtifacts(root, false, "x"); err == nil {
		t.Fatal("expanded redacted report exceeded publication limit")
	}
	files, diagnostics, err := readAuditArtifacts(root, true, "x")
	if err != nil || len(files) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "artifact_unavailable" {
		t.Fatalf("oversized partial report retained: files=%v diagnostics=%+v error=%v", files, diagnostics, err)
	}
}

func TestRunnerRedactsReassembledSecretWithoutCritic(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			job := testJob(command.Comment)
			job.Main.Provider = provider
			runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: "fixture-secret"}, Claude: config.ClaudeConfig{APIKey: "fixture-secret"}, runModel: func(context.Context, modelRun) (aicli.Result, error) {
				return aicli.Result{Answer: "public fixture-\x00secret text"}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if err != nil || result.Answer != "public [redacted] text" {
				t.Fatalf("NUL-split key was exposed: answer=%q error=%v", result.Answer, err)
			}
		})
	}
}
