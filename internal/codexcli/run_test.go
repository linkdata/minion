package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestSandboxArgsSelectsNativeMode(t *testing.T) {
	for _, test := range []struct {
		name, output string
		writable     bool
		want         []string
	}{
		{"review", "", false, []string{"-c", `sandbox_mode="read-only"`}},
		{"fix", "", true, []string{"-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.exclude_slash_tmp=true", "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", `sandbox_workspace_write.writable_roots=["/source"]`}},
		{"report", "/report", false, []string{"-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.exclude_slash_tmp=true", "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", `sandbox_workspace_write.writable_roots=["/report"]`}},
		{"fix and report", "/report", true, []string{"-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.exclude_slash_tmp=true", "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", `sandbox_workspace_write.writable_roots=["/source","/report"]`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sandboxArgs("/source", test.output, test.writable); !slices.Equal(got, test.want) {
				t.Fatalf("sandbox args = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRunInvokesCodexAndReturnsFinalMessage(t *testing.T) {
	if !slices.Contains(aicli.ModelGitEnvironment(), "GIT_CONFIG_KEY_0=safe.bareRepository") {
		t.Fatal("Codex shell Git key diverged from the shared model Git environment")
	}
	for _, tt := range []struct {
		name     string
		writable bool
	}{
		{name: "review"},
		{name: "fix", writable: true},
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

			answer, err := Run(t.Context(), dir, "Handle mention", tt.writable, "", config.CodexConfig{})
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
			}
			if tt.writable {
				want = append(want, "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.exclude_slash_tmp=true", "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", `sandbox_workspace_write.writable_roots=[`+strconv.Quote(dir)+`]`)
			} else {
				want = append(want, "-c", `sandbox_mode="read-only"`)
			}
			want = append(want,
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
				"-c", `shell_environment_policy.set.GIT_CONFIG_KEY_0="safe.bareRepository"`,
				"-c", `shell_environment_policy.filters={"PATH"="include","HOME"="include","TMPDIR"="include","TMP"="include","TEMP"="include","GOCACHE"="include","GIT_CONFIG_NOSYSTEM"="include","GIT_CONFIG_GLOBAL"="include","GIT_CONFIG_COUNT"="include","GIT_CONFIG_KEY_0"="include","GIT_CONFIG_VALUE_0"="include","GIT_ATTR_NOSYSTEM"="include","GIT_NO_REPLACE_OBJECTS"="include","GIT_NO_LAZY_FETCH"="include","GIT_OPTIONAL_LOCKS"="include","LANG"="include","LC_ALL"="include","LC_CTYPE"="include","TERM"="include"}`,
				"--", "-",
			)
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

func TestRunFiltersWritablePATHEntries(t *testing.T) {
	workspace, output := canonicalTempDir(t), canonicalTempDir(t)
	binary := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$PATH" > "$0.path"
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"test"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	home, err := HomePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.prepare(workspace, binary, home, output, true, config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	blocked := []string{filepath.Join(session.runtimeDir, "bin"), filepath.Join(workspace, "bin"), filepath.Join(output, "bin")}
	for _, dir := range blocked {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join(append(blocked, filepath.Dir(binary), "/usr/bin"), string(os.PathListSeparator)))
	if result, err := session.Run(t.Context(), workspace, "task", true, output, config.CodexConfig{}); err != nil || result.Answer != "done" {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	data, err := os.ReadFile(binary + ".path")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.SplitList(strings.TrimSpace(string(data)))
	for _, dir := range blocked {
		if slices.Contains(path, dir) {
			t.Fatalf("model-writable PATH entry %q reached Codex: %q", dir, path)
		}
	}
	if !slices.Contains(path, filepath.Dir(binary)) {
		t.Fatalf("trusted CLI bin missing from PATH: %q", path)
	}
}

func TestSessionDoesNotBindThreadAfterOutputStop(t *testing.T) {
	bin := installFakeCodex(t, `#!/bin/sh
cat >/dev/null
n=1
if [ -f "$0.count" ]; then n=$(( $(cat "$0.count") + 1 )); fi
printf '%s' "$n" > "$0.count"
if [ "$n" -eq 1 ]; then
  printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"thread.started","thread_id":"other"}'
else
  printf '%s\n' '{"type":"thread.started","thread_id":"fresh"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
fi
`)
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	source := t.TempDir()
	if _, err := session.Run(t.Context(), source, "first", false, "", config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "identity changed within one call") || session.threadID != "" {
		t.Fatalf("stopped output bound a thread: thread=%q err=%v", session.threadID, err)
	}
	if _, err := session.Run(t.Context(), source, "second", false, "", config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "previous invocation did not identify a resumable thread") {
		t.Fatalf("stopped output resumed: %v", err)
	}
	if calls, err := os.ReadFile(bin + ".count"); err != nil || string(calls) != "1" {
		t.Fatalf("stopped output redispatched: %q %v", calls, err)
	}
	session.NewConversation()
	result, err := session.Run(t.Context(), source, "fresh", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "done" || session.threadID != "fresh" {
		t.Fatalf("fresh conversation failed: %+v thread=%q err=%v", result, session.threadID, err)
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	t.Cleanup(func() {
		leftovers, _ := filepath.Glob(filepath.Join(root, ".minion-codex-*"))
		for _, path := range leftovers {
			_ = os.Chmod(filepath.Join(path, "locked-000"), 0o700)
			_ = os.Chmod(filepath.Join(path, "locked-500"), 0o700)
			_ = os.RemoveAll(path)
		}
	})
	result, err := Run(t.Context(), workspace, "Review", false, "", config.CodexConfig{})
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	argsFile := fakeCodex + ".args"
	promptFile := fakeCodex + ".prompt"
	settings := config.CodexConfig{Model: "gpt-6-sol", ReasoningEffort: "ultra", Skills: []string{skillPath}}
	if _, err := Run(t.Context(), dir, "Handle mention", false, "", settings); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--model\ngpt-6-sol\n", "-c\nmodel_reasoning_effort=\"ultra\"\n",
		`sandbox_mode="read-only"`,
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"answer\":\"public\",\"refutations\":\"private\"}"}}' '{"type":"turn.completed"}'
`)
	const schema = `{"type":"object","properties":{"answer":{"type":"string"},"refutations":{"type":"string"}},"required":["answer","refutations"],"additionalProperties":false}`
	result, err := Run(t.Context(), t.TempDir(), "Review candidate", true, "", config.CodexConfig{OutputSchema: schema})
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	settings := config.CodexConfig{Skills: []string{skills.BuiltinAntiPromptInjection}}
	if _, err := Run(t.Context(), dir, "Handle mention", false, "", settings); err != nil {
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
	if !strings.Contains(string(args), `sandbox_mode="read-only"`) || strings.Contains(string(args), "permissions.minion") {
		t.Fatalf("embedded skill run did not use the native read-only sandbox: %s", args)
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"audit done"}}' '{"type":"turn.completed"}'
`)
	argsFile := fakeCodex + ".args"
	if _, err := Run(t.Context(), workspace, "Audit source", false, output, config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`sandbox_mode="workspace-write"`, `sandbox_workspace_write.writable_roots=[` + strconv.Quote(output) + `]`} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("audit sandbox setting %q missing from %s", want, args)
		}
	}
	if strings.Contains(string(args), `sandbox_workspace_write.writable_roots=[`+strconv.Quote(workspace)) {
		t.Fatalf("audit source is writable: %s", args)
	}
	if strings.Contains(string(args), "--ephemeral") {
		t.Fatal("audit helpers cannot fork an ephemeral parent session")
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("removed persistent reports directory: %v", err)
	}
}

func TestRunRejectsUnsafeAuditReportDirectories(t *testing.T) {
	workspace := canonicalTempDir(t)
	installFakeCodex(t, "#!/bin/sh\nexit 99\n")
	inside := filepath.Join(workspace, "reports")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{workspace, inside, filepath.Dir(workspace), "/"} {
		want := "separate from the source snapshot"
		if unsafe == "/" {
			want = "expected a non-root directory"
		}
		if _, err := Run(t.Context(), workspace, "Audit source", false, unsafe, config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), want) {
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"approved"}}' '{"type":"turn.completed"}'
`)
	if _, err := Run(t.Context(), workspace, "Critique reports", false, "", config.CodexConfig{ReadOnlyDirs: []string{reports}}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(binary + ".args")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), `sandbox_mode="read-only"`) || strings.Contains(string(args), "sandbox_workspace_write.writable_roots") {
		t.Fatalf("critic did not use Codex's native read-only sandbox: %s", args)
	}
}

func TestRunRejectsUnsafeCriticReadDirs(t *testing.T) {
	root := canonicalTempDir(t)
	workspace, reports := filepath.Join(root, "repo"), filepath.Join(root, "reports")
	for _, dir := range []string{workspace, reports} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installFakeCodex(t, "#!/bin/sh\nexit 99\n")
	for _, test := range []struct{ dir, failure string }{
		{root, "separate from the source snapshot"},
		{workspace, "separate from the source snapshot"},
		{string(os.PathSeparator), "expected a non-root directory"},
		{filepath.Join(root, "missing"), ""},
	} {
		_, err := Run(t.Context(), workspace, "Critique reports", false, "", config.CodexConfig{ReadOnlyDirs: []string{test.dir}})
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
	symlinkOrSkip(t, skill, alias)
	binary := installFakeCodex(t, "#!/bin/sh\ntouch \"$0.invoked\"\nexit 99\n")
	for _, path := range []string{skill, alias} {
		if _, err := Run(t.Context(), source, "task", false, "", config.CodexConfig{Skills: []string{path}}); err == nil || !strings.Contains(err.Error(), "skill is inside the untrusted source snapshot") {
			t.Fatalf("accepted source skill %q: %v", path, err)
		}
	}
	if _, err := os.Stat(binary + ".invoked"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid skill started Codex: %v", err)
	}
}

func TestRunRejectsSkillInsideOutput(t *testing.T) {
	source, output := t.TempDir(), t.TempDir()
	skill := filepath.Join(output, "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: test\n---\nTest skill.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := installFakeCodex(t, "#!/bin/sh\ntouch \"$0.invoked\"\nexit 99\n")
	if _, err := Run(t.Context(), source, "task", false, output, config.CodexConfig{Skills: []string{skill}}); err == nil || !strings.Contains(err.Error(), "inside a writable directory") {
		t.Fatalf("accepted writable skill: %v", err)
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
	symlinkOrSkip(t, output, alias)
	binary := installFakeCodex(t, "#!/bin/sh\ntouch \"$0.invoked\"\nexit 99\n")
	for _, path := range []string{output, child, filepath.Dir(output), alias} {
		if _, err := Run(t.Context(), source, "task", false, output, config.CodexConfig{ReadOnlyDirs: []string{path}}); err == nil || !strings.Contains(err.Error(), "read-only directory overlaps the output directory") {
			t.Fatalf("accepted read-only/output overlap %q: %v", path, err)
		}
	}
	if _, err := os.Stat(binary + ".invoked"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid grants started Codex: %v", err)
	}
}

func TestChildEnvironmentIsolatesCodexSessionButKeepsAuthentication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix PATH fixture; Windows environment is tested separately")
	}
	env := childEnvironment([]string{
		"CODEX_PERMISSION_PROFILE=unsafe-parent-profile", "CODEX_THREAD_ID=parent", "CODEX_SESSION_ID=parent",
		"CODEX_APP_TOOLS_PIPE_PATH=parent-app", "CODEX_SANDBOX_NETWORK_DISABLED=1",
		"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "CODEX_ACCESS_TOKEN=test-access-token",
		"GH_TOKEN=secret", "GITHUB_TOKEN=secret", "OPENAI_API_KEY=secret", "PGPASSWORD=secret",
		"PATH=/usr/bin::.:rel", "HOME=/trusted/home", "HTTPS_PROXY=http://proxy", "SSL_CERT_FILE=/cert",
		"TMPDIR=/host-tmp", "GOCACHE=/host-cache", "GIT_CONFIG_GLOBAL=/private-git-config", "GIT_DIR=/private-git-dir",
	}, "/runtime", "/cache", "/trusted/codex", false)
	joined := strings.Join(env, "\n")
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("unsafe CLI PATH: %q", env)
	}
	for _, denied := range []string{"unsafe-parent-profile", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_SANDBOX_NETWORK_DISABLED", "GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "PGPASSWORD", "/host-tmp", "/host-cache", "/private-git-"} {
		if strings.Contains(joined, denied) {
			t.Fatalf("inherited parent session state: %s", joined)
		}
	}
	for _, kept := range []string{"CODEX_HOME=/trusted/codex", "CODEX_API_KEY=test-key", "CODEX_ACCESS_TOKEN=test-access-token", "PATH=/usr/bin", "HOME=/trusted/home", "HTTPS_PROXY=http://proxy", "SSL_CERT_FILE=/cert", "TMPDIR=/runtime", "GOCACHE=/cache", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("required environment setting missing: %s", kept)
		}
	}
}

func TestWindowsChildEnvironmentNormalizesNamesAndPath(t *testing.T) {
	safe, writable := t.TempDir(), t.TempDir()
	canonicalSafe, err := filepath.EvalSymlinks(safe)
	if err != nil {
		t.Fatal(err)
	}
	env := childEnvironmentForOS("windows", []string{
		"Path=" + safe + string(os.PathListSeparator) + writable,
		"pAtH=" + safe,
		"CoDeX_HoMe=/wrong", "UserProfile=/operator", "AppData=/operator/appdata",
		"LocalAppData=/operator/local", "SystemRoot=/windows", "GH_TOKEN=private",
	}, writable, writable, "/selected", false, writable)
	joined := strings.Join(env, "\n")
	for _, required := range []string{"PATH=" + canonicalSafe, "CODEX_HOME=/selected", "USERPROFILE=/operator", "APPDATA=/operator/appdata", "LOCALAPPDATA=/operator/local", "SYSTEMROOT=/windows"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing Windows environment %q in %s", required, joined)
		}
	}
	if strings.Count(joined, "PATH=") != 1 || strings.Contains(joined, "GH_TOKEN") || strings.Contains(joined, "/wrong") {
		t.Fatalf("unsafe Windows environment: %s", joined)
	}
}

func TestWindowsShellEnvironmentFilters(t *testing.T) {
	linux := shellEnvironmentFilters("linux")
	win := shellEnvironmentFilters("windows")
	if slices.Contains(linux, `"USERPROFILE"="include"`) {
		t.Fatalf("Linux received Windows shell variables: %v", linux)
	}
	for _, key := range []string{"PATH", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SYSTEMROOT", "COMSPEC"} {
		if !slices.Contains(win, strconv.Quote(key)+`="include"`) {
			t.Fatalf("Windows shell variable %q missing from %v", key, win)
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
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
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
			if _, err := Run(t.Context(), t.TempDir(), "Review this checkout", false, "", settings); err != nil {
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
		{name: "answer", output: `printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"dummy-configured-key"}}' '{"type":"turn.completed"}'`},
		{name: "event error", output: `printf '%s\n' '{"type":"turn.failed","error":{"message":"dummy-configured-key"}}'`, wantError: true},
		{name: "stderr", output: `printf '%s\n' 'dummy-configured-key' >&2; exit 1`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\n"+test.output+"\n")
			result, err := Run(t.Context(), t.TempDir(), "Review this checkout", false, "", config.CodexConfig{APIKey: "dummy-configured-key"})
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
	_, err = Run(t.Context(), t.TempDir(), "Review this checkout", false, "", config.CodexConfig{APIKey: key})
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
		clipped                           bool
	}{
		{"stderr tail", "printf '%s' " + aicli.ShellQuote(stderr) + " >&2\nexit 1\n", "LEAKSUFFIX", "rate limit reached", false},
		{"event prefix", "printf '%s\\n' " + aicli.ShellQuote(string(event)) + "\n", "KEYPREFIX__LEAK", "codex exec failed", true},
		{"incomplete event", "printf '%s\\n' " + aicli.ShellQuote(string(incompleteEvent)) + "\n", "KEYPREFIX__LEAK", "did not complete", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\n"+tc.script)
			_, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: key})
			if err == nil || strings.Contains(err.Error(), tc.forbidden) || !strings.Contains(err.Error(), tc.required) || tc.clipped && (!strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "y...") || len(err.Error()) > bounded.DiagnosticBytes+200) {
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
	_, err = Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: key})
	if err == nil || strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("encoded configured key escaped failed event: %v", err)
	}
}

