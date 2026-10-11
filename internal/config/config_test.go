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
	data := `{"bot_login":"linkdataminion","repositories":[{"name":"example-owner/example-repo","id":1,"owner_id":2,"actions":["review","fix","release"]}]}`
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
	legacy := strings.Replace(data, `"id":1,"owner_id":2,`, "", 1)
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted migrated config without repository ID pins")
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
	base := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	config := base
	config.InvitationOwners = []string{"Example-Owner", "another-owner"}
	config.InvitationOwnerIDs = map[string]int64{"example-owner": 2, "another-owner": 3}
	if err := config.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if config.InvitationOwners[0] != "example-owner" || config.InvitationOwners[1] != "another-owner" {
		t.Fatalf("invitation owners not normalized: %v", config.InvitationOwners)
	}
	for _, owners := range [][]string{{"example-owner", "EXAMPLE-OWNER"}, {"example-owner/other"}, {"-bad"}, {"bad-"}, {""}} {
		config := base
		config.InvitationOwners = owners
		config.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
		if err := config.Validate(t.TempDir()); err == nil {
			t.Errorf("accepted invalid invitation owners %v", owners)
		}
	}
}

func TestValidateRequiresExactIdentityPins(t *testing.T) {
	base := Config{BotLogin: "bot", InvitationOwners: []string{"owner"}, InvitationOwnerIDs: map[string]int64{"owner": 2}, Repositories: []Repository{{
		Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"},
		AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7},
	}}}
	if err := base.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name string
		edit func(*Config)
	}{
		{"missing repository ID", func(c *Config) { c.Repositories[0].ID = 0 }},
		{"missing repository owner ID", func(c *Config) { c.Repositories[0].OwnerID = 0 }},
		{"missing invitation owner pin", func(c *Config) { c.InvitationOwnerIDs = nil }},
		{"extra invitation owner pin", func(c *Config) { c.InvitationOwnerIDs = map[string]int64{"owner": 2, "other": 3} }},
		{"nonpositive invitation owner pin", func(c *Config) { c.InvitationOwnerIDs = map[string]int64{"owner": -1} }},
		{"conflicting owner pins", func(c *Config) { c.Repositories[0].OwnerID = 3 }},
		{"missing actor pin", func(c *Config) { c.Repositories[0].AllowedActorIDs = nil }},
		{"extra actor pin", func(c *Config) { c.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7, "other": 8} }},
		{"nonpositive actor pin", func(c *Config) { c.Repositories[0].AllowedActorIDs = map[string]int64{"writer": -1} }},
	} {
		t.Run(change.name, func(t *testing.T) {
			cfg := base
			cfg.Repositories = append([]Repository(nil), base.Repositories...)
			change.edit(&cfg)
			if err := cfg.Validate(t.TempDir()); err == nil {
				t.Fatal("accepted missing or mismatched identity pin")
			}
		})
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
	base := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
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
	cfg := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", ID: 1, OwnerID: 2, Actions: []string{"audit"}}}}
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
