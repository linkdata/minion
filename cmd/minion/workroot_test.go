package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workroot"
)

func isolateHostPaths(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
	}
	t.Setenv("PATH", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "GH_CONFIG_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG"} {
		t.Setenv(key, "")
	}
}

func symlinkFixture(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestWorkRootPreflightAllowsOrdinaryDirectory(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	cfg := config.Config{Main: "codex", WorkRoot: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "state.json")}
	if err := checkIsolation(cfg, filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("ordinary work directory was refused: %v", err)
	}
}

func TestWorkRootPreflightRefusesModelGrants(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, kind := range []string{"required skill", "critic optional skill", "symlink root", "resolved Codex binary", "resolved Claude binary"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			isolateHostPaths(t, root)
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
				extension := ""
				if runtime.GOOS == "windows" {
					extension = ".exe"
				}
				binary := filepath.Join(cfg.WorkRoot, "real-"+provider+extension)
				if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
				symlinkFixture(t, binary, filepath.Join(os.Getenv("PATH"), provider+extension))
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
					symlinkFixture(t, cfg.WorkRoot, alias)
					cfg.WorkRoot = alias
				}
			}
			if err := checkIsolation(cfg, filepath.Join(root, "config.json")); !errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "work root overlaps") {
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

func TestWorkRootPreflightIgnoresMissingSkills(t *testing.T) {
	root := t.TempDir()
	isolateHostPaths(t, root)
	cfg := config.Config{Main: "codex", WorkRoot: root, Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{"https://github.com/owner/repo/tree/pinned/skills/missing"}}}, AlwaysOnSkills: []string{"https://github.com/owner/repo/tree/pinned/skills/optional"}}
	if err := checkIsolation(cfg, filepath.Join(root, "config.json")); err != nil {
		t.Fatalf("missing/unselected skills changed startup: %v", err)
	}
}

func TestWorkRootPreflightRequiresWorkspaceBackend(t *testing.T) {
	root := t.TempDir()
	isolateHostPaths(t, root)
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := checkIsolation(config.Config{Main: "codex", WorkRoot: file}, filepath.Join(root, "config.json"))
	if !errors.Is(err, workroot.ErrUnsafe) || !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("startup did not enforce the workspace backend: %v", err)
	}
}

func TestWorkRootPreflightProtectsConfigurationAndCredentials(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, kind := range []string{"clean", "config", "codex home", "claude home", "unused claude home", "credential alias", "gh home", "default gh home", "gh credential alias"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			isolateHostPaths(t, root)
			parent := ".agents"
			if kind == "claude home" {
				parent = ".claude"
			}
			work, public, native := filepath.Join(root, "work"), filepath.Join(root, parent, "skills", "example"), filepath.Join(root, "native")
			for _, dir := range []string{work, native} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(public, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(public, "SKILL.md"), []byte("Test skill.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40))
			if err := os.WriteFile(filepath.Join(public, ".minion-install.json"), []byte(metadata), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", native)
			t.Setenv("CLAUDE_CONFIG_DIR", native)
			t.Setenv("GH_CONFIG_DIR", native)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			cfg := config.Config{Main: "codex", WorkRoot: work, Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{source}}}}
			configPath := filepath.Join(root, "config.json")
			switch kind {
			case "config":
				configPath = filepath.Join(public, "config.json")
			case "codex home":
				t.Setenv("CODEX_HOME", public)
			case "claude home":
				cfg.Main = "claude"
				t.Setenv("CLAUDE_CONFIG_DIR", public)
			case "unused claude home":
				t.Setenv("CLAUDE_CONFIG_DIR", public)
			case "credential alias":
				if err := os.WriteFile(filepath.Join(public, "auth.json"), []byte("fixture only"), 0600); err != nil {
					t.Fatal(err)
				}
				symlinkFixture(t, filepath.Join(public, "auth.json"), filepath.Join(native, "auth.json"))
			case "gh home":
				t.Setenv("GH_CONFIG_DIR", public)
			case "default gh home":
				t.Setenv("GH_CONFIG_DIR", "")
				if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
					t.Fatal(err)
				}
				symlinkFixture(t, public, filepath.Join(root, "config", "gh"))
			case "gh credential alias":
				if err := os.WriteFile(filepath.Join(public, "hosts.yml"), []byte("fixture only"), 0600); err != nil {
					t.Fatal(err)
				}
				symlinkFixture(t, filepath.Join(public, "hosts.yml"), filepath.Join(native, "hosts.yml"))
			}
			if err := os.WriteFile(configPath, []byte("fixture only"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "clean" {
				if err := checkIsolation(cfg, configPath); err != nil {
					t.Fatalf("non-exposed private paths rejected: %v", err)
				}
				return
			}

			if err := checkIsolation(cfg, configPath); !errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "private host path") || kind == "default gh home" && !strings.Contains(err.Error(), filepath.Join(root, "config", "gh")) {
				t.Fatalf("preflight accepted exposed %s: %v", kind, err)
			}
		})
	}
}

