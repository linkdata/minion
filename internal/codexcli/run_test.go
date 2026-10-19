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

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunInvokesCodexAndReturnsFinalMessage(t *testing.T) {
	for _, tt := range []struct {
		name     string
		writable bool
		access   string
	}{
		{name: "review", access: "read"},
		{name: "fix", writable: true, access: "write"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := canonicalTempDir(t)
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
			tmp, err := os.ReadFile(tmpFile)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := strings.TrimSpace(string(tmp))
			got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
			want := []string{
				"exec", "--json", "--ignore-user-config", "--ignore-rules", "--strict-config", "--skip-git-repo-check",
				"--disable", "apps", "--disable", "browser_use", "--disable", "browser_use_external",
				"--disable", "browser_use_full_cdp_access", "--disable", "computer_use",
				"--disable", "image_generation", "--disable", "plugins", "--disable", "remote_plugin",
				"--disable", "shell_snapshot", "--disable", "skill_mcp_dependency_install", "--disable", "memories", "--disable", "hooks", "--disable", "skill_search",
				"-c", "project_doc_max_bytes=0",
				"-c", `project_root_markers=[".minion-codex-root"]`,
				"-c", "skills.include_instructions=false",
				"-c", "approval_policy=never",
				"-c", `web_search="disabled"`,
				"-c", "shell_environment_policy.ignore_default_excludes=false",
				"-c", `shell_environment_policy.filters={"PATH"="include","HOME"="include","TMPDIR"="include","TMP"="include","TEMP"="include","GOCACHE"="include","GIT_CONFIG_NOSYSTEM"="include","GIT_CONFIG_GLOBAL"="include","GIT_ATTR_NOSYSTEM"="include","GIT_NO_REPLACE_OBJECTS"="include","GIT_NO_LAZY_FETCH"="include","GIT_OPTIONAL_LOCKS"="include","LANG"="include","LC_ALL"="include","LC_CTYPE"="include","TERM"="include"}`,
				"-c", `default_permissions="minion"`,
				"-c", fmt.Sprintf(`permissions.minion.filesystem={":root"="deny",":minimal"="read",":tmpdir"="write",%s=%q,%s="read",%s="read",%s="read",%s="read",%s="read",%s="read",%s="read",%s="read"}`, strconv.Quote(dir), tt.access, strconv.Quote(filepath.Join(dir, ".git")), strconv.Quote(filepath.Join(dir, ".agents")), strconv.Quote(filepath.Join(dir, ".codex")), strconv.Quote(filepath.Join(dir, ".claude")), strconv.Quote(filepath.Join(runtimeDir, ".git")), strconv.Quote(filepath.Join(runtimeDir, ".agents")), strconv.Quote(filepath.Join(runtimeDir, ".codex")), strconv.Quote(fakeCodex)),
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
			if !strings.HasPrefix(string(gotPrompt), "Source snapshot: ") || !strings.Contains(string(gotPrompt), strconv.Quote(dir)) || !strings.HasSuffix(string(gotPrompt), "Handle mention") {
				t.Fatalf("stdin prompt lacks source location or original request: %q", gotPrompt)
			}
			if strings.TrimSpace(string(cwd)) != runtimeDir {
				t.Fatalf("working directory = %q, want %q", strings.TrimSpace(string(cwd)), runtimeDir)
			}
			if marker, err := os.ReadFile(fakeCodex + ".marker"); err != nil || string(marker) != "present\n" {
				t.Fatalf("isolated project root marker missing during run: %q, %v", marker, err)
			}
			if filepath.Dir(runtimeDir) != filepath.Dir(dir) {
				t.Fatalf("runtime directory %q is not a source snapshot sibling", runtimeDir)
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
	skillPath := filepath.Join(canonicalTempDir(t), "SKILL.md")
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
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"answer\":\"public\",\"refutations\":\"private\"}"}}' '{"type":"turn.completed"}'
`)
	const schema = `{"type":"object","properties":{"answer":{"type":"string"},"refutations":{"type":"string"}},"required":["answer","refutations"],"additionalProperties":false}`
	result, err := Run(context.Background(), t.TempDir(), "Review candidate", true, "", config.CodexConfig{OutputSchema: schema})
	if err != nil || result.Answer != `{"answer":"public","refutations":"private"}` {
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
	dir := canonicalTempDir(t)
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
	workspace := canonicalTempDir(t)
	output := canonicalTempDir(t)
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
		want := "separate from the source snapshot"
		if unsafe == "/" {
			want = "expected a non-root directory"
		}
		if _, err := Run(context.Background(), workspace, "Audit source", false, unsafe, config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("accepted unsafe reports directory %q: %v", unsafe, err)
		}
	}
}

func TestRunAllowsCriticToReadReportsWithoutWriting(t *testing.T) {
	root := canonicalTempDir(t)
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
	for _, test := range []struct{ dir, failure string }{
		{root, "separate from the source snapshot"},
		{workspace, "separate from the source snapshot"},
		{string(os.PathSeparator), "expected a non-root directory"},
		{filepath.Join(root, "missing"), ""},
	} {
		_, err := Run(context.Background(), workspace, "Critique reports", false, "", config.CodexConfig{ReadOnlyDirs: []string{test.dir}})
		if err == nil || test.failure != "" && !strings.Contains(err.Error(), test.failure) || test.failure == "" && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe or missing report directory %s: %v; want %q", test.dir, err, test.failure)
		}
	}
}

func TestRunRejectsSkillInsideSource(t *testing.T) {
	source := t.TempDir()
	skill := filepath.Join(source, "SKILL.md")
	if err := os.WriteFile(skill, []byte("untrusted source"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.Symlink(skill, alias); err != nil {
		t.Fatal(err)
	}
	binary := installFakeCodex(t, "#!/bin/sh\ntouch \"$0.invoked\"\nexit 99\n")
	for _, path := range []string{skill, alias} {
		if _, err := Run(context.Background(), source, "task", false, "", config.CodexConfig{Skills: []string{path}}); err == nil || !strings.Contains(err.Error(), "skill is inside the untrusted source snapshot") {
			t.Fatalf("accepted source skill %q: %v", path, err)
		}
	}
	if _, err := os.Stat(binary + ".invoked"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid skill started Codex: %v", err)
	}
}

func TestRunRejectsReadOnlyOutputOverlap(t *testing.T) {
	source, output := t.TempDir(), filepath.Join(t.TempDir(), "reports")
	child := filepath.Join(output, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "output")
	if err := os.Symlink(output, alias); err != nil {
		t.Fatal(err)
	}
	binary := installFakeCodex(t, "#!/bin/sh\ntouch \"$0.invoked\"\nexit 99\n")
	for _, path := range []string{output, child, filepath.Dir(output), alias} {
		if _, err := Run(context.Background(), source, "task", false, output, config.CodexConfig{ReadOnlyDirs: []string{path}}); err == nil || !strings.Contains(err.Error(), "read-only directory overlaps the output directory") {
			t.Fatalf("accepted read-only/output overlap %q: %v", path, err)
		}
	}
	if _, err := os.Stat(binary + ".invoked"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid grants started Codex: %v", err)
	}
}

func TestChildEnvironmentIsolatesCodexSessionButKeepsAuthentication(t *testing.T) {
	env := childEnvironment([]string{
		"CODEX_PERMISSION_PROFILE=unsafe-parent-profile", "CODEX_THREAD_ID=parent", "CODEX_SESSION_ID=parent",
		"CODEX_APP_TOOLS_PIPE_PATH=parent-app", "CODEX_SANDBOX_NETWORK_DISABLED=1",
		"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "CODEX_ACCESS_TOKEN=test-access-token",
		"GH_TOKEN=secret", "GITHUB_TOKEN=secret", "OPENAI_API_KEY=secret", "PGPASSWORD=secret",
		"PATH=/usr/bin", "HOME=/trusted/home", "HTTPS_PROXY=http://proxy", "SSL_CERT_FILE=/cert",
		"TMPDIR=/host-tmp", "GOCACHE=/host-cache", "GIT_CONFIG_GLOBAL=/private-git-config", "GIT_DIR=/private-git-dir",
	}, "/runtime", "/cache", "/trusted/codex", false)
	joined := strings.Join(env, "\n")
	for _, denied := range []string{"unsafe-parent-profile", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_SANDBOX_NETWORK_DISABLED", "GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "PGPASSWORD", "/host-tmp", "/host-cache", "/private-git-"} {
		if strings.Contains(joined, denied) {
			t.Fatalf("inherited parent session state: %s", joined)
		}
	}
	for _, kept := range []string{"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "CODEX_ACCESS_TOKEN=test-access-token", "PATH=/usr/bin", "HOME=/trusted/home", "HTTPS_PROXY=http://proxy", "SSL_CERT_FILE=/cert", "TMPDIR=/runtime", "GOCACHE=/cache", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0"} {
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

func TestRunRedactsEncodedKeyBeforeQuotingFailure(t *testing.T) {
	key := `fixture"secret`
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' "+aicli.ShellQuote(string(encoded[1:len(encoded)-1]))+" >&2\nexit 1\n")
	_, err = Run(context.Background(), t.TempDir(), "Review this checkout", false, "", config.CodexConfig{APIKey: key})
	if err == nil || strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("encoded configured key escaped in quoted error: %v", err)
	}
}

func TestRunDoesNotExposeKeyFragmentsAtDiagnosticLimit(t *testing.T) {
	key := "KEYPREFIX__LEAKSUFFIX"
	stderr := key + strings.Repeat("x", bounded.DiagnosticBytes-len("LEAKSUFFIX")-len("rate limit reached")) + "rate limit reached"
	eventMessage := strings.Repeat("x", bounded.DiagnosticBytes-len(key)+2) + key + strings.Repeat("y", 300)
	event, err := json.Marshal(map[string]any{"type": "turn.failed", "error": map[string]string{"message": eventMessage}})
	if err != nil {
		t.Fatal(err)
	}
	incompleteEvent, err := json.Marshal(map[string]any{"type": "error", "message": eventMessage})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, script, forbidden, required string
		bounded                           bool
	}{
		{"stderr tail", "printf '%s' " + aicli.ShellQuote(stderr) + " >&2\nexit 1\n", "LEAKSUFFIX", "rate limit reached", false},
		{"event prefix", "printf '%s\\n' " + aicli.ShellQuote(string(event)) + "\n", "KEYPREFIX__LEAK", "codex exec failed", true},
		{"incomplete event", "printf '%s\\n' " + aicli.ShellQuote(string(incompleteEvent)) + "\n", "KEYPREFIX__LEAK", "did not complete", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\n"+tc.script)
			_, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: key})
			if err == nil || strings.Contains(err.Error(), tc.forbidden) || !strings.Contains(err.Error(), tc.required) || tc.bounded && (!strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "y...") || len(err.Error()) > bounded.DiagnosticBytes+200) {
				t.Fatalf("key fragment escaped or limit diagnostic lost in %s: %v", tc.name, err)
			}
		})
	}
}

