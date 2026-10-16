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
	"github.com/linkdata/minion/internal/skills"
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
printf '%s\n' "$@" > "$0.args"
pwd > "$0.cwd"
printf '%s,%s\n' "${GH_TOKEN-absent}" "${GITHUB_TOKEN-absent}" > "$0.env"
printf '%s\n' "$TMPDIR" > "$0.tmpdir"
printf '%s\n' "$GOCACHE" > "$0.gocache"
test -f "$PWD/.minion-codex-root" && printf 'present\n' > "$0.marker"
cat > "$0.prompt"
printf '%s\n' '{"type":"thread.started","thread_id":"test"}' '{"type":"item.completed","item":{"type":"agent_message","text":"first"}}' '{"type":"item.completed","item":{"type":"agent_message","text":"final answer"}}' '{"type":"turn.completed"}'
`)
			argsFile := fakeCodex + ".args"
			cwdFile := fakeCodex + ".cwd"
			envFile := fakeCodex + ".env"
			tmpFile := fakeCodex + ".tmpdir"
			cacheFile := fakeCodex + ".gocache"
			promptFile := fakeCodex + ".prompt"
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
				"exec", "--json", "--ignore-user-config", "--ignore-rules", "--strict-config", "--skip-git-repo-check",
				"--disable", "apps", "--disable", "browser_use", "--disable", "browser_use_external",
				"--disable", "browser_use_full_cdp_access", "--disable", "computer_use",
				"--disable", "image_generation", "--disable", "plugins", "--disable", "remote_plugin",
				"--disable", "shell_snapshot", "--disable", "skill_mcp_dependency_install",
				"-c", "project_doc_max_bytes=0",
				"-c", `project_root_markers=[".minion-codex-root"]`,
				"-c", "skills.include_instructions=false",
				"-c", "approval_policy=never",
				"-c", `web_search="disabled"`,
				"-c", "shell_environment_policy.ignore_default_excludes=false",
				"-c", `shell_environment_policy.filters={"PATH"="include","HOME"="include","TMPDIR"="include","TMP"="include","TEMP"="include","GOCACHE"="include","LANG"="include","LC_ALL"="include","LC_CTYPE"="include","TERM"="include"}`,
				"-c", `default_permissions="minion"`,
				"-c", fmt.Sprintf("permissions.minion.extends=%q", tt.parent),
				"-c", fmt.Sprintf(`permissions.minion.filesystem={":root"="deny",":minimal"="read",":tmpdir"="write",":slash_tmp"="deny",%s=%q,%s="read",%s="read",%s="read",%s="read",%s="read"}`, strconv.Quote(dir), tt.access, strconv.Quote(filepath.Join(dir, ".git")), strconv.Quote(filepath.Join(dir, ".agents")), strconv.Quote(filepath.Join(dir, ".codex")), strconv.Quote(filepath.Join(dir, ".claude")), strconv.Quote(fakeCodex)),
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
			if !strings.Contains(string(gotPrompt), strconv.Quote(dir)) || !strings.HasSuffix(string(gotPrompt), "Handle mention") {
				t.Fatalf("stdin prompt lacks checkout location or original request: %q", gotPrompt)
			}
			tmp, err := os.ReadFile(tmpFile)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := strings.TrimSpace(string(tmp))
			if strings.TrimSpace(string(cwd)) != runtimeDir {
				t.Fatalf("working directory = %q, want %q", strings.TrimSpace(string(cwd)), runtimeDir)
			}
			if marker, err := os.ReadFile(fakeCodex + ".marker"); err != nil || string(marker) != "present\n" {
				t.Fatalf("isolated project root marker missing during run: %q, %v", marker, err)
			}
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

func TestRunRemovesRestrictedRuntimeDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	installFakeCodex(t, `#!/bin/sh
mkdir "$TMPDIR/locked-000" "$TMPDIR/locked-500"
touch "$TMPDIR/locked-000/file" "$TMPDIR/locked-500/file"
chmod 000 "$TMPDIR/locked-000"
chmod 500 "$TMPDIR/locked-500"
ln -s ../outside "$TMPDIR/outside-link"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	t.Cleanup(func() {
		leftovers, _ := filepath.Glob(filepath.Join(root, ".minion-codex-*"))
		for _, path := range leftovers {
			_ = os.Chmod(filepath.Join(path, "locked-000"), 0o700)
			_ = os.Chmod(filepath.Join(path, "locked-500"), 0o700)
			_ = os.RemoveAll(path)
		}
	})
	result, err := Run(context.Background(), workspace, "Review", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "done" {
		t.Fatalf("Run() = %+v, %v; want the answer and successful cleanup", result, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(root, ".minion-codex-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("runtime directories remain: %v, %v", leftovers, err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("outside symlink target changed: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("outside symlink target was removed: %v", err)
	}
}

func TestRunPassesConfiguredModelEffortAndSkills(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: test\n---\nUse this skill."), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeCodex := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$0.args"
cat > "$0.prompt"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	argsFile := fakeCodex + ".args"
	promptFile := fakeCodex + ".prompt"
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

func TestRunUsesOutputSchema(t *testing.T) {
	fakeCodex := installFakeCodex(t, `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  if [ "$1" = '--output-schema' ]; then
    printf '%s\n' "$2" > "$0.schema-path"
    cp "$2" "$0.schema"
    break
  fi
  shift
done
cat > /dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"answer\":\"public\",\"review_notes\":\"private\"}"}}' '{"type":"turn.completed"}'
`)
	const schema = `{"type":"object","properties":{"answer":{"type":"string"},"review_notes":{"type":"string"}},"required":["answer","review_notes"],"additionalProperties":false}`
	result, err := Run(context.Background(), t.TempDir(), "Review candidate", true, "", config.CodexConfig{OutputSchema: schema})
	if err != nil || result.Answer != `{"answer":"public","review_notes":"private"}` {
		t.Fatalf("structured result = %q, %v", result.Answer, err)
	}
	content, err := os.ReadFile(fakeCodex + ".schema")
	if err != nil || string(content) != schema {
		t.Fatalf("schema passed to Codex = %q, %v", content, err)
	}
	info, err := os.Stat(fakeCodex + ".schema")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("copied schema permissions = %v, %v", info, err)
	}
	pathData, err := os.ReadFile(fakeCodex + ".schema-path")
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(string(pathData))
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".minion-codex-") {
		t.Fatalf("schema is not in the isolated runtime: %q", path)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("schema file retained after run: %v", err)
	}
}

func TestRunUsesEmbeddedSkillWithoutInstallation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	fakeCodex := installFakeCodex(t, `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$0.args"
printf '%s\n' "$TMPDIR" > "$0.tmpdir"
cat > "$0.prompt"
for skill in ../.minion-skills-*/anti-prompt-injection/SKILL.md; do
  printf '%s\n' "$skill" > "$0.skillpath"
  cat "$skill" > "$0.skill-copy"
done
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	settings := config.CodexConfig{Skills: []string{skills.BuiltinAntiPromptInjection}}
	if _, err := Run(context.Background(), dir, "Handle mention", false, "", settings); err != nil {
		t.Fatal(err)
	}
	runtimeData, err := os.ReadFile(fakeCodex + ".tmpdir")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := strings.TrimSpace(string(runtimeData))
	skillData, err := os.ReadFile(fakeCodex + ".skillpath")
	if err != nil {
		t.Fatal(err)
	}
	skillPath, err := filepath.Abs(filepath.Join(dir, strings.TrimSpace(string(skillData))))
	if err != nil {
		t.Fatal(err)
	}
	skillRoot := filepath.Dir(filepath.Dir(skillPath))
	if filepath.Dir(skillRoot) != filepath.Dir(runtimeDir) || skillRoot == runtimeDir {
		t.Fatalf("embedded skill root %q is not outside writable TMPDIR %q", skillRoot, runtimeDir)
	}
	args, err := os.ReadFile(fakeCodex + ".args")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), strconv.Quote(filepath.Dir(skillPath))+`="read"`) {
		t.Fatalf("embedded skill read permission missing: %s", args)
	}
	prompt, err := os.ReadFile(fakeCodex + ".prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), strconv.Quote(skillPath)) || strings.Contains(string(prompt), skills.BuiltinAntiPromptInjection) {
		t.Fatalf("embedded skill not materialized into prompt: %s", prompt)
	}
	want, _ := skills.BuiltinContent(skills.BuiltinAntiPromptInjection)
	got, err := os.ReadFile(fakeCodex + ".skill-copy")
	if err != nil || string(got) != string(want) {
		t.Fatalf("embedded skill seen by Codex = %q, %v", got, err)
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("embedded skill runtime directory remains: %v", err)
	}
	if _, err := os.Stat(skillRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("embedded skill root remains: %v", err)
	}
}

func TestRunAllowsSeparateAuditReportsWithoutSourceWrites(t *testing.T) {
	workspace := t.TempDir()
	output := t.TempDir()
	fakeCodex := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$0.args"
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"audit done"}}' '{"type":"turn.completed"}'
`)
	argsFile := fakeCodex + ".args"
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
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{workspace, inside, filepath.Dir(workspace), "/"} {
		if _, err := Run(context.Background(), workspace, "Audit source", false, unsafe, config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "separate from the checkout") {
			t.Fatalf("accepted unsafe reports directory %q: %v", unsafe, err)
		}
	}
}

func TestRunAllowsCriticToReadReportsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	workspace, reports := filepath.Join(root, "repo"), filepath.Join(root, "reports")
	for _, dir := range []string{workspace, reports} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	binary := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$0.args"
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"approved"}}' '{"type":"turn.completed"}'
`)
	if _, err := Run(context.Background(), workspace, "Critique reports", false, "", config.CodexConfig{ReadOnlyDirs: []string{reports}}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(binary + ".args")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{workspace, reports} {
		if !strings.Contains(string(args), strconv.Quote(dir)+`="read"`) || strings.Contains(string(args), strconv.Quote(dir)+`="write"`) {
			t.Fatalf("critic lacks read-only access to %s: %s", dir, args)
		}
	}
	for _, dir := range []string{root, workspace, string(os.PathSeparator), filepath.Join(root, "missing")} {
		if _, err := Run(context.Background(), workspace, "Critique reports", false, "", config.CodexConfig{ReadOnlyDirs: []string{dir}}); err == nil {
			t.Fatalf("accepted unsafe or missing report directory %s", dir)
		}
	}
}

func TestChildEnvironmentIsolatesCodexSessionButKeepsAuthentication(t *testing.T) {
	env := childEnvironment([]string{
		"CODEX_PERMISSION_PROFILE=unsafe-parent-profile", "CODEX_THREAD_ID=parent", "CODEX_SESSION_ID=parent",
		"CODEX_APP_TOOLS_PIPE_PATH=parent-app", "CODEX_SANDBOX_NETWORK_DISABLED=1",
		"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "CODEX_ACCESS_TOKEN=test-access-token",
		"GH_TOKEN=secret", "GITHUB_TOKEN=secret", "OPENAI_API_KEY=secret", "PGPASSWORD=secret",
		"PATH=/usr/bin", "HOME=/trusted/home", "HTTPS_PROXY=http://proxy", "SSL_CERT_FILE=/cert",
		"TMPDIR=/host-tmp", "GOCACHE=/host-cache",
	}, "/runtime", "/cache", false)
	joined := strings.Join(env, "\n")
	for _, denied := range []string{"unsafe-parent-profile", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_SANDBOX_NETWORK_DISABLED", "GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "PGPASSWORD", "/host-tmp", "/host-cache"} {
		if strings.Contains(joined, denied) {
			t.Fatalf("inherited parent session state: %s", joined)
		}
	}
	for _, kept := range []string{"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "CODEX_ACCESS_TOKEN=test-access-token", "PATH=/usr/bin", "HOME=/trusted/home", "HTTPS_PROXY=http://proxy", "SSL_CERT_FILE=/cert", "TMPDIR=/runtime", "GOCACHE=/cache"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("required environment setting missing: %s", kept)
		}
	}
}

func TestRunConfiguredAPIKeyOverridesEnvironment(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			fake := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$0.args"
printf '%s,%s,%s\n' "${CODEX_API_KEY-absent}" "${CODEX_ACCESS_TOKEN-absent}" "${OPENAI_API_KEY-absent}" > "$0.auth"
cat > "$0.prompt"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
			t.Setenv("CODEX_API_KEY", "dummy-environment-key")
			t.Setenv("CODEX_ACCESS_TOKEN", "dummy-environment-access-token")
			t.Setenv("OPENAI_API_KEY", "dummy-unrelated-key")
			settings := config.CodexConfig{}
			want := "dummy-environment-key,dummy-environment-access-token,absent\n"
			if configured {
				settings.APIKey = "dummy-configured-key"
				want = "dummy-configured-key,absent,absent\n"
			}
			if _, err := Run(context.Background(), t.TempDir(), "Review this checkout", false, "", settings); err != nil {
				t.Fatal(err)
			}
			auth, err := os.ReadFile(fake + ".auth")
			if err != nil || string(auth) != want {
				t.Fatalf("child authentication = %q, %v; want %q", auth, err, want)
			}
			for _, suffix := range []string{".args", ".prompt"} {
				data, err := os.ReadFile(fake + suffix)
				if err != nil || strings.Contains(string(data), "dummy-configured-key") {
					t.Fatalf("configured API key reached %s: %v", suffix, err)
				}
			}
		})
	}
}

func TestRunRedactsConfiguredAPIKey(t *testing.T) {
	for _, test := range []struct {
		name, output string
		wantError    bool
	}{
		{name: "answer", output: `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"dummy-configured-key"}}' '{"type":"turn.completed"}'`},
		{name: "event error", output: `printf '%s\n' '{"type":"turn.failed","error":{"message":"dummy-configured-key"}}'`, wantError: true},
		{name: "stderr", output: `printf '%s\n' 'dummy-configured-key' >&2; exit 1`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\n"+test.output+"\n")
			result, err := Run(context.Background(), t.TempDir(), "Review this checkout", false, "", config.CodexConfig{APIKey: "dummy-configured-key"})
			if (err != nil) != test.wantError {
				t.Fatalf("unexpected Run error: %v", err)
			}
			if strings.Contains(result.Answer, "dummy-configured-key") || err != nil && strings.Contains(err.Error(), "dummy-configured-key") {
				t.Fatal("configured API key escaped redaction")
			}
			if !test.wantError && result.Answer != "[redacted]" {
				t.Fatalf("answer = %q", result.Answer)
			}
		})
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
