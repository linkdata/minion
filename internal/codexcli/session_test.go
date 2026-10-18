package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
)

func TestSessionResumesWithStablePathsAndExactCurrentGrants(t *testing.T) {
	bin := installFakeCodex(t, `#!/bin/sh
set -eu
n=1
if [ -f "$0.count" ]; then n=$(( $(cat "$0.count") + 1 )); fi
printf '%s' "$n" > "$0.count"
printf '%s\n' "$@" > "$0.args.$n"
pwd > "$0.cwd.$n"
printf '%s\n' "$TMPDIR" "$GOCACHE" > "$0.paths.$n"
cat > "$0.prompt.$n"
if [ "$n" -eq 2 ]; then test -f "$TMPDIR/prior-note"; fi
if [ "$n" -eq 3 ]; then test ! -e "$TMPDIR/prior-note"; fi
printf private-note > "$TMPDIR/prior-note"
id=root
if [ "$n" -eq 3 ]; then id=fresh; fi
printf '{"type":"thread.started","thread_id":"%s"}\n' "$id"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	ctx := context.Background()
	dir, output, first, second := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	settings := config.CodexConfig{Model: "model", ReasoningEffort: "high", OutputSchema: `{"type":"object"}`, Skills: []string{skills.BuiltinAntiPromptInjection}, ReadOnlyDirs: []string{first}}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	for call := 1; call <= 3; call++ {
		if call == 2 {
			settings.ReadOnlyDirs = []string{second}
		}
		if call == 3 {
			session.NewConversation()
			settings.ReadOnlyDirs = nil
		}
		result, err := session.Run(ctx, dir, "message "+strconv.Itoa(call), true, output, settings)
		if err != nil || result.Answer != "done" {
			t.Fatalf("call%d result=%+v error=%v", call, result, err)
		}
	}
	read := func(suffix string) string {
		t.Helper()
		data, err := os.ReadFile(bin + suffix)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for call := 1; call <= 3; call++ {
		args := read(".args." + strconv.Itoa(call))
		for _, required := range []string{"--ignore-user-config\n", "--ignore-rules\n", "--strict-config\n", "--skip-git-repo-check\n", "approval_policy=never\n", "permissions.minion.network.enabled=false\n", `":root"="deny"`, `":tmpdir"="write"`, strconv.Quote(dir) + `="write"`, strconv.Quote(output) + `="write"`, strconv.Quote(filepath.Join(session.runtimeDir, ".agents")) + `="read"`, strconv.Quote(filepath.Join(session.runtimeDir, ".codex")) + `="read"`, "--disable\nmemories\n", "--disable\nhooks\n", "--disable\nskill_search\n", "--model\nmodel\n", `model_reasoning_effort="high"`, "--output-schema\n" + filepath.Join(session.runtimeDir, "output-schema.json") + "\n"} {
			if !strings.Contains(args, required) {
				t.Fatalf("call%d missing policy %q in %s", call, required, args)
			}
		}
		if call == 2 {
			if !strings.HasPrefix(args, "exec\nresume\n") || !strings.HasSuffix(args, "--\nroot\n-\n") {
				t.Fatalf("not an exact-ID stdin resume: %s", args)
			}
		} else if !strings.HasPrefix(args, "exec\n--json\n") || !strings.HasSuffix(args, "--\n-\n") {
			t.Fatalf("call%d unexpectedly continued a conversation: %s", call, args)
		}
		for _, grant := range []struct {
			path string
			want bool
		}{{first, call == 1}, {second, call == 2}} {
			if strings.Contains(args, strconv.Quote(grant.path)+`="read"`) != grant.want {
				t.Fatalf("call%d retained or omitted phase grant %s: %s", call, grant.path, args)
			}
		}
		if read(".cwd."+strconv.Itoa(call)) != session.runtimeDir+"\n" || read(".paths."+strconv.Itoa(call)) != session.runtimeDir+"\n"+filepath.Join(session.runtimeDir, "go-build")+"\n" {
			t.Fatalf("call%d runtime paths moved", call)
		}
		prompt := read(".prompt." + strconv.Itoa(call))
		if !strings.Contains(prompt, strconv.Quote(session.skillPaths[0])) || !strings.HasSuffix(prompt, "message "+strconv.Itoa(call)) {
			t.Fatalf("call%d skill path or stdin changed: %s", call, prompt)
		}
	}
	runtime, embedded := session.runtimeDir, session.embeddedDir
	// The CLI's own transcript directory is not part of Session scratch cleanup.
	transcript := filepath.Join(os.Getenv("CODEX_HOME"), "kept-native-transcript")
	if err := os.WriteFile(transcript, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{runtime, embedded} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch retained: %s: %v", path, err)
		}
	}
	if data, err := os.ReadFile(transcript); err != nil || string(data) != "kept" {
		t.Fatalf("native transcript removed: %q %v", data, err)
	}
}

func TestSessionBindsImmutableSettingsBeforeNativeDispatch(t *testing.T) {
	for _, field := range []string{"source", "output", "writable", "model", "effort", "key", "skills", "schema", "home", "binary"} {
		t.Run(field, func(t *testing.T) {
			bin := installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf called >> \"$0.calls\"\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
			dir, output := t.TempDir(), t.TempDir()
			settings := config.CodexConfig{Skills: []string{skills.BuiltinAntiPromptInjection}}
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := session.Run(context.Background(), dir, "first", false, output, settings); err != nil {
				t.Fatal(err)
			}
			writable := false
			switch field {
			case "source":
				dir = t.TempDir()
			case "output":
				output = t.TempDir()
			case "writable":
				writable = true
			case "model":
				settings.Model = "other"
			case "effort":
				settings.ReasoningEffort = "other"
			case "key":
				settings.APIKey = "private-value"
			case "skills":
				settings.Skills[0] = "different-skill"
			case "schema":
				settings.OutputSchema = `{"type":"string"}`
			case "home":
				t.Setenv("CODEX_HOME", t.TempDir())
			case "binary":
				replacement := t.TempDir()
				if err := os.WriteFile(filepath.Join(replacement, "codex"), []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", replacement+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			result, err := session.Run(context.Background(), dir, "second", writable, output, settings)
			if err == nil || !strings.Contains(err.Error(), "host settings changed") || strings.Contains(err.Error(), "private-value") || result.Answer != "" {
				t.Fatalf("immutable change accepted: %+v %v", result, err)
			}
			if data, err := os.ReadFile(bin + ".calls"); err != nil || string(data) != "called" {
				t.Fatalf("invalid second call reached CLI: %q %v", data, err)
			}
		})
	}
}

func TestSessionRejectsMissingOrDifferentResumeIdentity(t *testing.T) {
	for _, identity := range []string{"missing first", "missing resume", "different resume"} {
		t.Run(identity, func(t *testing.T) {
			bin := installFakeCodex(t, `#!/bin/sh
cat >/dev/null
printf called >> "$0.calls"
if [ "$1" != exec ] || [ "$2" != resume ]; then
  if [ "`+identity+`" != 'missing first' ]; then printf '%s\n' '{"type":"thread.started","thread_id":"root"}'; fi
elif [ "`+identity+`" = 'different resume' ]; then
  printf '%s\n' '{"type":"thread.started","thread_id":"other"}'
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
			dir := t.TempDir()
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := session.Run(context.Background(), dir, "first", false, "", config.CodexConfig{}); err != nil {
				t.Fatal(err)
			}
			result, err := session.Run(context.Background(), dir, "second", false, "", config.CodexConfig{})
			if err == nil || result.Answer != "" || identity != "missing first" && result.UsageComplete {
				t.Fatalf("unbound resume accepted: %+v %v", result, err)
			}
			want := "calledcalled"
			if identity == "missing first" {
				want = "called"
			}
			if data, err := os.ReadFile(bin + ".calls"); err != nil || string(data) != want {
				t.Fatalf("identity dispatch=%q error=%v", data, err)
			}
		})
	}
}

