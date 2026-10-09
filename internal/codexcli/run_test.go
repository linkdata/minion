package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunInvokesCodexAndReturnsFinalMessage(t *testing.T) {
	for _, tt := range []struct {
		name     string
		writable bool
		parent   string
		access   string
	}{
		{name: "review", parent: ":read-only", access: "read"},
		{name: "fix", writable: true, parent: ":workspace", access: "write"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeCodex := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$CAPTURE_ARGS"
pwd > "$CAPTURE_DIR"
printf '%s,%s\n' "${GH_TOKEN-absent}" "${GITHUB_TOKEN-absent}" > "$CAPTURE_ENV"
printf '%s\n' "$TMPDIR" > "$CAPTURE_TMPDIR"
printf '%s\n' "$GOCACHE" > "$CAPTURE_GOCACHE"
cat > "$CAPTURE_PROMPT"
printf '%s\n' '{"type":"thread.started","thread_id":"test"}' '{"type":"item.completed","item":{"type":"agent_message","text":"first"}}' '{"type":"item.completed","item":{"type":"agent_message","text":"final answer"}}' '{"type":"turn.completed"}'
`)
			argsFile := filepath.Join(dir, "args")
			cwdFile := filepath.Join(dir, "cwd")
			envFile := filepath.Join(dir, "env")
			tmpFile := filepath.Join(dir, "tmpdir")
			cacheFile := filepath.Join(dir, "gocache")
			promptFile := filepath.Join(dir, "prompt")
			t.Setenv("CAPTURE_ARGS", argsFile)
			t.Setenv("CAPTURE_DIR", cwdFile)
			t.Setenv("CAPTURE_ENV", envFile)
			t.Setenv("CAPTURE_TMPDIR", tmpFile)
			t.Setenv("CAPTURE_GOCACHE", cacheFile)
			t.Setenv("CAPTURE_PROMPT", promptFile)
			t.Setenv("GH_TOKEN", "secret")
			t.Setenv("GITHUB_TOKEN", "secret")

			answer, err := Run(context.Background(), dir, "Handle mention", tt.writable, "", config.CodexConfig{})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if answer.Answer != "final answer" {
				t.Fatalf("Run() answer = %q", answer.Answer)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
			want := []string{
				"exec", "--json", "--ignore-user-config", "--strict-config",
				"-c", "approval_policy=never",
				"-c", `web_search="disabled"`,
				"-c", "shell_environment_policy.ignore_default_excludes=false",
				"-c", `shell_environment_policy.filters={"PATH"="include","HOME"="include","TMPDIR"="include","TMP"="include","TEMP"="include","GOCACHE"="include","LANG"="include","LC_ALL"="include","LC_CTYPE"="include","TERM"="include"}`,
				"-c", `default_permissions="minion"`,
				"-c", fmt.Sprintf("permissions.minion.extends=%q", tt.parent),
				"-c", fmt.Sprintf(`permissions.minion.filesystem={":root"="deny",":minimal"="read",":tmpdir"="write",":slash_tmp"="deny",%s=%q,%s="read"}`, strconv.Quote(dir), tt.access, strconv.Quote(fakeCodex)),
				"-c", "permissions.minion.network.enabled=false",
				"--", "-",
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("arguments = %q, want %q", got, want)
			}
			cwd, err := os.ReadFile(cwdFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(cwd)) != dir {
				t.Fatalf("working directory = %q, want %q", strings.TrimSpace(string(cwd)), dir)
			}
			creds, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(creds)) != "absent,absent" {
				t.Fatalf("GitHub credentials reached Codex: %q", strings.TrimSpace(string(creds)))
			}
			gotPrompt, err := os.ReadFile(promptFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotPrompt) != "Handle mention" {
				t.Fatalf("stdin prompt = %q", gotPrompt)
			}
			tmp, err := os.ReadFile(tmpFile)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := strings.TrimSpace(string(tmp))
			if filepath.Dir(runtimeDir) != filepath.Dir(dir) {
				t.Fatalf("runtime directory %q is not a checkout sibling", runtimeDir)
			}
			if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime directory not removed: %v", err)
			}
			cache, err := os.ReadFile(cacheFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(cache)) != filepath.Join(runtimeDir, "go-build") {
				t.Fatalf("GOCACHE = %q", strings.TrimSpace(string(cache)))
			}
		})
	}
}

func TestRunPassesConfiguredModelEffortAndSkills(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: test\n---\nUse this skill."), 0600); err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$CAPTURE_ARGS"
cat > "$CAPTURE_PROMPT"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	argsFile := filepath.Join(dir, "args")
	promptFile := filepath.Join(dir, "prompt")
	t.Setenv("CAPTURE_ARGS", argsFile)
	t.Setenv("CAPTURE_PROMPT", promptFile)
	settings := config.CodexConfig{Model: "gpt-6-sol", ReasoningEffort: "ultra", Skills: []string{skillPath}}
	if _, err := Run(context.Background(), dir, "Handle mention", false, "", settings); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--model\ngpt-6-sol\n", "-c\nmodel_reasoning_effort=\"ultra\"\n",
		strconv.Quote(filepath.Dir(skillPath)) + `="read"`,
	} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("configured Codex argument %q missing from %s", want, args)
		}
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), strconv.Quote(skillPath)) || !strings.HasSuffix(string(prompt), "Handle mention") {
		t.Fatalf("configured skill or original prompt missing: %s", prompt)
	}
}

func TestRunAllowsSeparateAuditReportsWithoutSourceWrites(t *testing.T) {
	workspace := t.TempDir()
	output := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("CAPTURE_ARGS", argsFile)
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$CAPTURE_ARGS"
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"audit done"}}' '{"type":"turn.completed"}'
`)
	if _, err := Run(context.Background(), workspace, "Audit source", false, output, config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{strconv.Quote(workspace) + `="read"`, strconv.Quote(output) + `="write"`, "permissions.minion.network.enabled=false"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("audit permission %q missing from %s", want, args)
		}
	}
	if strings.Contains(string(args), "--ephemeral") {
		t.Fatal("audit helpers cannot fork an ephemeral parent session")
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("removed persistent reports directory: %v", err)
	}
	inside := filepath.Join(workspace, "reports")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{workspace, inside, filepath.Dir(workspace), "/"} {
		if _, err := Run(context.Background(), workspace, "Audit source", false, unsafe, config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "separate from the checkout") {
			t.Fatalf("accepted unsafe reports directory %q: %v", unsafe, err)
		}
	}
}

func TestChildEnvironmentIsolatesCodexSessionButKeepsAuthentication(t *testing.T) {
	env := childEnvironment([]string{
		"CODEX_PERMISSION_PROFILE=unsafe-parent-profile", "CODEX_THREAD_ID=parent", "CODEX_SESSION_ID=parent",
		"CODEX_APP_TOOLS_PIPE_PATH=parent-app", "CODEX_SANDBOX_NETWORK_DISABLED=1",
		"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "GH_TOKEN=secret", "PATH=/usr/bin",
	}, "/runtime", "/cache")
	joined := strings.Join(env, "\n")
	for _, denied := range []string{"unsafe-parent-profile", "parent", "GH_TOKEN", "CODEX_SANDBOX_NETWORK_DISABLED"} {
		if strings.Contains(joined, denied) {
			t.Fatalf("inherited parent session state: %s", joined)
		}
	}
	for _, kept := range []string{"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "PATH=/usr/bin"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("required environment setting missing: %s", kept)
		}
	}
}

