package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadModelProviders(t *testing.T) {
	for _, tt := range []struct {
		name, fields, main, adversarial string
		rounds                          int
		wantError                       string
	}{
		{name: "existing config", main: "codex", rounds: 5},
		{name: "codex only", fields: `,"main":"codex"`, main: "codex", rounds: 5},
		{name: "claude only", fields: `,"main":"claude"`, main: "claude", rounds: 5},
		{name: "claude critic", fields: `,"adversarial":"claude"`, main: "codex", adversarial: "claude", rounds: 5},
		{name: "codex critic", fields: `,"main":" claude ","adversarial":" codex "`, main: "claude", adversarial: "codex", rounds: 5},
		{name: "one round", fields: `,"critic_stopping":{"max_rounds":1}`, main: "codex", rounds: 1},
		{name: "three rounds", fields: `,"critic_stopping":{"max_rounds":3}`, main: "codex", rounds: 3},
		{name: "ten rounds", fields: `,"critic_stopping":{"max_rounds":10}`, main: "codex", rounds: 10},
		{name: "maximum rounds", fields: `,"critic_stopping":{"max_rounds":20}`, main: "codex", rounds: 20},
		{name: "zero rounds", fields: `,"critic_stopping":{"max_rounds":0}`, main: "codex", rounds: 5},
		{name: "unknown main", fields: `,"main":"other"`, wantError: "main must be"},
		{name: "unknown critic", fields: `,"adversarial":"other"`, wantError: "adversarial must be"},
		{name: "same default provider", fields: `,"adversarial":"codex"`, wantError: "adversarial must differ"},
		{name: "same explicit provider", fields: `,"main":"claude","adversarial":"claude"`, wantError: "adversarial must differ"},
		{name: "negative rounds", fields: `,"critic_stopping":{"max_rounds":-1}`, wantError: "critic_stopping.max_rounds"},
		{name: "too many rounds", fields: `,"critic_stopping":{"max_rounds":21}`, wantError: "critic_stopping.max_rounds"},
		{name: "model and effort at limit", fields: fmt.Sprintf(`,"codex":{"model":%q,"reasoning_effort":%q},"claude":{"model":%q,"effort":%q}`, strings.Repeat("m", MaxModelSettingBytes), strings.Repeat("e", MaxModelSettingBytes), strings.Repeat("m", MaxModelSettingBytes), strings.Repeat("e", MaxModelSettingBytes)), main: "codex", rounds: 5},
		{name: "codex model too long", fields: fmt.Sprintf(`,"codex":{"model":%q}`, strings.Repeat("m", MaxModelSettingBytes+1)), wantError: "codex model and reasoning_effort"},
		{name: "codex effort too long", fields: fmt.Sprintf(`,"codex":{"reasoning_effort":%q}`, strings.Repeat("e", MaxModelSettingBytes+1)), wantError: "codex model and reasoning_effort"},
		{name: "claude model too long", fields: fmt.Sprintf(`,"claude":{"model":%q}`, strings.Repeat("m", MaxModelSettingBytes+1)), wantError: "claude model and effort"},
		{name: "claude effort too long", fields: fmt.Sprintf(`,"claude":{"effort":%q}`, strings.Repeat("e", MaxModelSettingBytes+1)), wantError: "claude model and effort"},
		{name: "codex model NUL", fields: `,"codex":{"model":"bad\u0000model"}`, wantError: "codex model and reasoning_effort"},
		{name: "claude effort NUL", fields: `,"claude":{"effort":"bad\u0000effort"}`, wantError: "claude model and effort"},
		{name: "claude model whitespace", fields: `,"claude":{"model":"bad model"}`, wantError: "claude model and effort"},
		{name: "claude effort whitespace", fields: `,"claude":{"effort":"high\nlow"}`, wantError: "claude model and effort"},
		{name: "claude runtime skills", fields: `,"claude":{"skills":["/tmp/SKILL.md"]}`, wantError: "unknown field"},
		{name: "claude runtime directories", fields: `,"claude":{"read_only_dirs":["/tmp"]}`, wantError: "unknown field"},
		{name: "claude Unix socket override", fields: `,"claude":{"allow_all_unix_sockets":true}`, wantError: "unknown field"},
		{name: "codex runtime directories", fields: `,"codex":{"read_only_dirs":["/tmp"]}`, wantError: "unknown field"},
		{name: "claude runtime schema", fields: `,"claude":{"output_schema":"{}"}`, wantError: "unknown field"},
		{name: "codex runtime schema", fields: `,"codex":{"output_schema":"{}"}`, wantError: "unknown field"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := `{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.fields + `}`
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("invalid model configuration error: %v", err)
				}
				return
			}
			if err != nil || cfg.Main != tt.main || cfg.Adversarial != tt.adversarial || cfg.CriticStopping.MaxRounds != tt.rounds {
				t.Fatalf("Load() providers = %q, %q, %d, %v; want %q, %q, %d", cfg.Main, cfg.Adversarial, cfg.CriticStopping.MaxRounds, err, tt.main, tt.adversarial, tt.rounds)
			}
		})
	}
}

func TestValidateRejectsInvalidModelUTF8(t *testing.T) {
	cfg := Config{
		BotLogin:     "bot",
		Codex:        CodexConfig{Model: string([]byte{0xff})},
		Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}},
	}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "codex model and reasoning_effort") {
		t.Fatalf("invalid UTF-8 setting accepted: %v", err)
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

func TestLoadProviderAPIKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"bot_login":"bot","codex":{"api_key":"dummy-codex-key"},"claude":{"api_key":"dummy-claude-key"},"repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Codex.APIKey != "dummy-codex-key" || cfg.Claude.APIKey != "dummy-claude-key" {
		t.Fatalf("API keys not loaded: %v", err)
	}
	for _, bad := range []string{"dummy secret", "dummy\tsecret", "dummy\rsecret", "dummy\nsecret", "dummy\x00secret"} {
		for _, provider := range []string{"codex", "claude"} {
			invalid := cfg
			if provider == "codex" {
				invalid.Codex.APIKey = bad
			} else {
				invalid.Claude.APIKey = bad
			}
			if err := invalid.Validate(t.TempDir()); err == nil || strings.Contains(err.Error(), "dummy") {
				t.Fatalf("invalid key accepted or exposed: %v", err)
			}
		}
	}
}

func TestCodexNativeCredentialsStayOutOfConfigJSON(t *testing.T) {
	settings := CodexConfig{NativeAPIKey: "native-api-secret", NativeAccessToken: "native-access-secret"}
	encoded, err := json.Marshal(settings)
	if err != nil || strings.Contains(string(encoded), "secret") {
		t.Fatalf("native Codex credential serialized: %q, %v", encoded, err)
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
			if runtime.GOOS == "windows" {
				t.Setenv("USERPROFILE", home)
				t.Setenv("HOMEDRIVE", "")
				t.Setenv("HOMEPATH", "")
			}
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
		for _, action := range []string{"review", "fix", "comment", "security-audit"} {
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
