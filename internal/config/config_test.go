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
		cfg.Codex.Model != "" || cfg.Codex.ReasoningEffort != "" ||
		cfg.Claude.Model != "" || cfg.Claude.Effort != "" ||
		cfg.Main != "codex" || cfg.Adversarial != "" || cfg.CriticMaxRounds != 5 || cfg.JobTimeoutMinutes != 240 ||
		len(cfg.Commands) != 4 || len(cfg.AlwaysOnSkills) != 2 ||
		cfg.MaxGitHubContextBytes != DefaultMaxGitHubContextBytes ||
		cfg.AlwaysOnSkills[0] != defaultAntiInjectionSkill || cfg.AlwaysOnSkills[1] != defaultCavemanSkill {
		t.Fatalf("checked-in configuration has operational settings: %+v", cfg)
	}
}

func TestLoadMaxGitHubContextBytes(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		want        int
		wantError   bool
	}{
		{name: "default", want: DefaultMaxGitHubContextBytes},
		{name: "custom", field: `,"max_github_context_bytes":12345`, want: 12345},
		{name: "explicit zero", field: `,"max_github_context_bytes":0`, want: DefaultMaxGitHubContextBytes},
		{name: "negative", field: `,"max_github_context_bytes":-1`, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := `{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.field + `}`
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "max_github_context_bytes") {
					t.Fatalf("Load() error = %v, want size validation error", err)
				}
				return
			}
			if err != nil || cfg.MaxGitHubContextBytes != tt.want {
				t.Fatalf("Load() = (%d, %v), want (%d, nil)", cfg.MaxGitHubContextBytes, err, tt.want)
			}
		})
	}
}

func TestLoadValidatesWhitelistAndResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"bot_login":"linkdataminion","repositories":[{"name":"example-owner/example-repo","id":1,"owner_id":2,"actions":["review","fix"]}]}`
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

func TestValidateNormalizesIdentityPinLogins(t *testing.T) {
	cfg := Config{BotLogin: "bot", InvitationOwners: []string{"Owner"}, InvitationOwnerIDs: map[string]int64{"OWNER": 2}, Repositories: []Repository{{
		Name: "Owner/Repo", ID: 1, OwnerID: 2, Actions: []string{"review"},
		AllowedActors: []string{"Writer"}, AllowedActorIDs: map[string]int64{"WRITER": 7},
	}}}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.InvitationOwnerIDs["owner"] != 2 || cfg.Repositories[0].AllowedActorIDs["writer"] != 7 ||
		!cfg.Repositories[0].AllowsActor("writer", 7) {
		t.Fatalf("login pins were not normalized: owners=%v actors=%v", cfg.InvitationOwnerIDs, cfg.Repositories[0].AllowedActorIDs)
	}
	for _, tc := range []struct {
		name  string
		pins  map[string]int64
		owner bool
	}{
		{"duplicate owner casing", map[string]int64{"Owner": 2, "owner": 2}, true},
		{"duplicate actor casing", map[string]int64{"Writer": 7, "writer": 7}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := cfg
			copy.Repositories = append([]Repository(nil), cfg.Repositories...)
			if tc.owner {
				copy.InvitationOwnerIDs = tc.pins
			} else {
				copy.Repositories[0].AllowedActorIDs = tc.pins
			}
			if err := copy.Validate(t.TempDir()); err == nil {
				t.Fatal("accepted duplicate case-insensitive ID pins")
			}
		})
	}
}