func TestRunReportsCodexFailure(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' '{"type":"turn.failed","error":{"message":"model request failed"}}'
printf '%s\n' 'connection lost' >&2
exit 7
`)
	_, err := Run(context.Background(), t.TempDir(), "Review", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "model request failed") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("Run() error = %v, want Codex failure and exit code", err)
	}
}

func TestJSONLOutputCapturesUsageAndHelpers(t *testing.T) {
	var output jsonlOutput
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}
{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":70,"output_tokens":12}}
`))
	output.finish()
	if output.threadID != "root" || len(output.children) != 1 || output.children[0] != "child" || output.tokens == nil || *output.tokens != (usage.Tokens{Input: 100, CachedInput: 70, Output: 12}) {
		t.Fatalf("missing accounting events: %+v", output)
	}
	for _, invalid := range []string{
		`{"input_tokens":100,"output_tokens":12}`,
		`{"input_tokens":100,"cached_input_tokens":101,"output_tokens":12}`,
		`{"input_tokens":-1,"cached_input_tokens":0,"output_tokens":12}`,
	} {
		var broken jsonlOutput
		_, _ = broken.Write([]byte(`{"type":"turn.completed","usage":` + invalid + `}`))
		broken.finish()
		if broken.tokens != nil {
			t.Fatalf("invalid usage accepted: %s", invalid)
		}
	}
}

func TestJSONLOutputStopsAtCardinalityLimits(t *testing.T) {
	t.Run("events", func(t *testing.T) {
		cancelled := false
		output := jsonlOutput{cancel: func() { cancelled = true }}
		_, _ = output.Write([]byte(strings.Repeat("{\"type\":\"noop\"}\n", maxCodexEvents+1)))
		if !cancelled || !output.failed || output.events != maxCodexEvents+1 || !strings.Contains(output.failureReason, "event count") {
			t.Fatalf("unbounded events: cancelled=%t events=%d reason=%q", cancelled, output.events, output.failureReason)
		}
	})
	t.Run("children", func(t *testing.T) {
		cancelled := false
		output := jsonlOutput{cancel: func() { cancelled = true }}
		ids := make([]string, maxCodexChildren+1)
		for i := range ids {
			ids[i] = strconv.Itoa(i)
		}
		line, err := json.Marshal(map[string]any{"item": map[string]any{"type": "collab_tool_call", "receiver_thread_ids": ids}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = output.Write(append(line, '\n'))
		if !cancelled || !output.failed || len(output.children) != 0 || !strings.Contains(output.failureReason, "child agent") {
			t.Fatalf("unbounded children: cancelled=%t children=%d reason=%q", cancelled, len(output.children), output.failureReason)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		cancelled := false
		output := jsonlOutput{cancel: func() { cancelled = true }, bytes: maxCodexOutputBytes - 1}
		_, _ = output.Write([]byte("xx"))
		if !cancelled || !output.failed || !strings.Contains(output.failureReason, "size limit") {
			t.Fatalf("unbounded output bytes: cancelled=%t reason=%q", cancelled, output.failureReason)
		}
	})
}

func TestRunRetainsFallbackUsageOnFailure(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' '{"type":"thread.started","thread_id":"missing-rollout"}' '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":70,"output_tokens":12}}'
exit 7
`)
	result, err := Run(context.Background(), t.TempDir(), "Review", false, "", config.CodexConfig{})
	if err == nil || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 100, CachedInput: 70, Output: 12}) {
		t.Fatalf("lost partial usage: result=%+v, error=%v", result, err)
	}
}

func TestRunHonorsCancellation(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\nexec sleep 10\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, t.TempDir(), "Review", false, "", config.CodexConfig{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Run() took too long after cancellation: %s", time.Since(start))
	}
}

func TestRunRejectsExecutableFromCheckout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := Run(context.Background(), dir, "Review", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "executable is inside the repository checkout") {
		t.Fatalf("Run() error = %v, want checkout executable rejection", err)
	}
}

func installFakeCodex(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}
