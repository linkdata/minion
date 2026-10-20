package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workroot"
)

func mountedWorkRoot(t *testing.T, statePath string) string {
	t.Helper()
	for _, candidate := range []string{os.Getenv("XDG_RUNTIME_DIR"), "/dev/shm"} {
		if candidate != "" && workroot.Check(candidate, statePath, 1<<62) == nil {
			return candidate
		}
	}
	t.Skip("startup requires a separate mounted work root")
	return ""
}

func TestCronPassSkipsPreflightWhenStateLocked(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	workRoot := mountedWorkRoot(t, statePath)

	cfg := config.Config{
		BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot, WorkRootMaxBytes: 1 << 62,
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
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

	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-config", configPath}
	if code := run(); code != 0 {
		t.Fatalf("locked cron pass returned %d; want clean skip before AI and GitHub preflight", code)
	}
}

func TestRunRejectsUnsafeGrantBeforeStateOpen(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, kind := range []string{"selected executable", "selected installed skill"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			isolateHostPaths(t, home)
			statePath := filepath.Join(t.TempDir(), "state.json")
			workRoot := mountedWorkRoot(t, statePath)
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
				Main: "codex", BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot, WorkRootMaxBytes: 1 << 62,
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
			if code != 1 || !strings.Contains(string(logText), "work root overlaps model filesystem grant") || !strings.Contains(string(logText), grantPath) {
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