func TestRunRedactsEncodedKeyFromFailedEvent(t *testing.T) {
	key := `fixture"secret`
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal(map[string]any{"type": "turn.failed", "error": map[string]string{"message": string(encoded[1 : len(encoded)-1])}})
	if err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' "+aicli.ShellQuote(string(event))+"\n")
	_, err = Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: key})
	if err == nil || strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("encoded configured key escaped failed event: %v", err)
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
	output.stream.Finish()
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
		broken.stream.Finish()
		if broken.tokens != nil {
			t.Fatalf("invalid usage accepted: %s", invalid)
		}
	}
}

func TestJSONLOutputBindsRootThreadIdentity(t *testing.T) {
	for _, next := range []string{"root", "", "another-root"} {
		t.Run(next, func(t *testing.T) {
			canceled := false
			output := jsonlOutput{cancel: func() { canceled = true }}
			_, _ = fmt.Fprintf(&output, "{\"type\":\"thread.started\",\"thread_id\":\"root\"}\n{\"type\":\"thread.started\",\"thread_id\":%q}\n", next)
			_, _ = output.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}` + "\n" + `{"type":"turn.completed"}` + "\n"))
			output.stream.Finish()
			conflict := next == "another-root"
			if output.threadID != "root" || (output.stopErr != nil) != conflict || canceled != conflict || output.completed == conflict {
				t.Fatalf("root identity changed or conflict completed: root=%q error=%v canceled=%t completed=%t", output.threadID, output.stopErr, canceled, output.completed)
			}
		})
	}
}

func TestJSONLOutputStopsAtChildLimit(t *testing.T) {
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
	if !cancelled || !output.failed || len(output.children) != maxCodexChildren || !errors.Is(output.stopErr, aicli.ErrOutputLimit) || !strings.Contains(output.stopErr.Error(), "child agent") {
		t.Fatalf("unbounded children: cancelled=%t children=%d stop=%v", cancelled, len(output.children), output.stopErr)
	}
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

func TestRunRejectsExecutableFromSourceSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := Run(context.Background(), dir, "Review", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "executable is inside the untrusted source snapshot") {
		t.Fatalf("Run() error = %v, want source snapshot executable rejection", err)
	}
}

func installFakeCodex(t *testing.T, script string) string {
	t.Helper()
	t.Setenv("CODEX_HOME", canonicalTempDir(t))
	path, err := filepath.EvalSymlinks(filepath.Join(testsupport.FakeOnPath(t, "codex", script), "codex"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
