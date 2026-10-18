package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestWorkRootPreflightRefusesModelGrants(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, kind := range []string{"required skill", "critic optional skill", "symlink root", "resolved Codex binary", "resolved Claude binary"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("PATH", t.TempDir())
			cfg := config.Config{Main: "codex", Adversarial: "claude", WorkRoot: filepath.Join(root, "work")}
			if err := os.Mkdir(cfg.WorkRoot, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "resolved Codex binary", "resolved Claude binary":
				provider := "codex"
				if kind == "resolved Claude binary" {
					provider = "claude"
				}
				binary := filepath.Join(cfg.WorkRoot, "real-"+provider)
				if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(binary, filepath.Join(os.Getenv("PATH"), provider)); err != nil {
					t.Fatal(err)
				}
			case "required skill", "critic optional skill", "symlink root":
				parent := ".agents"
				if kind == "critic optional skill" {
					parent = ".claude"
					cfg.AlwaysOnSkills = []string{source}
				} else {
					cfg.Commands = map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{source}}}
				}
				skill := filepath.Join(root, parent, "skills", "example")
				if err := os.MkdirAll(skill, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("Test skill.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40))
				if err := os.WriteFile(filepath.Join(skill, ".minion-install.json"), []byte(metadata), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.WorkRoot = filepath.Join(skill, "work")
				if err := os.Mkdir(cfg.WorkRoot, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink root" {
					alias := filepath.Join(root, "alias")
					if err := os.Symlink(cfg.WorkRoot, alias); err != nil {
						t.Fatal(err)
					}
					cfg.WorkRoot = alias
				}
			}
			if err := checkWorkRootIsolation(cfg, filepath.Join(root, "config.json")); err == nil || !strings.Contains(err.Error(), "work root overlaps") {
				t.Fatalf("preflight accepted overlapping grant: %v", err)
			}
			if kind == "resolved Codex binary" || kind == "resolved Claude binary" {
				cfg.Main, cfg.Adversarial = "claude", ""
				if kind == "resolved Claude binary" {
					cfg.Main = "codex"
				}
				if err := checkWorkRootIsolation(cfg, filepath.Join(root, "config.json")); err != nil {
					t.Fatalf("unused executable affected startup: %v", err)
				}
			}
		})
	}
}

func TestWorkRootPreflightKeepsMissingSkillsDisabled(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", t.TempDir())
	cfg := config.Config{Main: "codex", WorkRoot: root, Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{"https://github.com/owner/repo/tree/pinned/skills/missing"}}}, AlwaysOnSkills: []string{"https://github.com/owner/repo/tree/pinned/skills/optional"}}
	if err := checkWorkRootIsolation(cfg, filepath.Join(root, "config.json")); err != nil {
		t.Fatalf("missing/unselected skills changed startup: %v", err)
	}
	installed, err := cfg.CommandInstalled("review")
	if err != nil || installed {
		t.Fatalf("missing command no longer disabled: installed=%t err=%v", installed, err)
	}
}

func TestWorkRootPreflightRequiresWorkspaceBackend(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", t.TempDir())
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := checkWorkRootIsolation(config.Config{Main: "codex", WorkRoot: file}, filepath.Join(root, "config.json"))
	if err == nil || (!errors.Is(err, syscall.ENOTDIR) && !strings.Contains(err.Error(), "requires Linux")) {
		t.Fatalf("startup did not enforce the workspace backend: %v", err)
	}
}

func TestWorkRootPreflightProtectsConfigurationAndCredentials(t *testing.T) {
	for _, kind := range []string{"config", "codex home", "claude home", "unused claude home", "credential alias"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("PATH", t.TempDir())
			work, public, native := filepath.Join(root, "work"), filepath.Join(root, "public"), filepath.Join(root, "native")
			for _, dir := range []string{work, public, native} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_HOME", native)
			t.Setenv("CLAUDE_CONFIG_DIR", native)
			cfg := config.Config{Main: "codex", WorkRoot: work, Codex: config.CodexConfig{ReadOnlyDirs: []string{public}}}
			configPath := filepath.Join(root, "config.json")
			switch kind {
			case "config":
				configPath = filepath.Join(public, "config.json")
			case "codex home":
				t.Setenv("CODEX_HOME", public)
			case "claude home":
				cfg.Main = "claude"
				cfg.Claude.ReadOnlyDirs = []string{public}
				t.Setenv("CLAUDE_CONFIG_DIR", public)
			case "unused claude home":
				t.Setenv("CLAUDE_CONFIG_DIR", public)
			case "credential alias":
				if err := os.WriteFile(filepath.Join(public, "auth.json"), []byte("fixture only"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(public, "auth.json"), filepath.Join(native, "auth.json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(configPath, []byte("fixture only"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkWorkRootIsolation(cfg, configPath); err == nil || !strings.Contains(err.Error(), "private host path") {
				t.Fatalf("preflight accepted exposed %s: %v", kind, err)
			}
		})
	}
}
