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
	"github.com/linkdata/minion/internal/monitor"
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
				runner := Runner{}
				if provider == "codex" {
					runner.Codex.APIKey = tc.key
				} else {
					runner.Claude.APIKey = tc.key
				}
				result, err := runner.runNative(t.Context(), modelRun{session: &session, provider: provider, dir: t.TempDir(), prompt: mainPrompt("inert fixture"), outputSchema: mainOutputSchema})
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

func TestAuditReportRedactsNULSplitConfiguredKeyBeforeCriticAndPublication(t *testing.T) {
	job := withClaudeCritic(auditRunnerJob(t))
	job.CriticMaxRounds = 1
	const key = "fixture-audit-secret"
	const splitKey = "fixture-audit-\x00secret"
	const criticKey = "fixture-critic-secret"
	report := "Audit evidence includes " + splitKey + " and " + criticKey + ".\n"
	want := "Audit evidence includes [redacted] and [redacted].\n"
	criticSawReports := false
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: key}, Claude: config.ClaudeConfig{APIKey: criticKey}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			for _, file := range auditArtifactsFixture(job, request.dir, report) {
				if file.Path == "architecture.md" {
					file.Data = []byte("Architecture includes " + splitKey + " and " + criticKey)
				}
				if file.Path == "coverage-ledger.json" {
					file.Data = []byte(strings.TrimSuffix(auditCoveredLedgerFixture, "}]") + `,"note":"fixture-audit-\u0000secret","fixture-audit-\u0073ecret":"fixture-audit-\u0073ecret","critic":"fixture-critic-secret","safe":"<safe>"}]`)
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
			if string(data) != want || strings.Contains(request.prompt, key) || strings.Contains(request.prompt, criticKey) || strings.Contains(request.prompt, "\x00") {
				t.Fatalf("critic received secret: report=%q prompt=%q", data, request.prompt)
			}
			architecture, err := os.ReadFile(filepath.Join(dir, "architecture.md"))
			if err != nil || string(architecture) != "Architecture includes [redacted] and [redacted]" {
				t.Fatalf("critic received raw architecture artifact: %q, %v", architecture, err)
			}
			ledger, err := os.ReadFile(filepath.Join(dir, "coverage-ledger.json"))
			if err != nil || !redactedAuditLedger(ledger) {
				t.Fatalf("critic received NUL-split JSON secret: %q, %v", ledger, err)
			}
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if err != nil || !criticSawReports || result.Answer != "" || result.Validate(job) != nil {
		t.Fatalf("audit result: critic saw reports=%t answer=%q error=%v", criticSawReports, result.Answer, err)
	}
	for _, file := range result.Artifacts {
		if strings.Contains(string(file.Data), key) || strings.Contains(string(file.Data), criticKey) || strings.Contains(string(file.Data), "\x00") {
			t.Fatalf("returned artifact contains configured key: %s", file.Path)
		}
		if file.Path == "REPORT.md" && string(file.Data) != want {
			t.Fatalf("returned report contains configured key: %q", file.Data)
		}
		if file.Path == "architecture.md" && string(file.Data) != "Architecture includes [redacted] and [redacted]" {
			t.Fatalf("returned architecture artifact contains configured key: %q", file.Data)
		}
		if file.Path == "coverage-ledger.json" && !redactedAuditLedger(file.Data) {
			t.Fatalf("returned JSON artifact contains configured key: %q", file.Data)
		}
	}
}

func TestAuditEncodedConfiguredKeyStopsBeforeCritic(t *testing.T) {
	const key = "fixture-audit-secret"
	const encoded = "fixture-audit-&#x73;ecret"
	for _, test := range []struct {
		name, path, content string
	}{
		{"Markdown", "architecture.md", "Architecture includes " + encoded},
		{"JSON", "coverage-ledger.json", strings.TrimSuffix(auditCoveredLedgerFixture, "}]") + `,"note":"` + encoded + `"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(auditRunnerJob(t))
			job.CriticMaxRounds = 1
			criticCalls := 0
			runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: key}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleCritic {
					criticCalls++
					return aicli.Result{Answer: approvedCritiqueJSON}, nil
				}
				for _, file := range auditArtifactsFixture(job, request.dir, "Safe report\n") {
					if file.Path == test.path {
						file.Data = []byte(test.content)
					}
					writeFixtureFile(t, request.outputDir, file.Path, file.Data)
				}
				return aicli.Result{Answer: proposalJSON("Model summary", "")}, nil
			}}
			result, err := runner.Run(t.Context(), job)
			if !errors.Is(err, monitor.ErrTerminalJob) || criticCalls != 0 || len(result.Calls) != 1 {
				t.Fatalf("encoded key reached critic or escaped terminal rejection: calls=%d receipts=%d error=%v", criticCalls, len(result.Calls), err)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("safe partial audit is invalid: %v", err)
			}
			if len(result.Artifacts) != len(auditArtifacts)-1 {
				t.Fatalf("safe partial artifacts were lost: count=%d", len(result.Artifacts))
			}
			for _, file := range result.Artifacts {
				if file.Path == test.path || artifactContainsConfiguredKey(file, key) {
					t.Fatalf("unsafe partial artifact retained: %q", file.Path)
				}
			}
		})
	}
}

func TestPartialAuditDropsMetadataWhenNormalizedTargetContainsKey(t *testing.T) {
	job := auditRunnerJob(t)
	key := job.Identity.Repository
	failure := errors.New("provider stopped")
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: key}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		writeFixtureFile(t, request.outputDir, "run-metadata.json", []byte(`{}`))
		writeFixtureFile(t, request.outputDir, "REPORT.md", []byte("Partial report\n"))
		return aicli.Result{}, failure
	}}
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, failure) || len(result.Artifacts) != 1 || result.Artifacts[0].Path != "REPORT.md" {
		t.Fatalf("partial audit retained unsafe metadata: %+v, %v", result, err)
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "artifact_metadata_invalid" {
			return
		}
	}
	t.Fatalf("partial audit lacks metadata diagnostic: %+v", result.Diagnostics)
}

func redactedAuditLedger(data []byte) bool {
	var ledger []map[string]any
	return json.Unmarshal(data, &ledger) == nil && len(ledger) == 1 &&
		ledger[0]["note"] == "[redacted]" && ledger[0]["[redacted]"] == "[redacted]" &&
		ledger[0]["critic"] == "[redacted]" &&
		ledger[0]["safe"] == "<safe>" && bytes.Contains(data, []byte("<safe>"))
}

func TestUnchangedAuditJSONPreservesOriginalBytes(t *testing.T) {
	root := t.TempDir()
	original := []byte(" [ { \"status\": \"covered\", \"note\": \"\\u0061 <safe>\", \"number\": 123, \"flag\": false, \"missing\": null } ]\n")
	writeFixtureFile(t, root, "coverage-ledger.json", original)
	files, diagnostics, err := readAuditArtifacts(root, false, "fixture-audit-secret")
	if err != nil || len(diagnostics) != 0 || len(files) != 1 || !bytes.Equal(files[0].Data, original) {
		t.Fatalf("unchanged JSON files=%+v diagnostics=%+v error=%v", files, diagnostics, err)
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
		{"case-folded status duplicate", "fixture-audit-secret", `{"status":"planned","STATUS":"covered"}`, "duplicate object keys"},
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
	result, err := runner.Run(t.Context(), job)
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
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				return aicli.Result{Answer: "public fixture-\x00secret text"}, nil
			}}
			if provider == "codex" {
				runner.Codex.APIKey = "fixture-secret"
			} else {
				runner.Claude.APIKey = "fixture-secret"
			}
			result, err := runner.Run(t.Context(), job)
			if err != nil || result.Answer != "public [redacted] text" {
				t.Fatalf("NUL-split key was exposed: answer=%q error=%v", result.Answer, err)
			}
		})
	}
}

func TestFixChangesRejectConfiguredKeysWithoutRejectingBinary(t *testing.T) {
	const key = "fixture-secret"
	for _, test := range []struct {
		name, key, data string
		want            bool
	}{
		{"literal", key, "prefix " + key, true},
		{"NUL split", key, "fixture-\x00secret", true},
		{"JSON escaped", key, `{"note":"fixture-\u0073ecret"}`, true},
		{"HTML numeric entity", key, "fixture-&#x73;ecret", true},
		{"JSON escaped entity", key, `{"note":"fixture-\u0026#x73;ecret"}`, true},
		{"literal ampersand key", "fixture-&amp;secret", "fixture-&amp;secret", true},
		{"marker-like key", "redacted", "[redacted]", true},
		{"benign binary", key, "x\x00y", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := containsConfiguredKey([]byte(test.data), test.key); got != test.want {
				t.Fatalf("configured key detection = %t, want %t", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		name, path, data, codexKey, claudeKey string
	}{
		{name: "Codex key in data", path: "source.txt", data: `{"note":"fixture-\u0073ecret"}`, codexKey: key},
		{name: "Codex key as HTML entity", path: "README.md", data: "fixture-&#x73;ecret", codexKey: key},
		{name: "Codex key in filename", path: key + ".txt", data: "safe", codexKey: key},
		{name: "Claude key in data", path: "source.txt", data: `{"note":"fixture-\u0073ecret"}`, claudeKey: key},
	} {
		for _, critic := range []bool{false, true} {
			t.Run(test.name+"/critic="+strconv.FormatBool(critic), func(t *testing.T) {
				job := testJob(command.Fix)
				if critic {
					job = withClaudeCritic(job)
				}
				calls := 0
				runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: test.codexKey}, Claude: config.ClaudeConfig{APIKey: test.claudeKey}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if request.role != roleMain {
						t.Fatal("critic received a fix containing a configured key")
					}
					writeFixtureFile(t, request.dir, test.path, []byte(test.data))
					return aicli.Result{Answer: proposalJSON("Done", "")}, nil
				}}
				result, err := runner.Run(t.Context(), job)
				if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "configured provider key") || calls != 1 || result.Answer != "" || len(result.Changes) != 0 {
					t.Fatalf("configured key reached fix result: calls=%d result=%+v error=%v", calls, result, err)
				}
			})
		}
	}
}
