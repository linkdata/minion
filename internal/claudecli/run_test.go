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

const successfulResult = `{"type":"result","subtype":"success","is_error":false,"result":"final answer","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}`

func fakeExecutable(t *testing.T, name, script string) string {
	t.Helper()
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

func TestRun(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "review", true: "fix"}[writable], func(t *testing.T) {
			workspace := t.TempDir()
			reports := t.TempDir()
			binary := fakeExecutable(t, "claude", `printf '%s\n' "$@" > "$0.args"
pwd > "$0.cwd"
printf '%s\n' "${GH_TOKEN-absent},${GITHUB_TOKEN-absent},${NODE_OPTIONS-absent}" > "$0.env"
cat > "$0.prompt"
printf '%s\n' '`+successfulResult+`'
`)
			t.Setenv("GH_TOKEN", "dummy-token")
			t.Setenv("GITHUB_TOKEN", "dummy-token")
			t.Setenv("NODE_OPTIONS", "dummy-option")
			result, err := Run(context.Background(), workspace, "Handle task", writable, "", config.ClaudeConfig{Model: "claude-test", Effort: "high", Skills: []string{skills.BuiltinAntiPromptInjection}, ReadOnlyDirs: []string{reports}})
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != "final answer" || !result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("result = %+v", result)
			}
			if readFile(t, binary+".env") != "absent,absent,absent\n" {
				t.Fatal("unsafe environment reached Claude")
			}
			args := strings.Split(strings.TrimSuffix(readFile(t, binary+".args"), "\n"), "\n")
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
			if !strings.Contains(prompt, workspace) || !strings.Contains(prompt, "SKILL.md") || !strings.HasSuffix(prompt, "Handle task") {
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

func TestRunStructuredOutput(t *testing.T) {
	const schema = `{"type":"object","required":["approved","feedback"],"properties":{"approved":{"type":"boolean"},"feedback":{"type":"string"}},"additionalProperties":false}`
	const structured = `{"approved":true,"feedback":"ok"}`
	for _, tc := range []struct {
		name, output string
		fails        bool
	}{
		{"success", strings.TrimSuffix(successfulResult, "}") + `,"structured_output":` + structured + `}`, false},
		{"missing", successfulResult, true},
		{"null", strings.TrimSuffix(successfulResult, "}") + `,"structured_output":null}`, true},
		{"failed", `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"structured_output":` + structured + `}`, true},
		{"denied", `{"type":"result","subtype":"success","structured_output":` + structured + `,"permission_denials":[{}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "printf '%s\\n' \"$@\" > \"$0.args\"\ncat >/dev/null\ncat <<'RESULT'\n"+tc.output+"\nRESULT\n")
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{OutputSchema: schema})
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
		{name: "empty checkout"},
		{name: "root checkout", dir: "/"},
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
		t.Fatal("accepted executable inside checkout")
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
	got := childEnvironment([]string{"HOME=/home/operator", "PATH=/bin", "ANTHROPIC_API_KEY=dummy", "anthropic_api_key=dummy", "GH_TOKEN=dummy", "GITHUB_TOKEN=dummy", "NODE_OPTIONS=dummy", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0", "BASH_ENV=bad", "HTTPS_PROXY=dummy", "invalid"}, "/run", "/cache")
	joined := strings.Join(got, "\n")
	for _, key := range []string{"anthropic_api_key", "GH_TOKEN", "GITHUB_TOKEN", "NODE_OPTIONS", "SUBPROCESS_ENV_SCRUB", "BASH_ENV"} {
		if strings.Contains(joined, key+"=") {
			t.Fatalf("unsafe environment key %s", key)
		}
	}
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=dummy") || !strings.Contains(joined, "GOCACHE=/cache") {
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
