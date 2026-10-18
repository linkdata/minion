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
			if err := checkIsolation(cfg, filepath.Join(root, "config.json")); err == nil || !strings.Contains(err.Error(), "work root overlaps") {
				t.Fatalf("preflight accepted overlapping grant: %v", err)
			}
			if kind == "resolved Codex binary" || kind == "resolved Claude binary" {
				cfg.Main, cfg.Adversarial = "claude", ""
				if kind == "resolved Claude binary" {
					cfg.Main = "codex"
				}
				if err := checkIsolation(cfg, filepath.Join(root, "config.json")); err != nil {
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
	if err := checkIsolation(cfg, filepath.Join(root, "config.json")); err != nil {
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
	err := checkIsolation(config.Config{Main: "codex", WorkRoot: file}, filepath.Join(root, "config.json"))
	if err == nil || (!errors.Is(err, syscall.ENOTDIR) && !strings.Contains(err.Error(), "requires Linux")) {
		t.Fatalf("startup did not enforce the workspace backend: %v", err)
	}
}

func TestWorkRootPreflightProtectsConfigurationAndCredentials(t *testing.T) {
	for _, kind := range []string{"config", "codex home", "claude home", "unused claude home", "credential alias", "gh home", "default gh home", "gh credential alias"} {
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
			t.Setenv("GH_CONFIG_DIR", native)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
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
			case "gh home":
				t.Setenv("GH_CONFIG_DIR", public)
			case "default gh home":
				t.Setenv("GH_CONFIG_DIR", "")
				t.Setenv("XDG_CONFIG_HOME", public)
			case "gh credential alias":
				if err := os.WriteFile(filepath.Join(public, "hosts.yml"), []byte("fixture only"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(public, "hosts.yml"), filepath.Join(native, "hosts.yml")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(configPath, []byte("fixture only"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkIsolation(cfg, configPath); err == nil || !strings.Contains(err.Error(), "private host path") {
				t.Fatalf("preflight accepted exposed %s: %v", kind, err)
			}
		})
	}
}

func TestWorkRootPreflightRequiresGlobalClaudeHomeWithExplicitNativeHomes(t *testing.T) {
	for _, test := range []struct{ name, home string }{
		{"missing", ""},
		{"relative", "relative-home"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", test.home)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
			t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			err := checkIsolation(config.Config{Main: "codex", WorkRoot: work}, filepath.Join(root, "config.json"))
			if err == nil || !strings.Contains(err.Error(), "default Claude home") || test.home != "" && !strings.Contains(err.Error(), "HOME must be absolute") {
				t.Fatalf("invalid global Claude configuration was skipped: %v", err)
			}
		})
	}
}

func TestPrivatePathsRejectAliasesIntoSelectedInstalledSkill(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, provider := range []string{"codex", "claude"} {
		for _, kind := range []string{"config", "credential"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Setenv("PATH", t.TempDir())
				for key, name := range map[string]string{"CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude", "GH_CONFIG_DIR": "gh", "XDG_CONFIG_HOME": "config"} {
					t.Setenv(key, filepath.Join(root, name))
				}
				parent, credential := ".agents", filepath.Join(root, "codex", "auth.json")
				if provider == "claude" {
					parent, credential = ".claude", filepath.Join(root, "claude", ".credentials.json")
				}
				skill := filepath.Join(root, parent, "skills", "example")
				work := filepath.Join(root, "work")
				for _, path := range []string{skill, work, filepath.Dir(credential)} {
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				for name, data := range map[string]string{
					"SKILL.md":             "Test skill.\n",
					".minion-install.json": fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40)),
					"fixture-data":         "Synthetic fixture only.\n",
				} {
					if err := os.WriteFile(filepath.Join(skill, name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				cfg := config.Config{Main: provider, WorkRoot: work, Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{source}}}}
				configPath := filepath.Join(root, "config.json")
				privatePath := credential
				if kind == "config" {
					privatePath = configPath
				}
				if err := os.Symlink(filepath.Join(skill, "fixture-data"), privatePath); err != nil {
					t.Fatal(err)
				}
				if err := checkIsolation(cfg, configPath); err == nil || !strings.Contains(err.Error(), "private host path") {
					t.Fatalf("selected installed skill exposed %s: %v", kind, err)
				}
				if err := os.Remove(privatePath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(privatePath, []byte("Synthetic fixture only.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := checkIsolation(cfg, configPath); err != nil {
					t.Fatalf("non-exposed selected installed skill was invalid: %v", err)
				}
			})
		}
	}
}
