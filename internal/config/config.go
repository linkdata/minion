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

	"github.com/linkdata/minion/internal/workroot"
)

var (
	repoNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	ownerNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
)

// Repository is an explicit boundary for both observation and actions.
type Repository struct {
	Name            string           `json:"name"`
	ID              int64            `json:"id"`
	OwnerID         int64            `json:"owner_id"`
	Actions         []string         `json:"actions"`
	AllowedActors   []string         `json:"allowed_actors,omitempty"`
	AllowedActorIDs map[string]int64 `json:"allowed_actor_ids,omitempty"`
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

// Config describes the bot, selected repositories, and identity pins.
type Config struct {
	BotLogin            string      `json:"bot_login"`
	Codex               CodexConfig `json:"codex,omitempty"`
	PollIntervalSeconds int         `json:"poll_interval_seconds,omitempty"`
	JobTimeoutMinutes   int         `json:"job_timeout_minutes,omitempty"`
	StatePath           string      `json:"state_path,omitempty"`
	WorkRoot            string      `json:"work_root,omitempty"`
	WorkRootMaxBytes    int64       `json:"work_root_max_bytes,omitempty"`
	StartAt             string      `json:"start_at,omitempty"`
	// AllowedOwnerIDs maps trusted owner logins to immutable GitHub IDs.
	// An empty map denies all owners.
	AllowedOwnerIDs    map[string]int64 `json:"allowed_owner_ids"`
	InvitationOwners   []string         `json:"invitation_owners,omitempty"`
	InvitationOwnerIDs map[string]int64 `json:"invitation_owner_ids,omitempty"`
	Repositories       []Repository     `json:"repositories"`
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

// Validate checks configuration and resolves paths relative to configDir.
//
// It normalizes repository names, account logins, and identity pin keys. Every
// selected repository and invitation owner must match an [Config.AllowedOwnerIDs] pin.
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
	if c.WorkRootMaxBytes == 0 {
		c.WorkRootMaxBytes = workroot.DefaultMaxBytes
	}
	if c.WorkRootMaxBytes < 0 {
		return fmt.Errorf("work_root_max_bytes must be positive")
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
	allowedOwnerIDs, err := normalizeIDPins(c.AllowedOwnerIDs)
	if err != nil {
		return fmt.Errorf("allowed_owner_ids: %w", err)
	}
	if len(allowedOwnerIDs) == 0 {
		return fmt.Errorf("allowed_owner_ids is required: pin each trusted repository owner before starting")
	}
	for owner, id := range allowedOwnerIDs {
		if len(owner) > 39 || !ownerNamePattern.MatchString(owner) || id <= 0 {
			return fmt.Errorf("allowed_owner_ids: %q requires a valid GitHub login and positive ID", owner)
		}
	}
	c.AllowedOwnerIDs = allowedOwnerIDs
	ownerIDs, err := normalizeIDPins(c.InvitationOwnerIDs)
	if err != nil {
		return fmt.Errorf("invitation_owner_ids: %w", err)
	}
	c.InvitationOwnerIDs = ownerIDs
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
		if c.InvitationOwnerIDs[owner] <= 0 {
			return fmt.Errorf("invitation owner %q requires a positive invitation_owner_ids pin", owner)
		}
		if !c.AllowsOwner(owner, c.InvitationOwnerIDs[owner]) {
			return fmt.Errorf("invitation owner %q must match an allowed_owner_ids pin", owner)
		}
	}
	if len(c.InvitationOwnerIDs) != len(c.InvitationOwners) {
		return fmt.Errorf("invitation_owner_ids must match invitation_owners exactly")
	}
	seen := map[string]bool{}
	for i := range c.Repositories {
		repo := &c.Repositories[i]
		parts := strings.Split(repo.Name, "/")
		if !repoNamePattern.MatchString(repo.Name) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
			return fmt.Errorf("invalid repository name %q (use owner/repo)", repo.Name)
		}
		repo.Name = strings.ToLower(repo.Name)
		if repo.ID <= 0 || repo.OwnerID <= 0 {
			return fmt.Errorf("%s: id and owner_id must be positive GitHub IDs", repo.Name)
		}
		owner, _, _ := strings.Cut(repo.Name, "/")
		if !c.AllowsOwner(owner, repo.OwnerID) {
			return fmt.Errorf("%s: owner_id must match an allowed_owner_ids pin for %s", repo.Name, owner)
		}
		if seen[repo.Name] {
			return fmt.Errorf("duplicate repository %q", repo.Name)
		}
		seen[repo.Name] = true
		actorIDs, err := normalizeIDPins(repo.AllowedActorIDs)
		if err != nil {
			return fmt.Errorf("%s: allowed_actor_ids: %w", repo.Name, err)
		}
		repo.AllowedActorIDs = actorIDs
		for j := range repo.AllowedActors {
			actor := strings.ToLower(strings.TrimSpace(repo.AllowedActors[j]))
			if actor == "" || strings.ContainsAny(actor, " /\\@") {
				return fmt.Errorf("%s: invalid allowed actor %q", repo.Name, repo.AllowedActors[j])
			}
			repo.AllowedActors[j] = actor
			if repo.AllowedActorIDs[actor] <= 0 {
				return fmt.Errorf("%s: allowed actor %q requires a positive allowed_actor_ids pin", repo.Name, actor)
			}
		}
		if len(repo.AllowedActorIDs) != len(repo.AllowedActors) {
			return fmt.Errorf("%s: allowed_actor_ids must match allowed_actors exactly", repo.Name)
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

func normalizeIDPins(pins map[string]int64) (map[string]int64, error) {
	normalized := make(map[string]int64, len(pins))
	for login, id := range pins {
		login = strings.ToLower(strings.TrimSpace(login))
		if _, exists := normalized[login]; exists {
			return nil, fmt.Errorf("duplicate ID pin for %q", login)
		}
		normalized[login] = id
	}
	return normalized, nil
}

// AllowsOwner reports whether a GitHub owner login and ID match the configured pin.
//
// It returns false when no positive pin matches.
func (c Config) AllowsOwner(login string, id int64) bool {
	return id > 0 && c.AllowedOwnerIDs[strings.ToLower(login)] == id
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

// MatchesIdentity reports whether GitHub's repository and owner IDs match the configured pins.
func (r Repository) MatchesIdentity(id, ownerID int64) bool {
	return r.ID > 0 && r.OwnerID > 0 && r.ID == id && r.OwnerID == ownerID
}

// AllowsActor reports whether a GitHub account is allowed to run commands.
//
// An empty AllowedActors list permits any account with a positive ID.
func (r Repository) AllowsActor(login string, id int64) bool {
	if id <= 0 {
		return false
	}
	if len(r.AllowedActors) == 0 {
		return true
	}
	for _, actor := range r.AllowedActors {
		if strings.EqualFold(actor, login) && r.AllowedActorIDs[actor] == id {
			return true
		}
	}
	return false
}
