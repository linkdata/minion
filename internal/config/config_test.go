package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
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
		cfg.Main != "codex" || cfg.Adversarial != "" || cfg.CriticStopping.MaxRounds != 5 ||
		cfg.CriticStopping.NonblockingAfterRound != nil || cfg.CriticStopping.ConsecutiveNoBlockerRounds != nil ||
		cfg.InactivityTimeoutMinutes != 10 || cfg.MaxRuntimeMinutes != 240 ||
		len(cfg.Commands) != 4 || len(cfg.AlwaysOnSkills) != 2 ||
		cfg.MaxGitHubContextBytes != DefaultMaxGitHubContextBytes ||
		cfg.AlwaysOnSkills[0] != defaultAntiInjectionSkill || cfg.AlwaysOnSkills[1] != defaultCavemanSkill {
		t.Fatalf("checked-in configuration has operational settings: %+v", cfg)
	}
}

func TestSkillListsFitPortableRoleLimits(t *testing.T) {
	urls := make([]string, MaxSkillsPerRole+1)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://github.com/o/r/tree/main/skills/skill-%d", i)
	}
	sourceAtLimit := "https://github.com/o/r/tree/" + strings.Repeat("x", MaxSkillURLBytes-len("https://github.com/o/r/tree//skills/one")) + "/skills/one"
	for _, test := range []struct {
		name               string
		required, optional []string
		wantError          string
	}{
		{"exact combined count", urls[:32], urls[32:64], ""},
		{"combined overflow", urls[:32], urls[32:], "required and always-on skills"},
		{"required overflow", urls, []string{}, "required and always-on skills"},
		{"default always-on overflow", urls[:63], nil, "required and always-on skills"},
		{"optional overflow", urls[:1], urls, "always_on_skills exceeds"},
		{"source exact bytes", []string{sourceAtLimit}, []string{}, ""},
		{"source exact bytes with slash", []string{sourceAtLimit + "/"}, []string{}, ""},
		{"optional exact bytes with slash", urls[:1], []string{sourceAtLimit + "/"}, ""},
		{"normalized source too long", []string{strings.Replace(sourceAtLimit, "/tree/", "/tree/x", 1) + "/"}, []string{}, "skill URL exceeds"},
		{"double trailing slash", []string{urls[0] + "//"}, []string{}, "invalid skill URL"},
		{"builtin trailing slash", []string{defaultAntiInjectionSkill + "/"}, []string{}, "invalid skill URL"},
		{"source too long", []string{"https://github.com/o/r/tree/" + strings.Repeat("x", MaxSkillURLBytes) + "/skills/one"}, []string{}, "skill URL exceeds"},
		{"optional source too long", urls[:1], []string{"https://github.com/o/r/tree/" + strings.Repeat("x", MaxSkillURLBytes) + "/skills/one"}, "skill URL exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", Commands: map[string]CommandMapping{"review": {Action: "review", Skills: test.required}}, AlwaysOnSkills: test.optional,
				Repositories: []Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
			if err := cfg.Validate(t.TempDir()); (err == nil) != (test.wantError == "") || err != nil && !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("skill bounds: %v; want error %q", err, test.wantError)
			}
			if test.wantError == "" {
				for _, sources := range [][]string{cfg.Commands["review"].Skills, cfg.AlwaysOnSkills} {
					for _, source := range sources {
						if len(source) > MaxSkillURLBytes || strings.HasSuffix(source, "/") {
							t.Fatalf("skill source was not normalized within the portable bound: %q", source)
						}
					}
				}
			}
		})
	}
}

