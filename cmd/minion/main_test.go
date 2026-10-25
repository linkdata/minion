package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

func startupCommandMarkers(t *testing.T) map[string]string {
	t.Helper()
	markers := make(map[string]string)
	for _, name := range []string{"git", "codex", "claude", "gh"} {
		path := filepath.Join(os.Getenv("PATH"), name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'called' > \"$0.called\"\nprintf 'fixture preflight failure' >&2\nexit 37\n"), 0700); err != nil {
			t.Fatal(err)
		}
		markers[name] = path + ".called"
	}
	return markers
}

func assertNoStartupCommands(t *testing.T, markers map[string]string) {
	t.Helper()
	for name, path := range markers {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("early shortcut invoked %s or could not inspect marker: %v", name, err)
		}
	}
}

func runStartupFixture(t *testing.T, cfg config.Config, args ...string) (int, string) {
	t.Helper()
	return runStartupFixtureWithRun(t, cfg, run, args...)
}

func runStartupFixtureWithRun(t *testing.T, cfg config.Config, start func() int, args ...string) (int, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "stderr.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	oldFlags, oldArgs, oldStderr := flag.CommandLine, os.Args, os.Stderr
	defer func() { flag.CommandLine, os.Args, os.Stderr = oldFlags, oldArgs, oldStderr }()
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = append([]string{"minion", "-config", configPath}, args...)
	os.Stderr = logFile
	code := start()
	os.Stderr = oldStderr
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data)
}

func TestCronPassSkipsPreflightWhenStateLocked(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	markers := startupCommandMarkers(t)
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	workRoot := t.TempDir()

	cfg := config.Config{
		BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot,
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	store, err := state.Open(statePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	for _, mode := range []struct {
		name string
		args []string
		code int
	}{
		{name: "cron"}, {name: "check", args: []string{"-check"}, code: 1}, {name: "loop", args: []string{"-loop"}, code: 1},
	} {
		t.Run(mode.name, func(t *testing.T) {
			code, output := runStartupFixture(t, cfg, mode.args...)
			if code != mode.code || mode.code == 1 && !strings.Contains(output, state.ErrLocked.Error()) || mode.code == 0 && !strings.Contains(output, "skipping this cron pass") {
				t.Fatalf("locked startup mode changed: exit=%d want=%d log=%q", code, mode.code, output)
			}
			assertNoStartupCommands(t, markers)
		})
	}
}

func TestRunRejectsUnsafeGrantBeforeStateOpen(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, kind := range []string{"selected executable", "selected installed skill"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && kind == "selected executable" {
				t.Skip("POSIX executable symlink fixture is unavailable on native Windows")
			}
			home := t.TempDir()
			isolateHostPaths(t, home)
			statePath := filepath.Join(t.TempDir(), "state.json")
			workRoot := t.TempDir()
			grantRoot, err := os.MkdirTemp(workRoot, "minion-admission-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(grantRoot); err != nil {
					t.Error(err)
				}
			})
			cfg := config.Config{
				Main: "codex", BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot,
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
			}
			var grantPath string
			switch kind {
			case "selected executable":
				bin := filepath.Join(grantRoot, "codex")
				grantPath = bin
				if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(bin, filepath.Join(os.Getenv("PATH"), "codex")); err != nil {
					t.Fatal(err)
				}
			case "selected installed skill":
				t.Setenv("HOME", grantRoot)
				if runtime.GOOS == "windows" {
					t.Setenv("USERPROFILE", grantRoot)
				}
				t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
				t.Setenv("GH_CONFIG_DIR", filepath.Join(home, "gh"))
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
				skill := filepath.Join(grantRoot, ".agents", "skills", "example")
				grantPath = skill
				if err := os.MkdirAll(skill, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("Fixture skill.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40))
				if err := os.WriteFile(filepath.Join(skill, ".minion-install.json"), []byte(metadata), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.Commands = map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{source}}}
				cfg.Repositories[0].Actions = []string{"review"}
			}
			grantPath, err = filepath.EvalSymlinks(grantPath)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			oldFlags, oldArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
			flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
			os.Args = []string{"minion", "-config", configPath}
			logPath := filepath.Join(t.TempDir(), "stderr.log")
			logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			code := func() int {
				oldStderr := os.Stderr
				os.Stderr = logFile
				defer func() { os.Stderr = oldStderr }()
				return run()
			}()
			if err := logFile.Close(); err != nil {
				t.Fatal(err)
			}
			logText, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if code != 1 || !strings.Contains(string(logText), "work root overlaps model filesystem grant") || !strings.Contains(string(logText), fmt.Sprintf("%q", grantPath)) {
				t.Fatalf("unsafe %s: exit=%d log=%q; want selected grant refusal", kind, code, logText)
			}
			for _, path := range []string{statePath, statePath + ".lock"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe %s opened state at %s: %v", kind, path, err)
				}
			}
		})
	}
}

