package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
)

func TestOnlyWritableClaudeDirectoriesAreAdded(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir, alias, namedSkill := filepath.Join(root, "reports"), filepath.Join(root, "alias"), filepath.Join(root, "SKILL.md")
	for _, path := range []string{realDir, namedSkill} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, realDir, alias)
	output, outputAlias := filepath.Join(root, "output"), filepath.Join(root, "output-alias")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, output, outputAlias)
	binary := fakeExecutable(t, "claude", `printf '%s\n' "$@" > "$0.args"
cat >/dev/null
printf '%s\n' '`+successfulResult+`'
`)
	source := t.TempDir()
	settings := config.ClaudeConfig{ReadOnlyDirs: []string{alias, namedSkill}}
	if _, err := Run(context.Background(), source, "task", true, outputAlias, settings); err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(readFile(t, binary+".args")), "\n")
	var dirs []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--add-dir" {
			dirs = append(dirs, args[i+1])
		}
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dirs, []string{canonicalSource, output}) || !slices.Equal(settings.ReadOnlyDirs, []string{alias, namedSkill}) {
		t.Fatalf("unexpected write roots or mutated caller settings: dirs=%v settings=%v", dirs, settings.ReadOnlyDirs)
	}
	if slices.Contains(dirs, realDir) || slices.Contains(dirs, namedSkill) || slices.Contains(dirs, outputAlias) {
		t.Fatalf("read-only evidence or output alias was added: %v", dirs)
	}
}

const successfulResult = `{"type":"result","subtype":"success","is_error":false,"result":"final answer","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11},"modelUsage":{"claude-main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func fakeExecutable(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Claude CLI is not supported on native Windows")
	}
	if name == "claude" {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		// Native invocations echo their explicit host-selected conversation ID.
		// Discovery probes have no session selector and emit no such event.
		script = `minion_session_flag=false
for minion_arg do
  if $minion_session_flag; then
    printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$minion_arg"
    break
  fi
  case "$minion_arg" in --session-id|--resume) minion_session_flag=true ;; esac
done
` + script
		for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			t.Setenv(key, "")
		}
	}
	return filepath.Join(testsupport.FakeOnPath(t, name, "#!/bin/sh\n"+script), name)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRun(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "review", true: "fix"}[writable], func(t *testing.T) {
			workspace := t.TempDir()
			reports := t.TempDir()
			output := t.TempDir()
			binary := fakeExecutable(t, "claude", `printf '%s\n' "$@" > "$0.args"