func TestAlwaysOnSkillLimitWithoutCommands(t *testing.T) {
	urls := make([]string, MaxSkillsPerRole+1)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://github.com/o/r/tree/main/skills/skill-%d", i)
	}
	cfg := Config{BotLogin: "bot", Commands: map[string]CommandMapping{}, AlwaysOnSkills: urls,
		Repositories: []Repository{{Name: "o/r", ID: 1, OwnerID: 2}}}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "always_on_skills exceeds") {
		t.Fatalf("standalone always-on limit: %v", err)
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
		{name: "maximum", field: fmt.Sprintf(`,"max_github_context_bytes":%d`, workspace.MaxBytes), want: workspace.MaxBytes},
		{name: "above protocol maximum", field: fmt.Sprintf(`,"max_github_context_bytes":%d`, workspace.MaxBytes+1), wantError: true},
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
	if err := os.WriteFile(path, []byte(strings.Replace(data, "example-owner/example-repo", "owner/"+strings.Repeat("x", 100), 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("exact-limit repository name rejected: %v", err)
	}
	for _, name := range []string{"../repo", "owner/..", "owner/" + strings.Repeat("x", 101)} {
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
	for _, test := range []struct {
		name      string
		owners    []string
		pins      map[string]int64
		wantError string
	}{
		{"duplicate", []string{"example-owner", "EXAMPLE-OWNER"}, map[string]int64{"example-owner": 2}, "duplicate invitation owner"},
		{"slash", []string{"example-owner/other"}, map[string]int64{"example-owner/other": 2}, "invalid invitation owner"},
		{"leading hyphen", []string{"-bad"}, map[string]int64{"-bad": 2}, "invalid invitation owner"},
		{"trailing hyphen", []string{"bad-"}, map[string]int64{"bad-": 2}, "invalid invitation owner"},
		{"empty", []string{""}, map[string]int64{"": 2}, "invalid invitation owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := base
			config.InvitationOwners = test.owners
			config.InvitationOwnerIDs = test.pins
			if err := config.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Errorf("invalid invitation owners %v: %v; want %q", test.owners, err, test.wantError)
			}
		})
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
		name      string
		mapping   map[string]CommandMapping
		wantError string
	}{
		{"bad name", map[string]CommandMapping{"Review": {Action: "review", Skills: []string{defaultReviewSkill}}}, "invalid command name"},
		{"bad action", map[string]CommandMapping{"review": {Action: "publish", Skills: []string{defaultReviewSkill}}}, "unsupported action \"publish\""},
		{"missing skill", map[string]CommandMapping{"review": {Action: "review"}}, "must have at least one skill"},
		{"bad URL", map[string]CommandMapping{"review": {Action: "review", Skills: []string{"https://evil.invalid/SKILL.md"}}}, "invalid skill URL"},
		{"duplicate skill", map[string]CommandMapping{"review": {Action: "review", Skills: []string{defaultReviewSkill, defaultReviewSkill}}}, "duplicate skill"},
		{"security audit without validator skill", map[string]CommandMapping{"security-audit": {Action: "security-audit", Skills: []string{defaultReviewSkill}}}, "requires a security-audit skill"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copy := base
			copy.Commands = tt.mapping
			err := copy.Validate(t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("invalid command mapping error: %v", err)
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

func TestValidateActionTimeoutMinutes(t *testing.T) {
	base := func() Config {
		return Config{BotLogin: "bot", Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	}
	defaults := base()
	if err := defaults.Validate(t.TempDir()); err != nil || defaults.InactivityTimeoutMinutes != 10 || defaults.MaxRuntimeMinutes != 240 {
		t.Fatalf("timeout defaults: inactivity=%d max=%d error=%v", defaults.InactivityTimeoutMinutes, defaults.MaxRuntimeMinutes, err)
	}
	for _, tt := range []struct {
		name                string
		inactivity, runtime int
		wantInactivity      int
		wantRuntime         int
	}{
		{name: "default inactivity", runtime: 7, wantInactivity: 10, wantRuntime: 7},
		{name: "default max runtime", inactivity: 3, wantInactivity: 3, wantRuntime: 240},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			cfg.InactivityTimeoutMinutes, cfg.MaxRuntimeMinutes = tt.inactivity, tt.runtime
			if err := cfg.Validate(t.TempDir()); err != nil || cfg.InactivityTimeoutMinutes != tt.wantInactivity || cfg.MaxRuntimeMinutes != tt.wantRuntime {
				t.Fatalf("Validate() inactivity=%d max=%d error=%v; want %d/%d", cfg.InactivityTimeoutMinutes, cfg.MaxRuntimeMinutes, err, tt.wantInactivity, tt.wantRuntime)
			}
		})
	}
	independent := base()
	independent.InactivityTimeoutMinutes, independent.MaxRuntimeMinutes = 240, 1
	if err := independent.Validate(t.TempDir()); err != nil {
		t.Fatalf("inactivity may exceed max runtime: %v", err)
	}
	for _, setting := range []struct {
		name string
		max  int
		set  func(*Config, int)
		get  func(Config) int
	}{
		{"inactivity_timeout_minutes", 240,
			func(c *Config, n int) { c.InactivityTimeoutMinutes = n },
			func(c Config) int { return c.InactivityTimeoutMinutes }},
		{"max_runtime_minutes", 1440,
			func(c *Config, n int) { c.MaxRuntimeMinutes = n },
			func(c Config) int { return c.MaxRuntimeMinutes }},
	} {
		for _, tt := range []struct {
			name      string
			minutes   int
			wantError bool
		}{
			{name: "minimum", minutes: 1},
			{name: "maximum", minutes: setting.max},
			{name: "negative", minutes: -1, wantError: true},
			{name: "above maximum", minutes: setting.max + 1, wantError: true},
			{name: "large", minutes: 1 << 30, wantError: true},
		} {
			t.Run(setting.name+"/"+tt.name, func(t *testing.T) {
				cfg := base()
				setting.set(&cfg, tt.minutes)
				err := cfg.Validate(t.TempDir())
				if tt.wantError {
					if err == nil || !strings.Contains(err.Error(), setting.name) {
						t.Fatalf("Validate() error=%v, want %s validation error", err, setting.name)
					}
					return
				}
				if err != nil || setting.get(cfg) != tt.minutes {
					t.Fatalf("Validate() %s=%d, error=%v; want %d", setting.name, setting.get(cfg), err, tt.minutes)
				}
			})
		}
	}
	if _, err := decode(strings.NewReader(`{"job_timeout_minutes":30}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("accepted retired timeout key: %v", err)
	}
}

func TestCommandNameMatchesPortableLimit(t *testing.T) {
	for _, length := range []int{1, 100, 101} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			name := strings.Repeat("a", length)
			cfg := Config{BotLogin: "bot", Commands: map[string]CommandMapping{name: {Action: "comment", Skills: []string{defaultAntiInjectionSkill}}}, Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{name}}}}
			err := cfg.Validate(t.TempDir())
			if (err != nil) != (length > 100) {
				t.Fatalf("command length %d validation=%v", length, err)
			}
			if length > 100 && !strings.Contains(err.Error(), "invalid command name") {
				t.Fatalf("wrong long-name refusal: %v", err)
			}
		})
	}
}

func TestCriticStoppingDefaultsAndExplicitZero(t *testing.T) {
	for _, tt := range []struct {
		name, fields                      string
		want                              [3]int
		explicitThreshold, explicitStreak bool
	}{
		{name: "omitted", want: [3]int{5, 4, 2}},
		{name: "critic default", fields: `,"adversarial":"claude"`, want: [3]int{5, 4, 2}},
		{name: "empty section", fields: `,"critic_stopping":{}`, want: [3]int{5, 4, 2}},
		{name: "null section", fields: `,"critic_stopping":null`, want: [3]int{5, 4, 2}},
		{name: "zero maximum", fields: `,"critic_stopping":{"max_rounds":0}`, want: [3]int{5, 4, 2}},
		{name: "one round defaults strict", fields: `,"critic_stopping":{"max_rounds":1}`, want: [3]int{1, 0, 2}},
		{name: "ten rounds", fields: `,"critic_stopping":{"max_rounds":10}`, want: [3]int{10, 9, 2}},
		{name: "hard maximum", fields: `,"critic_stopping":{"max_rounds":20}`, want: [3]int{20, 19, 2}},
		{name: "zero threshold disables", fields: `,"critic_stopping":{"nonblocking_after_round":0}`, want: [3]int{5, 0, 2}, explicitThreshold: true},
		{name: "first round", fields: `,"critic_stopping":{"nonblocking_after_round":1}`, want: [3]int{5, 1, 2}, explicitThreshold: true},
		{name: "last round", fields: `,"critic_stopping":{"max_rounds":3,"nonblocking_after_round":3}`, want: [3]int{3, 3, 2}, explicitThreshold: true},
		{name: "null settings are omitted", fields: `,"critic_stopping":{"nonblocking_after_round":null,"consecutive_no_blocker_rounds":null}`, want: [3]int{5, 4, 2}},
		{name: "zero streak disables", fields: `,"critic_stopping":{"nonblocking_after_round":1,"consecutive_no_blocker_rounds":0}`, want: [3]int{5, 1, 0}, explicitThreshold: true, explicitStreak: true},
		{name: "both disabled", fields: `,"critic_stopping":{"nonblocking_after_round":0,"consecutive_no_blocker_rounds":0}`, want: [3]int{5, 0, 0}, explicitThreshold: true, explicitStreak: true},
		{name: "one no-blocker round", fields: `,"critic_stopping":{"consecutive_no_blocker_rounds":1}`, want: [3]int{5, 4, 1}, explicitStreak: true},
		{name: "streak above maximum", fields: `,"critic_stopping":{"max_rounds":1,"consecutive_no_blocker_rounds":20}`, want: [3]int{1, 0, 20}, explicitStreak: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decode(strings.NewReader(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.fields + `}`))
			if err != nil {
				t.Fatal(err)
			}
			maxRounds, threshold, streak := cfg.CriticStopping.Resolve()
			if got := [3]int{maxRounds, threshold, streak}; got != tt.want {
				t.Fatalf("Resolve() = %v; want %v", got, tt.want)
			}
			if err := cfg.Validate(t.TempDir()); err != nil || cfg.CriticStopping.MaxRounds != tt.want[0] {
				t.Fatalf("Validate() maximum=%d err=%v; want %d", cfg.CriticStopping.MaxRounds, err, tt.want[0])
			}
			if (cfg.CriticStopping.NonblockingAfterRound != nil) != tt.explicitThreshold || (cfg.CriticStopping.ConsecutiveNoBlockerRounds != nil) != tt.explicitStreak {
				t.Fatalf("Validate() replaced omitted settings: %+v", cfg.CriticStopping)
			}
			data, err := json.Marshal(cfg)
			if err != nil || strings.Contains(string(data), `"nonblocking_after_round":`) != tt.explicitThreshold || strings.Contains(string(data), `"consecutive_no_blocker_rounds":`) != tt.explicitStreak {
				t.Fatalf("explicit zero/omission lost: %s, %v", data, err)
			}
			decoded, err := decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			maxRounds, threshold, streak = decoded.CriticStopping.Resolve()
			if got := [3]int{maxRounds, threshold, streak}; got != tt.want {
				t.Fatalf("JSON roundtrip = %v; want %v", got, tt.want)
			}
			if !tt.explicitThreshold {
				decoded.CriticStopping.MaxRounds = 2
				_, threshold, _ = decoded.CriticStopping.Resolve()
				if err := decoded.Validate(t.TempDir()); err != nil || threshold != 1 {
					t.Fatalf("omitted threshold stopped following maximum: %v", err)
				}
			}
		})
	}
}

func TestCriticStoppingRejectsInvalidSettingsAndOldKeys(t *testing.T) {
	for _, tt := range []struct {
		name, fields, wantError string
	}{
		{"negative maximum", `,"critic_stopping":{"max_rounds":-1}`, "critic_stopping.max_rounds"},
		{"excess maximum", `,"critic_stopping":{"max_rounds":21}`, "critic_stopping.max_rounds"},
		{"negative threshold", `,"critic_stopping":{"nonblocking_after_round":-1}`, "critic_stopping.nonblocking_after_round"},
		{"threshold above maximum", `,"critic_stopping":{"max_rounds":3,"nonblocking_after_round":4}`, "critic_stopping.nonblocking_after_round"},
		{"negative streak", `,"critic_stopping":{"consecutive_no_blocker_rounds":-1}`, "critic_stopping.consecutive_no_blocker_rounds"},
		{"excess streak", `,"critic_stopping":{"consecutive_no_blocker_rounds":21}`, "critic_stopping.consecutive_no_blocker_rounds"},
		{"fraction maximum", `,"critic_stopping":{"max_rounds":1.5}`, "max_rounds"},
		{"fraction threshold", `,"critic_stopping":{"nonblocking_after_round":1.5}`, "nonblocking_after_round"},
		{"fraction streak", `,"critic_stopping":{"consecutive_no_blocker_rounds":1.5}`, "consecutive_no_blocker_rounds"},
		{"string threshold", `,"critic_stopping":{"nonblocking_after_round":"1"}`, "nonblocking_after_round"},
		{"string streak", `,"critic_stopping":{"consecutive_no_blocker_rounds":"2"}`, "consecutive_no_blocker_rounds"},
		{"unknown nested key", `,"critic_stopping":{"max_round":2}`, `unknown field "max_round"`},
		{"old maximum", `,"critic_max_rounds":5`, `unknown field "critic_max_rounds"`},
		{"old threshold", `,"critic_nonblocking_rounds":0`, `unknown field "critic_nonblocking_rounds"`},
		{"old maximum with new section", `,"critic_max_rounds":5,"critic_stopping":{"max_rounds":5}`, `unknown field "critic_max_rounds"`},
		{"old threshold with new section", `,"critic_stopping":{"nonblocking_after_round":0},"critic_nonblocking_rounds":0`, `unknown field "critic_nonblocking_rounds"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decode(strings.NewReader(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.fields + `}`))
			if err == nil {
				err = cfg.Validate(t.TempDir())
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("invalid critic stopping error=%v; want %q", err, tt.wantError)
			}
		})
	}
}
