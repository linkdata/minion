package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckedInConfigIsSample(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BotLogin != "example-bot" || len(cfg.InvitationOwners) != 0 ||
		len(cfg.Repositories) != 1 || cfg.Repositories[0].Name != "example-owner/example-repo" ||
		cfg.Codex.Model != "" || cfg.Codex.ReasoningEffort != "" || len(cfg.Codex.Skills) != 0 {
		t.Fatalf("checked-in configuration has operational settings: %+v", cfg)
	}
}

func TestLoadValidatesWhitelistAndResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"bot_login":"linkdataminion","repositories":[{"name":"example-owner/example-repo","actions":["review","fix","release"]}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.PollIntervalSeconds != 60 || config.StatePath != filepath.Join(dir, "var/state.json") {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	bad := strings.Replace(data, "example-owner/example-repo", "example-owner/../other", 1)
	if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted repository outside whitelist syntax")
	}
	for _, name := range []string{"../repo", "owner/.."} {
		bad = strings.Replace(data, "example-owner/example-repo", name, 1)
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted repository %q", name)
		}
	}
}

func TestInvitationOwnersAreNormalizedAndValidated(t *testing.T) {
	base := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", Actions: []string{"review"}}}}
	config := base
	config.InvitationOwners = []string{"Example-Owner", "another-owner"}
	if err := config.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if config.InvitationOwners[0] != "example-owner" || config.InvitationOwners[1] != "another-owner" {
		t.Fatalf("invitation owners not normalized: %v", config.InvitationOwners)
	}
	for _, owners := range [][]string{{"example-owner", "EXAMPLE-OWNER"}, {"example-owner/other"}, {"-bad"}, {"bad-"}, {""}} {
		config := base
		config.InvitationOwners = owners
		if err := config.Validate(t.TempDir()); err == nil {
			t.Errorf("accepted invalid invitation owners %v", owners)
		}
	}
}

func TestCodexSettingsResolveAndValidateSkillPaths(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "skills", "example", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: example\n---\nUse this skill."), 0600); err != nil {
		t.Fatal(err)
	}
	base := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", Actions: []string{"review"}}}}
	config := base
	config.Codex = CodexConfig{Model: " gpt-6-sol ", ReasoningEffort: " ultra ", Skills: []string{"skills/example/SKILL.md"}}
	if err := config.Validate(dir); err != nil {
		t.Fatal(err)
	}
	if config.Codex.Model != "gpt-6-sol" || config.Codex.ReasoningEffort != "ultra" || config.Codex.Skills[0] != skill {
		t.Fatalf("Codex settings not normalized: %+v", config.Codex)
	}
	for _, skills := range [][]string{{"missing/SKILL.md"}, {"skills/example/notes.md"}, {"skills/example/SKILL.md", skill}} {
		config := base
		config.Codex.Skills = skills
		if err := config.Validate(dir); err == nil {
			t.Errorf("accepted invalid skills %v", skills)
		}
	}
	if err := os.WriteFile(skill, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	config = base
	config.Codex.Skills = []string{skill}
	if err := config.Validate(dir); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty skill was not rejected at startup: %v", err)
	}
}

func TestAuditRequiresConfiguredSecurityAuditSkill(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", Actions: []string{"audit"}}}}
	if err := cfg.Validate(dir); err == nil || !strings.Contains(err.Error(), "audit requires") {
		t.Fatalf("audit accepted without its skill: %v", err)
	}
	skillDir := filepath.Join(dir, "security-audit")
	if err := os.Mkdir(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("Perform a security audit."), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Codex.Skills = []string{"security-audit/SKILL.md"}
	if err := cfg.Validate(dir); err != nil {
		t.Fatal(err)
	}
}