func TestCommandMappingsValidateWithoutInstalledSkills(t *testing.T) {
	base := Config{BotLogin: "linkdataminion", Repositories: []Repository{{Name: "example-owner/example-repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	cfg := base
	cfg.Codex = CodexConfig{Model: " gpt-6-sol ", ReasoningEffort: " ultra "}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.Model != "gpt-6-sol" || cfg.Codex.ReasoningEffort != "ultra" ||
		len(cfg.Commands) != 4 || cfg.Commands["audit"].Action != "comment" ||
		cfg.Commands["security-audit"].Action != "security-audit" ||
		len(cfg.AlwaysOnSkills) != 2 || cfg.AlwaysOnSkills[0] != defaultAntiInjectionSkill ||
		cfg.AlwaysOnSkills[1] != defaultCavemanSkill {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if names := cfg.CommandNames(); strings.Join(names, ",") != "audit,fix,review,security-audit" {
		t.Fatalf("command names = %v", names)
	}
	for _, tt := range []struct {
		name    string
		mapping map[string]CommandMapping
	}{
		{"bad name", map[string]CommandMapping{"Review": {Action: "review", Skills: []string{defaultReviewSkill}}}},
		{"bad action", map[string]CommandMapping{"review": {Action: "publish", Skills: []string{defaultReviewSkill}}}},
		{"missing skill", map[string]CommandMapping{"review": {Action: "review"}}},
		{"bad URL", map[string]CommandMapping{"review": {Action: "review", Skills: []string{"https://evil.invalid/SKILL.md"}}}},
		{"duplicate skill", map[string]CommandMapping{"review": {Action: "review", Skills: []string{defaultReviewSkill, defaultReviewSkill}}}},
		{"security audit without validator skill", map[string]CommandMapping{"security-audit": {Action: "security-audit", Skills: []string{defaultReviewSkill}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copy := base
			copy.Commands = tt.mapping
			if err := copy.Validate(t.TempDir()); err == nil {
				t.Fatal("accepted invalid command mapping")
			}
		})
	}
}

func TestExplicitEmptyCommandsAndAlwaysOnSkills(t *testing.T) {
	cfg := Config{BotLogin: "bot", Commands: map[string]CommandMapping{}, AlwaysOnSkills: []string{}, Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.Commands == nil || len(cfg.Commands) != 0 || cfg.AlwaysOnSkills == nil || len(cfg.AlwaysOnSkills) != 0 {
		t.Fatalf("explicit disable was replaced by defaults: %+v", cfg)
	}
}

func TestSkillSourceNamesDoNotConflict(t *testing.T) {
	cfg := Config{BotLogin: "bot", Commands: map[string]CommandMapping{
		"review":  {Action: "review", Skills: []string{defaultReviewSkill}},
		"inspect": {Action: "comment", Skills: []string{"https://github.com/example/fork/tree/main/skills/ponytail-review"}},
	}, AlwaysOnSkills: []string{}, Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "conflicting source URLs") {
		t.Fatalf("accepted conflicting skill sources: %v", err)
	}
}

func TestSkillSourceURLsNormalizeTrailingSlash(t *testing.T) {
	url := "https://github.com/example/skills/tree/main/skills/review"
	cfg := Config{
		BotLogin: "bot",
		Commands: map[string]CommandMapping{
			"review":  {Action: "review", Skills: []string{url}},
			"inspect": {Action: "comment", Skills: []string{url + "/"}},
		},
		AlwaysOnSkills: []string{url + "/"},
		Repositories:   []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review", "inspect"}}},
	}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{cfg.Commands["review"].Skills[0], cfg.Commands["inspect"].Skills[0], cfg.AlwaysOnSkills[0]} {
		if got != url {
			t.Errorf("normalized skill URL = %q, want %q", got, url)
		}
	}
}

func TestSkillSourceNamesRejectCaseCollisions(t *testing.T) {
	url := "https://github.com/example/skills/tree/main/skills/review"
	upper := "https://github.com/example/skills/tree/main/skills/Review"
	for _, tt := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"across commands", func(c *Config) {
			c.Commands["inspect"] = CommandMapping{Action: "comment", Skills: []string{upper}}
		}, "collide on case-insensitive filesystems"},
		{"command and always-on", func(c *Config) {
			c.AlwaysOnSkills = []string{upper}
		}, "collide on case-insensitive filesystems"},
		{"within command", func(c *Config) {
			c.Commands["review"] = CommandMapping{Action: "review", Skills: []string{url, upper}}
		}, "duplicate skill"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				BotLogin:       "bot",
				Commands:       map[string]CommandMapping{"review": {Action: "review", Skills: []string{url}}},
				AlwaysOnSkills: []string{},
				Repositories:   []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}},
			}
			tt.change(&cfg)
			if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuiltinSkillCanEnableCommandWithoutInstallation(t *testing.T) {
	cfg := Config{
		BotLogin:       "bot",
		Commands:       map[string]CommandMapping{"inspect": {Action: "comment", Skills: []string{defaultAntiInjectionSkill}}},
		AlwaysOnSkills: []string{},
		Repositories:   []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"inspect"}}},
	}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	installed, err := cfg.CommandInstalled("inspect")
	if err != nil || !installed {
		t.Fatalf("builtin skill unavailable: installed=%v, err=%v", installed, err)
	}
}

func TestBuiltinNameConflictsWithOtherSource(t *testing.T) {
	cfg := Config{
		BotLogin: "bot",
		Commands: map[string]CommandMapping{
			"inspect": {Action: "comment", Skills: []string{"https://github.com/example/skills/tree/main/skills/anti-prompt-injection"}},
		},
		Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"inspect"}}},
	}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "conflicting source URLs") {
		t.Fatalf("accepted conflicting built-in skill source: %v", err)
	}
}

func TestConfiguredRepositoryActionMustExist(t *testing.T) {
	cfg := Config{BotLogin: "bot", Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"release"}}}}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsupported action") {
		t.Fatalf("accepted action without command mapping: %v", err)
	}
}

func TestRemovedReleaseActionRejected(t *testing.T) {
	cfg := Config{
		BotLogin: "bot",
		Commands: map[string]CommandMapping{
			"publish": {Action: "release", Skills: []string{defaultAntiInjectionSkill}},
		},
		Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"publish"}}},
	}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), `unsupported action "release"`) {
		t.Fatalf("accepted removed release action: %v", err)
	}
}

func TestCommandInstalledRequiresAtLeastOneSkill(t *testing.T) {
	cfg := Config{Commands: map[string]CommandMapping{"review": {Action: "review"}}}
	installed, err := cfg.CommandInstalled("review")
	if err != nil || installed {
		t.Fatalf("command without required skills is installed: %v, %v", installed, err)
	}
}

func TestLoadRejectsLegacyCodexSkills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"bot_login":"bot","codex":{"skills":["skills/review/SKILL.md"]},"repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("accepted legacy codex.skills: %v", err)
	}
}

func TestValidateJobTimeoutMinutes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		minutes   int
		want      int
		wantError bool
	}{
		{name: "default", want: 240},
		{name: "minimum", minutes: 1, want: 1},
		{name: "custom", minutes: 360, want: 360},
		{name: "maximum", minutes: 1440, want: 1440},
		{name: "negative", minutes: -1, wantError: true},
		{name: "above maximum", minutes: 1441, wantError: true},
		{name: "large", minutes: 1 << 30, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", JobTimeoutMinutes: tt.minutes, Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
			err := cfg.Validate(t.TempDir())
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "job_timeout_minutes") {
					t.Fatalf("Validate() error=%v, want runtime validation error", err)
				}
				return
			}
			if err != nil || cfg.JobTimeoutMinutes != tt.want {
				t.Fatalf("Validate() timeout=%d, error=%v; want %d", cfg.JobTimeoutMinutes, err, tt.want)
			}
		})
	}
}
