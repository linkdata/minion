package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workroot"
)

func TestWorkRootPreflightRefusesModelGrants(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, kind := range []string{"main read", "critic read", "required skill", "critic optional skill", "system read", "symlink root"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("PATH", t.TempDir())
			cfg := config.Config{Main: "codex", Adversarial: "claude", WorkRoot: filepath.Join(root, "work")}
			if err := os.Mkdir(cfg.WorkRoot, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "main read":
				cfg.Codex.ReadOnlyDirs = []string{root}
			case "critic read":
				cfg.Claude.ReadOnlyDirs = []string{cfg.WorkRoot}
			case "required skill", "critic optional skill":
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
			case "system read":
				cfg.WorkRoot = "/usr"
			case "symlink root":
				cfg.Codex.ReadOnlyDirs = []string{cfg.WorkRoot}
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(cfg.WorkRoot, alias); err != nil {
					t.Fatal(err)
				}
				cfg.WorkRoot = alias
			}
			if err := checkWorkRootIsolation(cfg); !errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "work root overlaps") {
				t.Fatalf("preflight accepted overlapping grant: %v", err)
			}
		})
	}
}

func TestWorkRootPreflightKeepsMissingSkillsDisabled(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", t.TempDir())
	cfg := config.Config{Main: "codex", WorkRoot: root, Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{"https://github.com/owner/repo/tree/pinned/skills/missing"}}}, AlwaysOnSkills: []string{"https://github.com/owner/repo/tree/pinned/skills/optional"}, Claude: config.ClaudeConfig{ReadOnlyDirs: []string{root}}}
	if err := checkWorkRootIsolation(cfg); err != nil {
		t.Fatalf("missing/unselected skills or unused provider changed startup: %v", err)
	}
	installed, err := cfg.CommandInstalled("review")
	if err != nil || installed {
		t.Fatalf("missing command no longer disabled: installed=%t err=%v", installed, err)
	}
}
