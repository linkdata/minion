package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

const successfulResult = `{"type":"result","subtype":"success","is_error":false,"result":"final answer","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11},"modelUsage":{"claude-main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`

func fakeExecutable(t *testing.T, name, script string) string {
	t.Helper()
	if name == "claude" {
		for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
			t.Setenv(key, "")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUnixSocketWorkaround(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         bool
	}{
		{name: "omitted claude", want: true},
		{name: "empty claude", fields: `,"claude":{}`, want: true},
		{name: "null", fields: `,"claude":{"allow_all_unix_sockets":null}`, want: true},
		{name: "enabled", fields: `,"claude":{"allow_all_unix_sockets":true}`, want: true},
		{name: "disabled", fields: `,"claude":{"allow_all_unix_sockets":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := `{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tc.fields + `}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			fakeExecutable(t, "bwrap", "exit 99\n")
			fakeExecutable(t, "socat", "exit 99\n")
			binary := fakeExecutable(t, "claude", `printf '%s\n' "$@" > "$0.args"
if [ "$1" = '--version' ]; then
  printf '%s\n' '2.1.302 (Claude Code)'
elif [ "$4" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  input=$(cat)
  case "$input" in
    *minion-initialize*) printf '%s\n' '`+modelCatalogue+`' ;;
    *) printf '%s\n' '`+successfulResult+`' ;;
  esac
fi
`)
			settings, err := Validate(context.Background(), cfg.Claude)
			if err != nil {
				t.Fatal(err)
			}
			checkUnixSocketPolicy(t, binary, tc.want)
			if _, err := Run(context.Background(), t.TempDir(), "task", false, "", settings); err != nil {
				t.Fatal(err)
			}
			checkUnixSocketPolicy(t, binary, tc.want)
		})
	}
}

func checkUnixSocketPolicy(t *testing.T, binary string, want bool) {
	t.Helper()
	args := strings.Split(strings.TrimSpace(readFile(t, binary+".args")), "\n")
	for i, arg := range args[:len(args)-1] {
		if arg == "--settings" {
			var settings struct {
				Sandbox struct {
					Network struct {
						AllowAllUnixSockets *bool `json:"allowAllUnixSockets"`
					} `json:"network"`
				} `json:"sandbox"`
			}
			if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
				t.Fatal(err)
			}
			if got := settings.Sandbox.Network.AllowAllUnixSockets; got == nil || *got != want {
				t.Fatalf("allowAllUnixSockets = %v, want %t", got, want)
			}
			return
		}
	}
	t.Fatal("missing sandbox settings")
}

func TestRun(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "review", true: "fix"}[writable], func(t *testing.T) {
			workspace := t.TempDir()
			reports := t.TempDir()
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
			result, err := Run(context.Background(), workspace, "Handle task", writable, "", config.ClaudeConfig{Model: "claude-test", Effort: "high", Skills: []string{skills.BuiltinAntiPromptInjection}, ReadOnlyDirs: []string{reports}})
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
			values := map[string]string{}
			for i, arg := range args[:len(args)-1] {
				values[arg] = args[i+1]
			}
			for key, want := range map[string]string{"--permission-mode": "dontAsk", "--tools": "Bash", "--allowedTools": "Bash", "--permission-prompts": "none", "--model": "claude-test", "--effort": "high", "--setting-sources": "", "--mcp-config": `{"mcpServers":{}}`} {
				if got, ok := values[key]; !ok || got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			var settings struct {
				Sandbox struct {
					Enabled                  bool `json:"enabled"`
					FailIfUnavailable        bool `json:"failIfUnavailable"`
					AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
					Filesystem               struct {
						DenyRead   []string `json:"denyRead"`
						DenyWrite  []string `json:"denyWrite"`
						AllowWrite []string `json:"allowWrite"`
					} `json:"filesystem"`
					Network struct {
						AllowedDomains  []string `json:"allowedDomains"`
						StrictAllowlist bool     `json:"strictAllowlist"`
					} `json:"network"`
				} `json:"sandbox"`
			}
			if err := json.Unmarshal([]byte(values["--settings"]), &settings); err != nil {
				t.Fatal(err)
			}
			policy := settings.Sandbox
			if !policy.Enabled || !policy.FailIfUnavailable || policy.AllowUnsandboxedCommands || !policy.Network.StrictAllowlist || len(policy.Network.AllowedDomains) != 0 || len(policy.Filesystem.DenyRead) != 1 || policy.Filesystem.DenyRead[0] != "/" {
				t.Fatalf("unsafe sandbox policy: %+v", policy)
			}
			contains := func(paths []string, path string) bool {
				for _, value := range paths {
					if value == path {
						return true
					}
				}
				return false
			}
			if contains(policy.Filesystem.AllowWrite, workspace) != writable || !contains(policy.Filesystem.DenyWrite, reports) {
				t.Fatalf("wrong workspace/report permissions: %+v", policy.Filesystem)
			}
			if writable && !contains(policy.Filesystem.DenyWrite, filepath.Join(workspace, ".git")) || !writable && !contains(policy.Filesystem.DenyWrite, workspace) {
				t.Fatal("read-only protections missing")
			}
			prompt := readFile(t, binary+".prompt")
			if !strings.Contains(prompt, "Source snapshot: ") || !strings.Contains(prompt, workspace) || !strings.Contains(prompt, "SKILL.md") || !strings.HasSuffix(prompt, "Handle task") {
				t.Fatalf("prompt = %q", prompt)
			}
			cwd := strings.TrimSpace(readFile(t, binary+".cwd"))
			if cwd == workspace || filepath.Dir(cwd) != filepath.Dir(workspace) {
				t.Fatalf("runtime directory = %q", cwd)
			}
			if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime retained: %v", err)
			}
		})
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"missing", ""},
		{"malformed", "{"},
		{"failed", `{"type":"result","subtype":"error_during_execution","is_error":true}`},
		{"empty", `{"type":"result","subtype":"success","result":" "}`},
		{"denied", `{"type":"result","subtype":"success","result":"declined","permission_denials":[{}]}`},
		{"duplicate", successfulResult + "\n" + successfulResult},
		{"unfinished background task", `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}` + "\n" + successfulResult},
		{"background completion without final answer", `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}` + "\n" + successfulResult + "\n" + `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'RESULT'\n"+tc.output+"\nRESULT\n")
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if err == nil || result.Answer != "" || result.UsageComplete {
				t.Fatalf("Run = %+v, %v", result, err)
			}
		})
	}
}

