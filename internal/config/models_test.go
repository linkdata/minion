package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelProviders(t *testing.T) {
	for _, tt := range []struct {
		name, fields, main, adversarial string
		rounds                          int
		wantError                       bool
	}{
		{name: "existing config", main: "codex", rounds: 3},
		{name: "codex only", fields: `,"main":"codex"`, main: "codex", rounds: 3},
		{name: "claude only", fields: `,"main":"claude"`, main: "claude", rounds: 3},
		{name: "claude critic", fields: `,"adversarial":"claude"`, main: "codex", adversarial: "claude", rounds: 3},
		{name: "codex critic", fields: `,"main":" claude ","adversarial":" codex "`, main: "claude", adversarial: "codex", rounds: 3},
		{name: "one round", fields: `,"critic_max_rounds":1`, main: "codex", rounds: 1},
		{name: "ten rounds", fields: `,"critic_max_rounds":10`, main: "codex", rounds: 10},
		{name: "zero rounds", fields: `,"critic_max_rounds":0`, main: "codex", rounds: 3},
		{name: "unknown main", fields: `,"main":"other"`, wantError: true},
		{name: "unknown critic", fields: `,"adversarial":"other"`, wantError: true},
		{name: "same default provider", fields: `,"adversarial":"codex"`, wantError: true},
		{name: "same explicit provider", fields: `,"main":"claude","adversarial":"claude"`, wantError: true},
		{name: "negative rounds", fields: `,"critic_max_rounds":-1`, wantError: true},
		{name: "too many rounds", fields: `,"critic_max_rounds":11`, wantError: true},
		{name: "claude model whitespace", fields: `,"claude":{"model":"bad model"}`, wantError: true},
		{name: "claude effort whitespace", fields: `,"claude":{"effort":"high\nlow"}`, wantError: true},
		{name: "claude runtime skills", fields: `,"claude":{"skills":["/tmp/SKILL.md"]}`, wantError: true},
		{name: "claude runtime directories", fields: `,"claude":{"read_only_dirs":["/tmp"]}`, wantError: true},
		{name: "codex runtime directories", fields: `,"codex":{"read_only_dirs":["/tmp"]}`, wantError: true},
		{name: "claude runtime schema", fields: `,"claude":{"output_schema":"{}"}`, wantError: true},
		{name: "codex runtime schema", fields: `,"codex":{"output_schema":"{}"}`, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := `{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.fields + `}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tt.wantError {
				if err == nil {
					t.Fatal("accepted invalid model configuration")
				}
				return
			}
			if err != nil || cfg.Main != tt.main || cfg.Adversarial != tt.adversarial || cfg.CriticMaxRounds != tt.rounds {
				t.Fatalf("Load() providers = %q, %q, %d, %v; want %q, %q, %d", cfg.Main, cfg.Adversarial, cfg.CriticMaxRounds, err, tt.main, tt.adversarial, tt.rounds)
			}
		})
	}
}

func TestValidateTrimsClaudeSettings(t *testing.T) {
	cfg := Config{
		BotLogin:     "bot",
		Claude:       ClaudeConfig{Model: " sonnet ", Effort: " high "},
		Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}},
	}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.Model != "sonnet" || cfg.Claude.Effort != "high" {
		t.Fatalf("Claude settings were not trimmed: %+v", cfg.Claude)
	}
}

func TestCommandInstalledUsesMainProvider(t *testing.T) {
	for _, tt := range []struct {
		main, installedRoot, adversarial string
		want                             bool
	}{
		{main: "", installedRoot: ".agents", want: true},
		{main: "codex", installedRoot: ".agents", adversarial: "claude", want: true},
		{main: "claude", installedRoot: ".agents"},
		{main: "claude", installedRoot: ".claude", adversarial: "codex", want: true},
		{main: "codex", installedRoot: ".claude"},
	} {
		t.Run(tt.main+"/"+tt.installedRoot, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, tt.installedRoot, "skills", "ponytail-review")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("Review code.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, defaultReviewSkill, strings.Repeat("a", 40))
			if err := os.WriteFile(filepath.Join(dir, ".minion-install.json"), []byte(metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Main: tt.main, Adversarial: tt.adversarial, Commands: defaultCommands()}
			installed, err := cfg.CommandInstalled("review")
			if err != nil || installed != tt.want {
				t.Fatalf("CommandInstalled() = %t, %v; want %t", installed, err, tt.want)
			}
		})
	}
}

func TestCommandProviderCapabilities(t *testing.T) {
	for _, provider := range []string{"", "codex", "claude", "unknown"} {
		for _, action := range []string{"review", "fix", "comment", "release", "security-audit"} {
			mapping := CommandMapping{Action: action, Skills: []string{defaultAntiInjectionSkill}}
			want := provider != "unknown" && (provider != "claude" || action != "security-audit")
			if got := mapping.SupportsProvider(provider); got != want {
				t.Errorf("%s/%s supported = %t; want %t", provider, action, got, want)
			}
			cfg := Config{Main: provider, Commands: map[string]CommandMapping{"test": mapping}}
			if installed, err := cfg.CommandInstalled("test"); err != nil || installed != want {
				t.Errorf("%s/%s installed = %t, %v; want %t", provider, action, installed, err, want)
			}
		}
	}
}