func TestWorkRootPreflightRequiresUserHomeWithExplicitNativeHomes(t *testing.T) {
	for _, test := range []struct{ name, home string }{
		{"missing", ""},
		{"relative", "relative-home"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			isolateHostPaths(t, test.home)
			t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
			t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			err := checkIsolation(config.Config{Main: "codex", WorkRoot: work}, filepath.Join(root, "config.json"))
			if err == nil || !strings.Contains(err.Error(), "resolve user home") || test.home != "" && !strings.Contains(err.Error(), "HOME must be absolute") {
				t.Fatalf("invalid user home was skipped: %v", err)
			}
		})
	}
}

func TestPrivatePathsRejectAliasesIntoSelectedInstalledSkill(t *testing.T) {
	const source = "https://github.com/owner/repo/tree/pinned/skills/example"
	for _, provider := range []string{"codex", "claude"} {
		for _, kind := range []string{
			"config", "state", "state lock", "usage", "credential", "git credentials", "git config", "overridden git config", "overridden system git config", "overridden legacy git config", "netrc",
			"xdg git credentials", "xdg git config", "ssh home",
			"default codex auth", "default codex config", "default claude credentials",
			"default claude settings", "default xdg git credentials", "default xdg git config",
			"default gh credentials",
		} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				isolateHostPaths(t, root)
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
					"SKILL.md":     "Test skill.\n",
					"fixture-data": "Synthetic fixture only.\n",
				} {
					if err := os.WriteFile(filepath.Join(skill, name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				cfg := config.Config{Main: provider, WorkRoot: work, Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{source}}}}
				configPath := filepath.Join(root, "config.json")
				privatePath := credential
				switch kind {
				case "config":
					privatePath = configPath
				case "state", "state lock", "usage":
					cfg.StatePath = filepath.Join(root, parent, "state.json")
					privatePath = cfg.StatePath
					if kind == "state lock" {
						privatePath += ".lock"
					} else if kind == "usage" {
						privatePath = filepath.Join(filepath.Dir(cfg.StatePath), "usage")
					}
				case "git credentials":
					privatePath = filepath.Join(root, ".git-credentials")
				case "git config":
					privatePath = filepath.Join(root, ".gitconfig")
				case "overridden git config":
					privatePath = filepath.Join(root, "gitconfig-override")
					t.Setenv("GIT_CONFIG_GLOBAL", privatePath)
				case "overridden system git config":
					privatePath = filepath.Join(root, "gitconfig-system-override")
					t.Setenv("GIT_CONFIG_SYSTEM", privatePath)
				case "overridden legacy git config":
					privatePath = filepath.Join(root, "gitconfig-legacy-override")
					t.Setenv("GIT_CONFIG", privatePath)
				case "netrc":
					privatePath = filepath.Join(root, ".netrc")
				case "xdg git credentials":
					privatePath = filepath.Join(root, "config", "git", "credentials")
				case "xdg git config":
					privatePath = filepath.Join(root, "config", "git", "config")
				case "ssh home":
					privatePath = filepath.Join(root, ".ssh")
				case "default codex auth":
					privatePath = filepath.Join(root, ".codex", "auth.json")
				case "default codex config":
					privatePath = filepath.Join(root, ".codex", "config.toml")
				case "default claude credentials":
					privatePath = filepath.Join(root, ".claude", ".credentials.json")
				case "default claude settings":
					privatePath = filepath.Join(root, ".claude", "settings.json")
				case "default xdg git credentials":
					privatePath = filepath.Join(root, ".config", "git", "credentials")
				case "default xdg git config":
					privatePath = filepath.Join(root, ".config", "git", "config")
				case "default gh credentials":
					privatePath = filepath.Join(root, ".config", "gh", "hosts.yml")
				}
				if err := os.MkdirAll(filepath.Dir(privatePath), 0700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(skill, "fixture-data")
				if kind == "ssh home" || kind == "usage" {
					target = skill
				}
				symlinkFixture(t, target, privatePath)
				if err := checkIsolation(cfg, configPath); err != nil {
					t.Fatalf("uninstalled skill changed admission: %v", err)
				}
				metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40))
				if err := os.WriteFile(filepath.Join(skill, ".minion-install.json"), []byte(metadata), 0600); err != nil {
					t.Fatal(err)
				}
				if err := checkIsolation(cfg, configPath); !errors.Is(err, workroot.ErrUnsafe) || !strings.Contains(err.Error(), "private host path") {
					t.Fatalf("selected installed skill exposed %s: %v", kind, err)
				}
				if err := os.Remove(privatePath); err != nil {
					t.Fatal(err)
				}
				if kind == "ssh home" || kind == "usage" {
					if err := os.Mkdir(privatePath, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(privatePath, []byte("Synthetic fixture only.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := checkIsolation(cfg, configPath); err != nil {
					t.Fatalf("non-exposed selected installed skill was invalid: %v", err)
				}
			})
		}
	}
}

func TestIsolationAdmissionPreservesMissingPathCause(t *testing.T) {
	root := t.TempDir()
	isolateHostPaths(t, root)
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	symlinkFixture(t, filepath.Join(root, "missing-config"), configPath)
	err := checkIsolation(config.Config{Main: "codex", WorkRoot: work}, configPath)
	if !errors.Is(err, workroot.ErrUnsafe) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unresolved host admission lost safety classification or missing-path cause: %v", err)
	}
}