func TestRunChecksGitBeforeProvidersAndGitHub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell Git wrapper fixture is unavailable on native Windows")
	}
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "cron"}, {name: "check", args: []string{"-check"}}, {name: "loop", args: []string{"-loop"}},
	} {
		for _, gitMode := range []string{"old", "failed", "real"} {
			t.Run(mode.name+"/"+gitMode, func(t *testing.T) {
				isolateHostPaths(t, t.TempDir())
				for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
					t.Setenv(key, "")
				}
				markers := startupCommandMarkers(t)
				bin := os.Getenv("PATH")
				script := "#!/bin/sh\nprintf 'called\\n' >> \"$0.called\"\n"
				switch gitMode {
				case "old":
					script += "case \" $* \" in *\" --version \"*) printf 'git version 2.45.0\\n'; exit 0;; esac\nprintf 'unexpected old-Git capability invocation' >&2\nexit 91\n"
				case "failed":
					script += "printf 'fixture Git failure' >&2\nexit 37\n"
				case "real":
					if err := os.Symlink(gitBinary, filepath.Join(bin, "git.real")); err != nil {
						t.Fatal(err)
					}
					script += "exec \"$0.real\" \"$@\"\n"
				}
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				statePath := filepath.Join(t.TempDir(), "state.json")
				cfg := config.Config{Main: "codex", BotLogin: "fixture", StatePath: statePath,
					WorkRoot: t.TempDir(),
					Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
					Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
				}
				code, output := runStartupFixture(t, cfg, mode.args...)
				if code != 1 {
					t.Fatalf("startup continued after %s preflight failure: exit=%d log=%q", gitMode, code, output)
				}
				wantStage, wantCause := "Git preflight:", "fixture Git failure"
				if gitMode == "old" {
					wantCause = "2.45.0"
				}
				if gitMode == "real" {
					wantStage, wantCause = "AI preflight:", "fixture preflight failure"
				}
				if !strings.Contains(output, wantStage) || !strings.Contains(output, wantCause) || (gitMode != "real" && strings.Contains(output, "AI preflight:")) || strings.Contains(output, "GitHub authentication:") {
					t.Fatalf("startup lost preflight order or concrete cause: %s", output)
				}
				for name, marker := range markers {
					_, err := os.Stat(marker)
					wantCalled := name == "git" || name == "codex" && gitMode == "real"
					if (err == nil) != wantCalled || err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s invocation=%t want=%t error=%v log=%q", name, err == nil, wantCalled, err, output)
					}
				}
				store, err := state.Open(statePath, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if len(store.Data.JobFailures) != 0 {
					t.Fatalf("preflight created job failures: %+v", store.Data)
				}
			})
		}
	}
}

func TestInstallSkillsBypassesGitAndProviderPreflight(t *testing.T) {
	for _, extra := range []string{"", "-check", "-loop"} {
		t.Run("extra="+extra, func(t *testing.T) {
			isolateHostPaths(t, t.TempDir())
			markers := startupCommandMarkers(t)
			root := t.TempDir()
			cfg := config.Config{BotLogin: "fixture", StatePath: filepath.Join(root, "state.json"), WorkRoot: filepath.Join(root, "not-mounted"),
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
			}
			args := []string{"-install-skills"}
			wantCode, wantLog := 0, "configured skills installed for Codex and Claude"
			if extra != "" {
				args = append(args, extra)
				wantCode, wantLog = 2, "-install-skills cannot be combined with -loop or -check"
			}
			code, output := runStartupFixture(t, cfg, args...)
			if code != wantCode || !strings.Contains(output, wantLog) {
				t.Fatalf("install-only shortcut changed: exit=%d log=%q", code, output)
			}
			assertNoStartupCommands(t, markers)
			for _, path := range []string{cfg.StatePath, cfg.StatePath + ".lock"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("install-only path opened state: path=%s error=%v", path, err)
				}
			}
		})
	}
}