func TestSessionRuntimeControlFilesDoNotFollowPriorModelSymlinks(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	dir := t.TempDir()
	settings := config.CodexConfig{OutputSchema: `{"type":"object"}`}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Run(context.Background(), dir, "first", false, "", settings); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{instructionRoot, "output-schema.json"} {
		path := filepath.Join(session.runtimeDir, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Run(context.Background(), dir, "second", false, "", settings); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "original" {
		t.Fatalf("prior model symlink target overwritten: %q %v", data, err)
	}
}

func TestSessionFreshScratchResetFailureDoesNotDispatch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	bin := installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf called >> \"$0.calls\"\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	dir := t.TempDir()
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Run(context.Background(), dir, "first", false, "", config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(session.runtimeDir)
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(parent, 0700); err != nil {
			t.Error(err)
		}
	}()
	session.NewConversation()
	result, err := session.Run(context.Background(), dir, "fresh", false, "", config.CodexConfig{})
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "reset conversation scratch") || result.Answer != "" {
		t.Fatalf("fresh reset failed open: %+v %v", result, err)
	}
	if data, err := os.ReadFile(bin + ".calls"); err != nil || string(data) != "called" {
		t.Fatalf("failed reset reached native CLI: %q %v", data, err)
	}
}

func TestSessionRejectsRetargetedHomeWithoutNativeDispatch(t *testing.T) {
	binary := installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", alias)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	if _, err := session.Run(context.Background(), source, "first", false, "", config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), source, "second", false, "", config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "host settings changed") {
		t.Fatalf("retargeted home accepted: %v", err)
	}
	if data, err := os.ReadFile(binary + ".calls"); err != nil || string(data) != "called" {
		t.Fatalf("retarget dispatched: %q,%v", data, err)
	}
}

func TestSessionMissingResumeIdentityRetainsOwnedUsage(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
cat >/dev/null
if [ "$2" = resume ]; then
 cat "$0.next" >> "$(cat "$0.rollout")"
else
 printf '%s\n' '{"type":"thread.started","thread_id":"root"}'
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	root := writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	for suffix, data := range map[string]string{".rollout": root, ".next": strings.Join(completedUsageTurn("root", "new", "r2", 20), "\n") + "\n"} {
		if err := os.WriteFile(binary+suffix, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	if _, err := session.Run(context.Background(), source, "first", false, "", config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	result, err := session.Run(context.Background(), source, "second", false, "", config.CodexConfig{})
	if err == nil || result.Answer != "" || result.UsageComplete || result.Tokens.Input != 20 || session.accounting.known["root"].path != root {
		t.Fatalf("resume lost owned usage: %+v,%v,%+v", result, err, session.accounting.known)
	}
}