func TestRunBackgroundWaitCeiling(t *testing.T) {
	const diagnostic = "Background tasks still running 10m after the last turn (subagent reviewer); stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."
	for _, tc := range []struct {
		name, stderr, answer string
		fails                bool
	}{
		{name: "native ceiling", stderr: diagnostic, answer: "final answer", fails: true},
		{name: "other diagnostics", stderr: "prefix: " + diagnostic, answer: "final answer"},
		{name: "answer text", answer: diagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := strings.Replace(successfulResult, "final answer", tc.answer, 1)
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'DIAGNOSTIC' >&2\n"+tc.stderr+"\nDIAGNOSTIC\ncat <<'RESULT'\n"+output+"\nRESULT\n")
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if (err != nil) != tc.fails || result.UsageComplete == tc.fails || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			if tc.fails {
				if result.Answer != "" || !strings.Contains(err.Error(), "wait ceiling") {
					t.Fatalf("ceiling returned an answer or lost its diagnostic: %+v, %v", result, err)
				}
			} else if result.Answer != tc.answer {
				t.Fatalf("answer = %q, want %q", result.Answer, tc.answer)
			}
		})
	}
}

func TestRunStructuredOutput(t *testing.T) {
	const schema = `{"type":"object","required":["answer","review_notes"],"properties":{"answer":{"type":"string"},"review_notes":{"type":"string"}},"additionalProperties":false}`
	const structured = `{"answer":"public","review_notes":"private"}`
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
	workspace := t.TempDir()
	fakeExecutable(t, "claude", "exit 99\n")
	for _, tc := range []struct {
		name, dir, output string
		settings          config.ClaudeConfig
	}{
		{name: "empty source snapshot"},
		{name: "root source snapshot", dir: "/"},
		{name: "same output", dir: workspace, output: workspace},
		{name: "ancestor output", dir: workspace, output: filepath.Dir(workspace)},
		{name: "relative skill", dir: workspace, settings: config.ClaudeConfig{Skills: []string{"SKILL.md"}}},
		{name: "read root", dir: workspace, settings: config.ClaudeConfig{ReadOnlyDirs: []string{"/"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), tc.dir, "task", true, tc.output, tc.settings); err == nil {
				t.Fatal("accepted invalid paths")
			}
		})
	}
	inside := filepath.Join(workspace, "claude")
	if err := os.WriteFile(inside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace)
	if _, err := Run(context.Background(), workspace, "task", false, "", config.ClaudeConfig{}); err == nil {
		t.Fatal("accepted executable inside source snapshot")
	}
}