func TestRunStartFailureReleasesUsageBaseline(t *testing.T) {
	installFakeCodex(t, "#!"+filepath.Join(t.TempDir(), "missing-interpreter")+"\n")
	root := writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	var session Session
	session.threadID = "root"
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	_, err := session.Run(t.Context(), canonicalTempDir(t), "Review", false, "", config.CodexConfig{})
	if err == nil || session.used || session.accounting.before != nil || session.accounting.known["root"].path != root {
		t.Fatalf("start failure retained baseline or lost known rollout: err=%v used=%t before=%v known=%v", err, session.used, session.accounting.before != nil, session.accounting.known["root"].path)
	}
}

func TestRunReportsCodexFailure(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' '{"type":"turn.failed","error":{"message":"model request failed"}}'
printf '%s\n' 'connection lost' >&2
exit 7
`)
	_, err := Run(t.Context(), t.TempDir(), "Review", false, "", config.CodexConfig{})
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

func TestJSONLOutputKeepsFirstParseFailure(t *testing.T) {
	cancellations := 0
	output := jsonlOutput{cancel: func() { cancellations++ }}
	_, _ = output.Write([]byte("{\"type\":\"thread.started\",\"thread_id\":\"root\"}\n{damaged\n{\"type\":\"thread.started\",\"thread_id\":\"other\"}\n"))
	output.stream.Finish()
	if output.parseErr == nil || output.stopErr != nil || output.threadID != "root" || cancellations != 1 {
		t.Fatalf("parse failure was replaced by a later event: parse=%v stop=%v thread=%q cancellations=%d", output.parseErr, output.stopErr, output.threadID, cancellations)
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

func TestJSONLOutputFramingLimitCancelsCLI(t *testing.T) {
	for _, test := range []struct{ name, events string }{
		{"line", strings.Repeat("x", aicli.MaxEventBytes+1) + "\n"},
		{"event count", strings.Repeat("{}\n", 100001)},
	} {
		t.Run(test.name, func(t *testing.T) {
			cancellations := 0
			output := jsonlOutput{cancel: func() { cancellations++ }}
			_, _ = output.Write([]byte(test.events))
			_, _ = output.Write([]byte(`{"type":"turn.completed"}` + "\n"))
			if cancellations != 1 || !output.failed || !errors.Is(output.stopErr, aicli.ErrOutputLimit) || output.completed {
				t.Fatalf("framing limit did not stop CLI once: cancellations=%d failed=%t stop=%v completed=%t", cancellations, output.failed, output.stopErr, output.completed)
			}
		})
	}
}

func TestRunRetainsFallbackUsageOnFailure(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":70,"output_tokens":12}}'
exit 7
`)
	result, err := Run(t.Context(), t.TempDir(), "Review", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "exit status 7") || strings.Contains(err.Error(), "did not identify a resumable thread") || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 100, CachedInput: 70, Output: 12}) {
		t.Fatalf("lost partial usage: result=%+v, error=%v", result, err)
	}
}

func TestRunRejectsMissingInitialThreadIdentityWithKnownUsage(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":70,"output_tokens":12}}'
`)
	result, err := Run(t.Context(), t.TempDir(), "Review", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "did not identify a resumable thread") || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 100, CachedInput: 70, Output: 12}) {
		t.Fatalf("missing initial thread accepted or lost usage: result=%+v, error=%v", result, err)
	}
}

func TestRunHonorsCancellation(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\nexec sleep 10\n")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
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
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := canonicalTempDir(t)
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := Run(t.Context(), dir, "Review", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "codex executable "+path+" is inside the untrusted source snapshot") {
		t.Fatalf("Run() error = %v, want source snapshot executable rejection", err)
	}
}

func installFakeCodex(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake Codex executable")
	}
	t.Setenv("CODEX_HOME", canonicalTempDir(t))
	path, err := filepath.EvalSymlinks(filepath.Join(testsupport.FakeOnPath(t, "codex", script), "codex"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
