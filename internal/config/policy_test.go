package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func policyRounds(value int) *int { return &value }

func policyConfig() Config {
	return Config{
		BotLogin: "bot", Adversarial: "codex",
		Repositories: []Repository{{Name: "owner/repo", ID: 11, OwnerID: 22, Actions: []string{"review"}}},
	}
}

func TestRequiredCleanRoundsPrecedence(t *testing.T) {
	cfg := policyConfig()
	cfg.CriticStopping.MaxRounds = 1
	cfg.CriticStopping.RequiredCleanRounds = policyRounds(2)
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatalf("independent clean-review count rejected: %v", err)
	}
	cfg.CriticStopping.MaxRounds = 0
	cfg.CriticStopping.RequiredCleanRounds = nil
	if got := cfg.RequiredCleanRounds(11, 7); got != 1 {
		t.Fatalf("critic default = %d, want 1", got)
	}
	cfg.Adversarial = ""
	if got := cfg.RequiredCleanRounds(11, 7); got != 0 {
		t.Fatalf("no-critic default = %d, want 0", got)
	}
	cfg.Adversarial = "codex"
	cfg.CriticStopping.RequiredCleanRounds = policyRounds(2)
	cfg.RequesterPolicies = []RequesterPolicy{{Login: "Writer", ID: 7, RequiredCleanRounds: policyRounds(3)}}
	cfg.Repositories[0].RequiredCleanRounds = policyRounds(0)
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		repoID, requesterID int64
		want                int
	}{
		{11, 7, 0}, // Repository override wins, including explicit zero.
		{11, 8, 0},
		{12, 7, 3}, // Requester is matched by stable GitHub ID.
		{12, 8, 2},
	} {
		if got := cfg.RequiredCleanRounds(tt.repoID, tt.requesterID); got != tt.want {
			t.Errorf("repo %d requester %d: got %d, want %d", tt.repoID, tt.requesterID, got, tt.want)
		}
	}
	if cfg.RequesterPolicies[0].Login != "writer" {
		t.Fatalf("requester login was not normalized: %+v", cfg.RequesterPolicies[0])
	}
	cfg.Repositories[0].RequiredCleanRounds = nil
	if got := cfg.RequiredCleanRounds(11, 7); got != 3 {
		t.Fatalf("repo inheritance = %d, want requester override 3", got)
	}
	cfg.RequesterPolicies[0].RequiredCleanRounds = nil
	if got := cfg.RequiredCleanRounds(11, 7); got != 2 {
		t.Fatalf("requester inheritance = %d, want global 2", got)
	}
}

func TestPolicyJSONRoundTrip(t *testing.T) {
	cfg := policyConfig()
	cfg.CriticStopping.RequiredCleanRounds = policyRounds(0)
	cfg.RequesterPolicies = []RequesterPolicy{{Login: "writer", ID: 7, RequiredCleanRounds: policyRounds(0)}}
	cfg.Repositories[0].RequiredCleanRounds = policyRounds(0)
	cfg.AIInstructions = "Read the task and report findings.\n"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte(`"required_clean_rounds":0`)) != 3 || !bytes.Contains(data, []byte(`"ai_instructions":"Read the task and report findings.\n"`)) {
		t.Fatalf("explicit zero or instructions lost: %s", data)
	}
	decoded, err := decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if decoded.CriticStopping.RequiredCleanRounds == nil || decoded.RequesterPolicies[0].RequiredCleanRounds == nil ||
		decoded.Repositories[0].RequiredCleanRounds == nil || decoded.RequiredCleanRounds(11, 7) != 0 || decoded.AIInstructions != cfg.AIInstructions {
		t.Fatalf("policy roundtrip changed settings: %+v", decoded)
	}
}

func TestPolicyValidation(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		change     func(*Config)
	}{
		{"negative global", "critic_stopping.required_clean_rounds", func(c *Config) { c.CriticStopping.RequiredCleanRounds = policyRounds(-1) }},
		{"global above maximum", "critic_stopping.required_clean_rounds", func(c *Config) { c.CriticStopping.RequiredCleanRounds = policyRounds(21) }},
		{"negative requester", "requester_policies.required_clean_rounds", func(c *Config) {
			c.RequesterPolicies = []RequesterPolicy{{Login: "writer", ID: 7, RequiredCleanRounds: policyRounds(-1)}}
		}},
		{"requester above maximum", "requester_policies.required_clean_rounds", func(c *Config) {
			c.RequesterPolicies = []RequesterPolicy{{Login: "writer", ID: 7, RequiredCleanRounds: policyRounds(21)}}
		}},
		{"repo above maximum", "owner/repo.required_clean_rounds", func(c *Config) { c.Repositories[0].RequiredCleanRounds = policyRounds(21) }},
		{"invalid login", "requester_policies", func(c *Config) { c.RequesterPolicies = []RequesterPolicy{{Login: "bad/name", ID: 7}} }},
		{"missing ID", "requester_policies", func(c *Config) { c.RequesterPolicies = []RequesterPolicy{{Login: "writer"}} }},
		{"duplicate login", "duplicate login or ID", func(c *Config) {
			c.RequesterPolicies = []RequesterPolicy{{Login: "Writer", ID: 7}, {Login: "writer", ID: 8}}
		}},
		{"duplicate ID", "duplicate login or ID", func(c *Config) {
			c.RequesterPolicies = []RequesterPolicy{{Login: "alice", ID: 7}, {Login: "bob", ID: 7}}
		}},
		{"duplicate repository ID", "duplicate repository ID", func(c *Config) {
			c.Repositories = append(c.Repositories, Repository{Name: "owner/renamed", ID: 11, OwnerID: 22, Actions: []string{"review"}})
		}},
		{"instructions over limit", "ai_instructions", func(c *Config) { c.AIInstructions = strings.Repeat("x", MaxAIInstructionsBytes+1) }},
		{"instructions invalid UTF-8", "ai_instructions", func(c *Config) { c.AIInstructions = string([]byte{0xff}) }},
		{"instructions NUL", "ai_instructions", func(c *Config) { c.AIInstructions = "before\x00after" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := policyConfig()
			tt.change(&cfg)
			if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validation error = %v, want %q", err, tt.want)
			}
		})
	}
}
