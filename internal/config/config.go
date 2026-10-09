package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	repoNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	ownerNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
)

// Repository is an explicit boundary for both observation and actions.
type Repository struct {
	Name                 string   `json:"name"`
	Actions              []string `json:"actions"`
	AllowedActors        []string `json:"allowed_actors,omitempty"`
	ProtectedReleaseTags bool     `json:"protected_release_tags,omitempty"`
}

type CodexConfig struct {
	Model           string   `json:"model,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Skills          []string `json:"skills,omitempty"`
}

func (c CodexConfig) SecurityAuditSkill() string {
	for _, path := range c.Skills {
		if filepath.Base(filepath.Dir(path)) == "security-audit" {
			return path
		}
	}
	return ""
}

type Config struct {
	BotLogin            string       `json:"bot_login"`
	Codex               CodexConfig  `json:"codex,omitempty"`
	PollIntervalSeconds int          `json:"poll_interval_seconds,omitempty"`
	JobTimeoutMinutes   int          `json:"job_timeout_minutes,omitempty"`
	StatePath           string       `json:"state_path,omitempty"`
	WorkRoot            string       `json:"work_root,omitempty"`
	StartAt             string       `json:"start_at,omitempty"`
	InvitationOwners    []string     `json:"invitation_owners,omitempty"`
	Repositories        []Repository `json:"repositories"`
}

// Load reads and validates configuration.
//
// Relative paths are resolved against the configuration file's directory.
func Load(path string) (Config, error) {
	var cfg Config
	var err error
	path, err = filepath.Abs(path)
	if err != nil {
		return cfg, err
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	cfg, err = decode(f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return cfg, err
	}
	if err := cfg.Validate(filepath.Dir(path)); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decode(r io.Reader) (Config, error) {
	var cfg Config
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return cfg, fmt.Errorf("decode config: trailing data")
	}
	return cfg, nil
}

func (c *Config) Validate(configDir string) error {
	if c.BotLogin == "" {
		return fmt.Errorf("bot_login is required")
	}
	if strings.ContainsAny(c.BotLogin, " /\\@") {
		return fmt.Errorf("invalid bot_login %q", c.BotLogin)
	}
	c.Codex.Model = strings.TrimSpace(c.Codex.Model)
	c.Codex.ReasoningEffort = strings.TrimSpace(c.Codex.ReasoningEffort)
	if strings.ContainsAny(c.Codex.Model, " \t\r\n") || strings.ContainsAny(c.Codex.ReasoningEffort, " \t\r\n") {
		return fmt.Errorf("codex model and reasoning_effort must not contain whitespace")
	}
	seenSkills := map[string]bool{}
	for i, skill := range c.Codex.Skills {
		if filepath.Base(skill) != "SKILL.md" {
			return fmt.Errorf("codex skill %q must name a SKILL.md file", skill)
		}
		if !filepath.IsAbs(skill) {
			skill = filepath.Join(configDir, skill)
		}
		resolved, err := filepath.EvalSymlinks(skill)
		if err != nil {
			return fmt.Errorf("codex skill %q: %w", skill, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("codex skill %q must be a regular file", skill)
		}
		content, err := os.ReadFile(resolved)
		if err != nil {
			return fmt.Errorf("read codex skill %q: %w", skill, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			return fmt.Errorf("codex skill %q is empty", skill)
		}
		if filepath.Base(resolved) != "SKILL.md" {
			return fmt.Errorf("codex skill %q must resolve to a SKILL.md file", skill)
		}
		if seenSkills[resolved] {
			return fmt.Errorf("duplicate codex skill %q", skill)
		}
		seenSkills[resolved] = true
		c.Codex.Skills[i] = resolved
	}
	if c.PollIntervalSeconds == 0 {
		c.PollIntervalSeconds = 60
	}
	if c.PollIntervalSeconds < 60 {
		return fmt.Errorf("poll_interval_seconds must be at least 60")
	}
	if c.JobTimeoutMinutes == 0 {
		c.JobTimeoutMinutes = 30
	}
	if c.JobTimeoutMinutes < 1 || c.JobTimeoutMinutes > 240 {
		return fmt.Errorf("job_timeout_minutes must be between 1 and 240")
	}
	if c.StatePath == "" {
		c.StatePath = "var/state.json"
	}
	if c.WorkRoot == "" {
		c.WorkRoot = "var/work"
	}
	if !filepath.IsAbs(c.StatePath) {
		c.StatePath = filepath.Join(configDir, c.StatePath)
	}
	if !filepath.IsAbs(c.WorkRoot) {
		c.WorkRoot = filepath.Join(configDir, c.WorkRoot)
	}
	if c.StartAt != "" {
		if _, err := time.Parse(time.RFC3339, c.StartAt); err != nil {
			return fmt.Errorf("start_at must be RFC3339: %w", err)
		}
	}
	if len(c.Repositories) == 0 {
		return fmt.Errorf("repositories must contain at least one repository")
	}
	seenOwners := map[string]bool{}
	for i, owner := range c.InvitationOwners {
		if len(owner) > 39 || !ownerNamePattern.MatchString(owner) {
			return fmt.Errorf("invalid invitation owner %q", owner)
		}
		owner = strings.ToLower(owner)
		if seenOwners[owner] {
			return fmt.Errorf("duplicate invitation owner %q", owner)
		}
		seenOwners[owner] = true
		c.InvitationOwners[i] = owner
	}
	seen := map[string]bool{}
	for i := range c.Repositories {
		repo := &c.Repositories[i]
		parts := strings.Split(repo.Name, "/")
		if !repoNamePattern.MatchString(repo.Name) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
			return fmt.Errorf("invalid repository name %q (use owner/repo)", repo.Name)
		}
		repo.Name = strings.ToLower(repo.Name)
		if seen[repo.Name] {
			return fmt.Errorf("duplicate repository %q", repo.Name)
		}
		seen[repo.Name] = true
		for j := range repo.AllowedActors {
			actor := strings.ToLower(strings.TrimSpace(repo.AllowedActors[j]))
			if actor == "" || strings.ContainsAny(actor, " /\\@") {
				return fmt.Errorf("%s: invalid allowed actor %q", repo.Name, repo.AllowedActors[j])
			}
			repo.AllowedActors[j] = actor
		}
		if len(repo.Actions) == 0 {
			return fmt.Errorf("%s: actions must not be empty", repo.Name)
		}
		actions := map[string]bool{}
		for _, action := range repo.Actions {
			if action != "review" && action != "audit" && action != "fix" && action != "release" {
				return fmt.Errorf("%s: unsupported action %q", repo.Name, action)
			}
			if action == "audit" && c.Codex.SecurityAuditSkill() == "" {
				return fmt.Errorf("%s: audit requires a security-audit/SKILL.md entry in codex.skills", repo.Name)
			}
			if actions[action] {
				return fmt.Errorf("%s: duplicate action %q", repo.Name, action)
			}
			actions[action] = true
		}
	}
	return nil
}

func (c Config) Repository(name string) (Repository, bool) {
	for _, repo := range c.Repositories {
		if repo.Name == strings.ToLower(name) {
			return repo, true
		}
	}
	return Repository{}, false
}

func (r Repository) AllowsAction(action string) bool {
	for _, allowed := range r.Actions {
		if action == allowed {
			return true
		}
	}
	return false
}

func (r Repository) AllowsActor(login string) bool {
	if len(r.AllowedActors) == 0 {
		return true
	}
	for _, actor := range r.AllowedActors {
		if strings.EqualFold(actor, login) {
			return true
		}
	}
	return false
}