func TestRunCancelled(t *testing.T) {
	fakeExecutable(t, "claude", "sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Run(ctx, t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if !errors.Is(err, context.Canceled) || result.UsageComplete || result.Answer != "" {
		t.Fatalf("Run = %+v, %v", result, err)
	}
}

func TestChildEnvironment(t *testing.T) {
	got, err := childEnvironment([]string{"HOME=/home/operator", "PATH=/bin", "CLAUDE_CONFIG_DIR=/home/operator/.claude", "ANTHROPIC_API_KEY=", "anthropic_api_key=dummy", "GH_TOKEN=dummy", "GITHUB_TOKEN=dummy", "NODE_OPTIONS=dummy", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0", "BASH_ENV=bad", "HTTPS_PROXY=http://proxy.example:8080", "invalid"}, "/run", "/cache", "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, key := range []string{"ANTHROPIC_API_KEY", "anthropic_api_key", "GH_TOKEN", "GITHUB_TOKEN", "NODE_OPTIONS", "SUBPROCESS_ENV_SCRUB", "BASH_ENV"} {
		if strings.Contains(joined, key+"=") {
			t.Fatalf("unsafe environment key %s", key)
		}
	}
	if !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/home/operator/.claude") || !strings.Contains(joined, "GOCACHE=/cache") || !strings.Contains(joined, "HTTPS_PROXY=http://proxy.example:8080") {
		t.Fatalf("missing CLI configuration: %q", got)
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTPS_PROXY"} {
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
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			env, err := childEnvironment([]string{tc.name + "=" + tc.value}, "/run", "/cache", "")
			if env != nil || err == nil || !strings.Contains(err.Error(), tc.name) || strings.Contains(err.Error(), "dummy-secret") {
				t.Fatalf("credential environment accepted or exposed in error: %v, %v", env, err)
			}
		})
	}
}

func TestCredentialEnvironmentStopsCLI(t *testing.T) {
	binary := fakeExecutable(t, "claude", "touch \"$0.called\"\nexit 99\n")
	t.Setenv("ANTHROPIC_API_KEY", "dummy-secret")
	for _, enabled := range []bool{false, true} {
		settings := config.ClaudeConfig{AllowAllUnixSockets: &enabled}
		if _, err := Validate(context.Background(), settings); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Fatalf("Validate accepted environment credentials: %v", err)
		}
		if _, err := Run(context.Background(), t.TempDir(), "task", false, "", settings); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Fatalf("Run accepted environment credentials: %v", err)
		}
	}
	if _, err := os.Stat(binary + ".called"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CLI launched with credentials: %v", err)
	}
}
