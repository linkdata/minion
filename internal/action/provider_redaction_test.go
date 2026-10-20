package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func TestRunAIRedactsNULSplitSecretsInCriticEvidence(t *testing.T) {
	const key = "fixture-secret"
	const splitKey = "fixture-\x00secret"
	mainCalls, criticCalls := 0, 0
	runner := Runner{Codex: config.CodexConfig{APIKey: key}, Claude: config.ClaudeConfig{APIKey: key}}
	runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			mainCalls++
			if mainCalls == 1 {
				return aicli.Result{Answer: proposalJSON("candidate "+splitKey, "")}, nil
			}
			if strings.Contains(request.prompt, key) || strings.Contains(request.prompt, "\x00") || !strings.Contains(request.prompt, "[redacted]") {
				t.Fatalf("critic feedback reached revision prompt: %q", request.prompt)
			}
			return aicli.Result{Answer: proposalJSON("revised", "finding "+splitKey)}, nil
		}
		criticCalls++
		if strings.Contains(request.proposedAnswer, key) || strings.Contains(request.proposedAnswer, "\x00") || strings.Contains(request.criticPrompt("", ""), key) || strings.Contains(request.criticPrompt("", ""), "\x00") {
			t.Fatalf("critic received configured key: %+v", request)
		}
		if request.reviewPhase == reviewVerification && (len(request.refutations) != 1 || request.refutations[0].Text != "finding [redacted]") {
			t.Fatalf("private refutation reached verification: %+v", request.refutations)
		}
		if criticCalls == 1 {
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"feedback fixture-\u0000secret"}`}, nil
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}
	job := Job{Main: RoleSettings{Provider: "codex"}, Critic: &RoleSettings{Provider: "claude"}, CriticMaxRounds: 2}
	checkpoint := func(history []Refutation) error {
		if len(history) > 0 && (len(history) != 1 || history[0].Text != "finding [redacted]") {
			t.Fatalf("private refutation reached checkpoint: %+v", history)
		}
		return nil
	}
	answer, err := runner.runAI(context.Background(), job, t.TempDir(), "task", "", checkpoint, &modelSessions{})
	if err != nil || answer != "revised" || mainCalls != 2 || criticCalls != 3 {
		t.Fatalf("NUL-split redaction: answer=%q main=%d critic=%d error=%v", answer, mainCalls, criticCalls, err)
	}
}

func TestAuditReportRedactsNULSplitConfiguredKeyBeforeCriticAndPublication(t *testing.T) {
	job := withClaudeCritic(auditRunnerJob(t))
	job.CriticMaxRounds = 1
	const key = "fixture-audit-secret"
	const splitKey = "fixture-audit-\x00secret"
	report := "Audit evidence includes " + splitKey + " and remains readable.\n"
	want := "Audit evidence includes [redacted] and remains readable.\n"
	criticSawReports := false
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: key}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			for _, file := range auditArtifactsFixture(job, request.dir, report) {
				if file.Path == "architecture.md" {
					file.Data = []byte("Architecture includes " + splitKey)
				}
				if file.Path == "coverage-ledger.json" {
					file.Data = []byte(`[{"status":"covered","note":"fixture-audit-\u0000secret","fixture-audit-\u0073ecret":"fixture-audit-\u0073ecret","safe":"<safe>"}]`)
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
			if string(data) != want || strings.Contains(request.prompt, key) || strings.Contains(request.prompt, "\x00") {
				t.Fatalf("critic received secret: report=%q prompt=%q", data, request.prompt)
			}
			architecture, err := os.ReadFile(filepath.Join(dir, "architecture.md"))
			if err != nil || string(architecture) != "Architecture includes [redacted]" {
				t.Fatalf("critic received raw architecture artifact: %q, %v", architecture, err)
			}
			ledger, err := os.ReadFile(filepath.Join(dir, "coverage-ledger.json"))
			if err != nil || !redactedAuditLedger(ledger) {
				t.Fatalf("critic received NUL-split JSON secret: %q, %v", ledger, err)
			}
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || !criticSawReports || result.Answer != want || result.Validate(job) != nil {
		t.Fatalf("audit result: critic saw reports=%t answer=%q error=%v", criticSawReports, result.Answer, err)
	}
	for _, file := range result.Artifacts {
		if strings.Contains(string(file.Data), key) || strings.Contains(string(file.Data), "\x00") {
			t.Fatalf("published artifact contains configured key: %s", file.Path)
		}
		if file.Path == "REPORT.md" && string(file.Data) != want {
			t.Fatalf("published report contains configured key: %q", file.Data)
		}
		if file.Path == "architecture.md" && string(file.Data) != "Architecture includes [redacted]" {
			t.Fatalf("published architecture artifact contains configured key: %q", file.Data)
		}
		if file.Path == "coverage-ledger.json" && !redactedAuditLedger(file.Data) {
			t.Fatalf("published JSON artifact contains configured key: %q", file.Data)
		}
	}
}

func redactedAuditLedger(data []byte) bool {
	var ledger []map[string]any
	return json.Unmarshal(data, &ledger) == nil && len(ledger) == 1 &&
		ledger[0]["note"] == "[redacted]" && ledger[0]["[redacted]"] == "[redacted]" &&
		ledger[0]["safe"] == "<safe>" && bytes.Contains(data, []byte("<safe>"))
}

func TestUnchangedAuditJSONPreservesOriginalBytes(t *testing.T) {
	root := t.TempDir()
	original := []byte(" [ { \"status\": \"covered\", \"note\": \"\\u0061 <safe>\", \"number\": 123, \"flag\": false, \"missing\": null } ]\n")
	writeFixtureFile(t, root, "coverage-ledger.json", original)
	files, diagnostics, err := readAuditArtifacts(root, false, "fixture-audit-secret")
	if err != nil || len(diagnostics) != 0 || len(files) != 1 || !bytes.Equal(files[0].Data, original) {
		t.Fatalf("unchanged JSON bytes=%q diagnostics=%+v error=%v", files, diagnostics, err)
	}
}

func TestAuditJSONScalarConfiguredKeysAreRedacted(t *testing.T) {
	for _, literal := range []string{"123", "true", "false", "null"} {
		t.Run(literal, func(t *testing.T) {
			root := t.TempDir()
			writeFixtureFile(t, root, "coverage-ledger.json", []byte(`{"value":[`+literal+`]}`))
			files, diagnostics, err := readAuditArtifacts(root, false, literal)
			if err != nil || len(diagnostics) != 0 || len(files) != 1 || bytes.Contains(files[0].Data, []byte(literal)) {
				t.Fatalf("scalar redaction: files=%v diagnostics=%+v error=%v", files, diagnostics, err)
			}
			var parsed struct {
				Value []any `json:"value"`
			}
			if err := json.Unmarshal(files[0].Data, &parsed); err != nil || len(parsed.Value) != 1 || parsed.Value[0] != "[redacted]" {
				t.Fatalf("scalar key survived JSON redaction: %s, %+v, %v", files[0].Data, parsed, err)
			}
		})
	}
}

func TestAuditJSONRejectsUnsafeKeyEncodings(t *testing.T) {
	for _, test := range []struct {
		name, secret, source, want string
	}{
		{"nested escaped duplicate", "fixture-audit-secret", `{"outer":[{"note":"fixture-audit-\u0073ecret","n\u006fte":"safe"}]}`, "duplicate object keys"},
		{"redacted key collision", "fixture-audit-secret", `{"fixture-audit-secret":"A","[redacted]":"B"}`, "keys collide after redaction"},
		{"key in raw Unicode escape", "1234", `{"note":"\u1234"}`, "configured key in source syntax"},
		{"key in JSON punctuation", ":", `{"note":"safe"}`, "configured key in source syntax"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixtureFile(t, root, "coverage-ledger.json", []byte(test.source))
			if _, _, err := readAuditArtifacts(root, false, test.secret); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe JSON was accepted: %v", err)
			}
			files, diagnostics, err := readAuditArtifacts(root, true, test.secret)
			if err != nil || len(files) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "artifact_unavailable" {
				t.Fatalf("unsafe partial JSON retained: files=%d diagnostics=%+v error=%v", len(files), diagnostics, err)
			}
		})
	}
}

func TestAuditJSONRedactsMarkerLikeConfiguredKeys(t *testing.T) {
	for _, secret := range []string{"redacted", "[redacted]"} {
		t.Run(secret, func(t *testing.T) {
			root := t.TempDir()
			writeFixtureFile(t, root, "coverage-ledger.json", []byte(`{"note":`+strconv.Quote(secret)+`}`))
			files, diagnostics, err := readAuditArtifacts(root, false, secret)
			if err != nil || len(diagnostics) != 0 || len(files) != 1 || bytes.Contains(files[0].Data, []byte(secret)) {
				t.Fatalf("configured key survived JSON redaction: files=%v diagnostics=%+v error=%v", files, diagnostics, err)
			}
			var parsed struct {
				Note string `json:"note"`
			}
			if err := json.Unmarshal(files[0].Data, &parsed); err != nil || parsed.Note == "" || strings.Contains(parsed.Note, secret) {
				t.Fatalf("JSON redaction result=%q error=%v", files[0].Data, err)
			}
		})
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

func TestAuditJSONRedactionRechecksByteLimit(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "coverage-ledger.json", []byte(`{"note":"`+strings.Repeat("x", maxAuditArtifactBytes/3)+`"}`))
	if _, _, err := readAuditArtifacts(root, false, "x"); err == nil {
		t.Fatal("expanded JSON exceeded artifact byte limit")
	}
	files, diagnostics, err := readAuditArtifacts(root, true, "x")
	if err != nil || len(files) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "artifact_unavailable" {
		t.Fatalf("oversized partial JSON retained: files=%v diagnostics=%+v error=%v", files, diagnostics, err)
	}
}

func TestInvalidAuditJSONIsOmittedFromPartialArtifacts(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "coverage-ledger.json", []byte(`{"note":"fixture-audit-\u0073ecret"`))
	if _, _, err := readAuditArtifacts(root, false, "fixture-audit-secret"); err == nil || !strings.Contains(err.Error(), "audit JSON is invalid") {
		t.Fatalf("invalid complete audit JSON: %v", err)
	}
	files, diagnostics, err := readAuditArtifacts(root, true, "fixture-audit-secret")
	if err != nil || len(files) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "artifact_unavailable" {
		t.Fatalf("invalid partial JSON retained: files=%d diagnostics=%+v error=%v", len(files), diagnostics, err)
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