pwd > "$0.cwd"
printf '%s\n' "${GH_TOKEN-absent},${GITHUB_TOKEN-absent},${NODE_OPTIONS-absent},${CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS-absent}" > "$0.env"
cat > "$0.prompt"
printf '%s\n' '`+successfulResult+`'
`)
			t.Setenv("GH_TOKEN", "dummy-token")
			t.Setenv("GITHUB_TOKEN", "dummy-token")
			t.Setenv("NODE_OPTIONS", "dummy-option")
			t.Setenv("CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS", "600000")
			result, err := Run(context.Background(), workspace, "Handle task", writable, output, config.ClaudeConfig{Model: "claude-test", Effort: "high", Skills: []string{skills.BuiltinAntiPromptInjection}, ReadOnlyDirs: []string{reports}})
			if err != nil {
				t.Fatal(err)
			}
			workspace, err = filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatal(err)
			}
			reports, err = filepath.EvalSymlinks(reports)
			if err != nil {
				t.Fatal(err)
			}
			output, err = filepath.EvalSymlinks(output)
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != "final answer" || !result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("result = %+v", result)
			}
			if readFile(t, binary+".env") != "absent,absent,absent,0\n" {
				t.Fatal("unsafe or incorrect environment reached Claude")
			}
			args := strings.Split(strings.TrimSuffix(readFile(t, binary+".args"), "\n"), "\n")
			if strings.Contains(strings.Join(args, "\n"), "--include-partial-messages") {
				t.Fatal("unused token deltas consume the bounded event budget")
			}
			if !slices.Contains(args, "--setting-sources=") {
				t.Fatalf("empty setting-source selection missing: %q", args)
			}
			for _, flag := range []string{"--safe-mode", "--strict-mcp-config", "--disable-slash-commands", "--no-chrome"} {
				if !slices.Contains(args, flag) {
					t.Fatalf("missing Claude hardening flag %q: %q", flag, args)
				}
			}
			values := map[string]string{}
			var addDirs []string
			for i, arg := range args[:len(args)-1] {
				values[arg] = args[i+1]
				if arg == "--add-dir" {
					addDirs = append(addDirs, args[i+1])
				}
			}
			for key, want := range map[string]string{"--permission-mode": "dontAsk", "--tools": "Bash", "--allowedTools": "Bash", "--permission-prompts": "none", "--model": "claude-test", "--effort": "high", "--mcp-config": `{"mcpServers":{}}`} {
				if got, ok := values[key]; !ok || got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			var settings struct {
				DisableClaudeAIConnectors bool  `json:"disableClaudeAiConnectors"`
				AutoMemoryEnabled         *bool `json:"autoMemoryEnabled"`
				Sandbox                   struct {
					Enabled                  bool `json:"enabled"`
					FailIfUnavailable        bool `json:"failIfUnavailable"`
					AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
					Credentials              struct {
						EnvVars []struct{ Name, Mode string } `json:"envVars"`
					} `json:"credentials"`
				} `json:"sandbox"`
			}
			if err := json.Unmarshal([]byte(values["--settings"]), &settings); err != nil {
				t.Fatal(err)
			}
			policy := settings.Sandbox
			if !settings.DisableClaudeAIConnectors || settings.AutoMemoryEnabled == nil || *settings.AutoMemoryEnabled {
				t.Fatalf("Claude connected tools or memory enabled: %+v", settings)
			}
			if !policy.Enabled || !policy.FailIfUnavailable || policy.AllowUnsandboxedCommands {
				t.Fatalf("unsafe sandbox policy: %+v", policy)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(values["--settings"]), &raw); err != nil {
				t.Fatal(err)
			}
			var nativeSandbox map[string]json.RawMessage
			if err := json.Unmarshal(raw["sandbox"], &nativeSandbox); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"filesystem", "network"} {
				if _, authored := nativeSandbox[name]; authored {
					t.Fatalf("Minion authored Claude %s rules: %s", name, raw["sandbox"])
				}
			}
			wantDenials := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"}
			var denials []string
			for _, rule := range policy.Credentials.EnvVars {
				if rule.Mode != "deny" {
					t.Fatalf("credential environment rule is not denied: %+v", rule)
				}
				denials = append(denials, rule.Name)
			}
			if !slices.Equal(denials, wantDenials) {
				t.Fatalf("credential environment denials = %v, want %v", denials, wantDenials)
			}
			wantAddDirs := []string{output}
			if writable {
				wantAddDirs = append([]string{workspace}, wantAddDirs...)
			}
			if !slices.Equal(addDirs, wantAddDirs) {
				t.Fatalf("--add-dir grants = %v, want %v", addDirs, wantAddDirs)
			}
			cwd := strings.TrimSpace(readFile(t, binary+".cwd"))
			prompt := readFile(t, binary+".prompt")
			if !strings.Contains(prompt, "Source snapshot: ") || !strings.Contains(prompt, workspace) || !strings.Contains(prompt, "SKILL.md") || !strings.HasSuffix(prompt, "Handle task") {
				t.Fatalf("prompt = %q", prompt)
			}
			if cwd == workspace || filepath.Dir(cwd) != filepath.Dir(workspace) {
				t.Fatalf("runtime directory = %q", cwd)
			}
			if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime retained: %v", err)
			}
		})
	}
}

func TestRunFiltersWritablePATHEntries(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary, err := filepath.EvalSymlinks(fakeExecutable(t, "claude", `printf '%s\n' "$PATH" > "$0.path"
cat >/dev/null
printf '%s\n' '`+successfulResult+`'
`))
	if err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := session.prepare(workspace, binary, true, output, config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	blocked := []string{filepath.Join(session.runtimeDir, "bin"), filepath.Join(workspace, "bin"), filepath.Join(output, "bin")}
	for _, dir := range blocked {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join(append(blocked, filepath.Dir(binary), "/usr/bin"), string(os.PathListSeparator)))
	if result, err := session.Run(context.Background(), workspace, "task", true, output, config.ClaudeConfig{}); err != nil || result.Answer != "final answer" {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	path := filepath.SplitList(strings.TrimSpace(readFile(t, binary+".path")))
	for _, dir := range blocked {
		if slices.Contains(path, dir) {
			t.Fatalf("model-writable PATH entry %q reached Claude: %q", dir, path)
		}
	}
	if !slices.Contains(path, filepath.Dir(binary)) {
		t.Fatalf("trusted CLI bin missing from PATH: %q", path)
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"missing", ""},
		{"malformed", "{"},
		{"failed", `{"type":"result","subtype":"error_during_execution","is_error":true}`},
		{"empty", `{"type":"result","subtype":"success","result":" "}`},
		{"denied", `{"type":"result","subtype":"success","result":"declined","permission_denials":[{}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'RESULT'\n"+tc.output+"\nRESULT\n")
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if err == nil || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{}) {
				t.Fatalf("Run = %+v, %v", result, err)
			}
		})
	}
}

func TestRunBackgroundWaitCeiling(t *testing.T) {
	emit := func(data string) string { return "cat <<'RESULT'\n" + data + "\nRESULT\n" }
	known := usage.Tokens{Input: 15, CachedInput: 7, Output: 11}
	for _, tc := range []struct {
		name, stderr, script, answer, extraFailure string
		tokens                                     usage.Tokens
		fails, outputLimit                         bool
	}{
		{name: "native ceiling", stderr: waitCeilingDiagnostic, script: emit(successfulResult), tokens: known, fails: true},
		{name: "oversized native ceiling", stderr: waitCeilingDiagnostic + strings.Repeat(" ", bounded.DiagnosticBytes), script: emit(successfulResult), tokens: known, fails: true, extraFailure: "stderr line exceeded"},
		{name: "oversized stderr cannot certify completion", stderr: strings.Repeat("x", bounded.DiagnosticBytes+1), script: emit(successfulResult), tokens: known, fails: true, extraFailure: "stderr line exceeded"},
		{name: "answer text", script: emit(strings.Replace(successfulResult, "final answer", waitCeilingDiagnostic, 1)), answer: waitCeilingDiagnostic, tokens: known},
		{name: "early diagnostic outlives tail", stderr: waitCeilingDiagnostic + "\n" + strings.Repeat("later diagnostic\n", 2000), script: emit(successfulResult), tokens: known, fails: true},
		{name: "diagnostic after oversized line", stderr: strings.Repeat("x", bounded.DiagnosticBytes+1) + "\n" + waitCeilingDiagnostic, script: emit(successfulResult), tokens: known, fails: true},
		{name: "quota", stderr: waitCeilingDiagnostic, script: emit(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792200000}}`), fails: true, extraFailure: "resets at 2026-10-17T01:20:00Z"},
		{name: "output", stderr: waitCeilingDiagnostic, script: "head -c 4194305 /dev/zero | tr '\\000' x\n", fails: true, outputLimit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'DIAGNOSTIC' >&2\n"+tc.stderr+"\nDIAGNOSTIC\n"+tc.script)
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if (err != nil) != tc.fails || result.UsageComplete == tc.fails || result.Tokens != tc.tokens {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			if tc.fails {
				if result.Answer != "" || !strings.Contains(err.Error(), "wait ceiling") || !strings.Contains(err.Error(), tc.extraFailure) || tc.outputLimit && !errors.Is(err, aicli.ErrOutputLimit) {
					t.Fatalf("ceiling returned an answer or lost its cause: %+v, %v", result, err)
				}
			} else if result.Answer != tc.answer {
				t.Fatalf("answer = %q, want %q", result.Answer, tc.answer)
			}
		})
	}
}

func TestRunStructuredOutput(t *testing.T) {
	const schema = `{"type":"object","required":["answer","refutations"],"properties":{"answer":{"type":"string"},"refutations":{"type":"string"}},"additionalProperties":false}`
	const structured = `{"answer":"public","refutations":"private"}`
	for _, tc := range []struct {
		name, output string
		fails        bool
	}{
		{"success", strings.TrimSuffix(successfulResult, "}") + `,"structured_output":` + structured + `}`, false},
		{"final schema after background completion", `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}` + "\n" + successfulResult + "\n" + `{"type":"system","subtype":"background_tasks_changed","tasks":[]}` + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"structured_output":` + structured + `}`, false},
		{"missing", successfulResult, true},
		{"null", strings.TrimSuffix(successfulResult, "}") + `,"structured_output":null}`, true},
		{"failed", `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"structured_output":` + structured + `}`, true},
		{"denied", `{"type":"result","subtype":"success","structured_output":` + structured + `,"permission_denials":[{}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "printf '%s\\n' \"$@\" > \"$0.args\"\ncat >/dev/null\ncat <<'RESULT'\n"+numberResultEvents(tc.output)+"\nRESULT\n")
			result, err := Run(context.Background(), t.TempDir(), "task", true, "", config.ClaudeConfig{OutputSchema: schema})
			if (err != nil) != tc.fails || tc.fails && (result.Answer != "" || result.UsageComplete) || !tc.fails && result.Answer != structured {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			if !strings.Contains(readFile(t, binary+".args"), "--json-schema\n"+schema+"\n") {
				t.Fatal("schema not passed to CLI")
			}
		})
	}
}

func TestRunRejectsUnsafePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Claude CLI is not supported on native Windows")
	}
	workspace := t.TempDir()
	workspaceChild := filepath.Join(workspace, "evidence")
	if err := os.Mkdir(workspaceChild, 0700); err != nil {
		t.Fatal(err)
	}
	outputParent := t.TempDir()
	output := filepath.Join(outputParent, "reports")
	outputChild := filepath.Join(output, "sub")
	if err := os.MkdirAll(outputChild, 0700); err != nil {
		t.Fatal(err)
	}
	fakeExecutable(t, "claude", "exit 99\n")
	const pathSecret = "dummy-secret"
	for _, tc := range []struct {
		name, dir, output string
		settings          config.ClaudeConfig
		failure           string
		cause             error
	}{
		{name: "empty source snapshot", failure: "path is required"},
		{name: "root source snapshot", dir: "/", failure: "non-root directory"},
		{name: "same output", dir: workspace, output: workspace, failure: "must be separate from source snapshot"},
		{name: "ancestor output", dir: workspace, output: filepath.Dir(workspace), failure: "must be separate from source snapshot"},
		{name: "missing output", dir: workspace, output: filepath.Join(outputParent, pathSecret), settings: config.ClaudeConfig{APIKey: pathSecret}, failure: "claude output directory", cause: os.ErrNotExist},
		{name: "relative skill", dir: workspace, settings: config.ClaudeConfig{Skills: []string{"SKILL.md"}}, failure: "absolute SKILL.md"},
		{name: "read root", dir: workspace, settings: config.ClaudeConfig{ReadOnlyDirs: []string{"/"}}, failure: "non-root directory"},
		{name: "missing read", dir: workspace, settings: config.ClaudeConfig{APIKey: pathSecret, ReadOnlyDirs: []string{filepath.Join(outputParent, pathSecret)}}, failure: "claude read-only directory", cause: os.ErrNotExist},
		{name: "read output", dir: workspace, output: output, settings: config.ClaudeConfig{ReadOnlyDirs: []string{output}}, failure: "read-only directory overlaps"},
		{name: "read output ancestor", dir: workspace, output: output, settings: config.ClaudeConfig{ReadOnlyDirs: []string{outputParent}}, failure: "read-only directory overlaps"},
		{name: "read inside output", dir: workspace, output: output, settings: config.ClaudeConfig{ReadOnlyDirs: []string{outputChild}}, failure: "read-only directory overlaps"},
		{name: "read workspace ancestor", dir: workspace, settings: config.ClaudeConfig{ReadOnlyDirs: []string{filepath.Dir(workspace)}}, failure: "read-only directory overlaps"},
		{name: "read inside writable workspace", dir: workspace, settings: config.ClaudeConfig{ReadOnlyDirs: []string{workspaceChild}}, failure: "read-only directory overlaps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), tc.dir, "task", true, tc.output, tc.settings); err == nil || !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("invalid path refusal = %v; want %q", err, tc.failure)
			} else if tc.cause != nil && (!errors.Is(err, tc.cause) || strings.Contains(err.Error(), pathSecret) || !strings.Contains(err.Error(), "[redacted]")) {
				t.Fatalf("path refusal lost cause or redaction: %v", err)
			}
		})
	}
	inside := filepath.Join(workspace, "claude")
	if err := os.WriteFile(inside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace)
	if _, err := Run(context.Background(), workspace, "task", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "inside the untrusted source snapshot") {
		t.Fatalf("executable inside source snapshot refusal = %v", err)
	}
}

func TestRunRejectsSkillInsideOutput(t *testing.T) {
	source, output := t.TempDir(), t.TempDir()
	skill := filepath.Join(output, "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: test\n---\nTest skill.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := fakeExecutable(t, "claude", "touch \"$0.invoked\"\nexit 99\n")
	if _, err := Run(context.Background(), source, "task", false, output, config.ClaudeConfig{Skills: []string{skill}}); err == nil || !strings.Contains(err.Error(), "inside a writable directory") {
		t.Fatalf("accepted writable skill: %v", err)
	}
	if _, err := os.Stat(binary + ".invoked"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid skill started Claude: %v", err)
	}
}

func TestChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix environment fixture; native Windows Claude is unsupported")
	}
	got, err := childEnvironment([]string{"HOME=/home/operator", "PATH=/bin::.:rel", "CLAUDE_CONFIG_DIR=/wrong-native-home", "ANTHROPIC_API_KEY=", "anthropic_api_key=dummy", "GH_TOKEN=dummy", "GITHUB_TOKEN=dummy", "GIT_CONFIG_GLOBAL=/private-git-config", "GIT_DIR=/private-git-dir", "NODE_OPTIONS=dummy", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0", "BASH_ENV=bad", "HTTPS_PROXY=http://proxy.example:8080", "https_proxy=http://lower.example:3128", "http_proxy=http://lower.example:3128", "all_proxy=socks5://lower.example:1080", "no_proxy=localhost", "Https_Proxy=http://ignored.example:3128", "invalid"}, "/run", "/cache", "", "/home/operator/.claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	bin, err := aicli.ResolvePath("/bin")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "PATH="+bin) {
		t.Fatalf("unsafe CLI PATH: %q", got)
	}
	for _, required := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing local Git setting %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "/wrong-native-home") {
		t.Fatalf("inherited native home overrode selected configuration: %q", got)
	}
	if strings.Contains(joined, "/private-git-") {
		t.Fatalf("inherited host Git setting: %s", joined)
	}

	for _, key := range []string{"ANTHROPIC_API_KEY", "anthropic_api_key", "GH_TOKEN", "GITHUB_TOKEN", "NODE_OPTIONS", "SUBPROCESS_ENV_SCRUB", "BASH_ENV", "Https_Proxy"} {
		if strings.Contains(joined, key+"=") {
			t.Fatalf("unsafe environment key %s", key)
		}
	}
	if !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/home/operator/.claude") || !strings.Contains(joined, "GOCACHE=/cache") || !strings.Contains(joined, "HTTPS_PROXY=http://proxy.example:8080") {
		t.Fatalf("missing CLI configuration: %q", got)
	}
	for _, entry := range []string{"https_proxy=http://lower.example:3128", "http_proxy=http://lower.example:3128", "all_proxy=socks5://lower.example:1080", "no_proxy=localhost"} {
		if !strings.Contains(joined, entry) {
			t.Fatalf("proxy spelling/value was not preserved: %q", got)
		}
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		found := false
		for _, rule := range credentialDenials() {
			if rule["name"] == key && rule["mode"] == "deny" {
				found = true
			}
		}
		if !found {
			t.Fatalf("credential %s is not denied to Bash", key)
		}
	}
}

func TestRunRefusesNativeWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows only")
	}
	result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "requires Linux or macOS") || result.Answer != "" {
		t.Fatalf("unsupported native Claude was launched: %+v, %v", result, err)
	}
}

func TestChildEnvironmentRejectsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{"ANTHROPIC_API_KEY", "dummy-secret"},
		{"CLAUDE_CODE_OAUTH_TOKEN", "dummy-secret"},
		{"HTTP_PROXY", "http://user:dummy-secret@proxy.example:8080"},
		{"HTTPS_PROXY", "user:dummy-secret@proxy.example:8080"},
		{"ALL_PROXY", "socks5://dummy-secret@proxy.example:1080"},
		{"HTTPS_PROXY", "http://dummy-secret%"},
		{"HTTP_PROXY", "/dummy-secret"},
		{"HTTPS_PROXY", "http:/user:dummy-secret@proxy.example:8080"},
		{"HTTP_PROXY", "http:user:dummy-secret@proxy.example:8080"},
		{"http_proxy", "http://user:dummy-secret@proxy.example:8080"},
		{"https_proxy", "user:dummy-secret@proxy.example:8080"},
		{"all_proxy", "socks5://dummy-secret@proxy.example:1080"},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			env, err := childEnvironment([]string{tc.name + "=" + tc.value}, "/run", "/cache", "", "")
			if env != nil || err == nil || !strings.Contains(err.Error(), tc.name) || strings.Contains(err.Error(), "dummy-secret") {
				t.Fatalf("credential environment accepted or exposed in error: %v, %v", env, err)
			}
		})
	}
}

func TestCredentialEnvironmentStopsCLI(t *testing.T) {
	binary := fakeExecutable(t, "claude", "touch \"$0.called\"\nexit 99\n")
	t.Setenv("ANTHROPIC_API_KEY", "dummy-secret")
	if _, err := Validate(context.Background(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("Validate accepted environment credentials: %v", err)
	}
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !result.UsageComplete || result.Tokens != (usage.Tokens{}) || result.Answer != "" {
		t.Fatalf("Run accepted environment credentials or lost complete zero usage: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(binary + ".called"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CLI launched with credentials: %v", err)
	}
}

func TestRunUsageLimitKeepsSafeDiagnosticAndExitCause(t *testing.T) {
	const quota = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792200000}}`
	const failed = `{"type":"result","subtype":"success","is_error":true,"result_index":0,"result":"PRIVATE_RESULT","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			output := "cat <<'RESULT'\n" + quota + "\n" + failed + "\nRESULT\nexit 7\n"
			if malformed {
				output = "printf '%s\\n' '" + assistantUsageEvent("partial", 3, 5, 7, 11) + "' '" + quota + "'\nprintf '{'\nexec sleep 30\n"
			}
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'DIAG' >&2\nPRIVATE_STDERR\nforged journal line\nDIAG\n"+output)
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			var exit *exec.ExitError
			if err == nil || !errors.As(err, &exit) || !strings.Contains(err.Error(), "resets at 2026-10-17T01:20:00Z") || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "forged journal line") || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) || malformed && !strings.Contains(err.Error(), "invalid Claude JSONL event") {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestRunQuietNativeFailureHasUsefulDiagnostic(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nexit 7\n")
	_, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(err.Error(), "no further diagnostics") || strings.Contains(err.Error(), `: ""`) {
		t.Fatalf("quiet failure lost cause or useful diagnostic: %v", err)
	}
}

func TestRunStartupFailureRetainsBoundedQuotedDiagnostic(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nhead -c 16384 /dev/zero | tr '\\000' x >&2\nprintf 'startup failed\\nforged journal line' >&2\nexit 7\n")
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(err.Error(), `startup failed\nforged journal line`) || strings.Contains(err.Error(), "\nforged journal line") || len(err.Error()) > 9000 || result.Answer != "" || result.UsageComplete {
		t.Fatalf("lost or unsafe startup diagnostic: result=%+v error=%v", result, err)
	}
}

func TestRunRedactsKeyFragmentAtStderrTail(t *testing.T) {
	key := "KEYPREFIX__LEAKSUFFIX"
	stderr := key + strings.Repeat("x", bounded.DiagnosticBytes-len("LEAKSUFFIX")-len("rate limit reached")) + "rate limit reached"
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s' "+aicli.ShellQuote(stderr)+" >&2\nexit 7\n")
	_, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: key})
	if err == nil || strings.Contains(err.Error(), "LEAKSUFFIX") || !strings.Contains(err.Error(), "rate limit reached") {
		t.Fatalf("clipped stderr exposed key fragment or lost limit diagnostic: %v", err)
	}
}

func TestRunQuotesNativeFailureSubtype(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'RESULT'\n"+`{"type":"result","subtype":"failed\nforged journal line","is_error":true}`+"\nRESULT\n")
	_, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || strings.Contains(err.Error(), "\nforged journal line") || !strings.Contains(err.Error(), `failed\nforged journal line`) {
		t.Fatalf("unquoted native failure: %v", err)
	}
}

func TestRunRedactsKeyBeforeClippingNativeFailureSubtype(t *testing.T) {
	key := "KEYPREFIX__LEAKSUFFIX"
	subtype := strings.Repeat("x", bounded.DiagnosticBytes-5) + key
	event, err := json.Marshal(map[string]any{"type": "result", "subtype": subtype, "is_error": true})
	if err != nil {
		t.Fatal(err)
	}
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' "+aicli.ShellQuote(string(event))+"\n")
	_, err = Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: key})
	if err == nil || strings.Contains(err.Error(), "KEYPR") {
		t.Fatalf("clipped native failure exposed key fragment: %v", err)
	}
}

func TestRunRequiresNativeConversationIdentity(t *testing.T) {
	for _, test := range []struct{ name, init, failure string }{
		{"missing", "", "missing Claude conversation identity"},
		{"foreign", `{"type":"system","subtype":"init","session_id":"other"}`, "conversation identity changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "exit 99\n")
			// Override the normal fake's valid identity prelude for this boundary control.
			script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '" + test.init + "' '" + successfulResult + "'\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if err == nil || !strings.Contains(err.Error(), test.failure) || result.Answer != "" || result.UsageComplete {
				t.Fatalf("native identity refusal: %+v, %v", result, err)
			}
		})
	}
}

func TestRunParseFailureCancelsActiveCLI(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+assistantUsageEvent("partial", 3, 5, 7, 2)+"' '{bad'\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	result, err := Run(ctx, t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "invalid Claude JSONL event") || ctx.Err() != nil || time.Since(started) > 5*time.Second || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 2}) {
		t.Fatalf("parse failure did not stop active CLI: %+v, %v, parent=%v", result, err, ctx.Err())
	}
}

func TestRunFinalUnterminatedFailureKeepsSafeDiagnosticAndUsage(t *testing.T) {
	for _, tc := range []struct{ name, event, failure string }{
		{"quota", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, "provider usage limit reached"},
		{"pressure", `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command \"tests\" was stopped because the system is running low on memory"}`, "provider memory pressure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "cat >/dev/null\nprintf '%s\\n' " + aicli.ShellQuote(assistantUsageEvent("root", 3, 5, 7, 2)) + "\nprintf '%s' " + aicli.ShellQuote(tc.event) + "\nprintf 'PRIVATE_STDERR' >&2\nexit 7\n"
			fakeExecutable(t, "claude", script)
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(err.Error(), tc.failure) || strings.Contains(err.Error(), "PRIVATE_STDERR") || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 2}) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}
