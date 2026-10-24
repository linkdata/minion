package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

var (
	ownerNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	commandPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,99}$`)
)

const (
	// DefaultMaxGitHubContextBytes is the default GitHub text budget for an AI action.
	DefaultMaxGitHubContextBytes = 32 << 10
	// DefaultJobTimeout is the runtime budget for each execution attempt.
	DefaultJobTimeout = 4 * time.Hour
	// MaxJobTimeout is the largest configurable per-attempt runtime budget.
	MaxJobTimeout = 24 * time.Hour
	// DefaultCriticMaxRounds is the default proposal/revision round limit.
	DefaultCriticMaxRounds = 5
	// MaxCriticMaxRounds is the largest configurable proposal/revision round limit.
	MaxCriticMaxRounds = 20
	// DefaultCriticConsecutiveNoBlockerRounds is the default no-blocker review streak.
	DefaultCriticConsecutiveNoBlockerRounds = 2
	// MaxCriticConsecutiveNoBlockerRounds is the largest configurable no-blocker review streak.
	MaxCriticConsecutiveNoBlockerRounds = 20
	// MaxSkillsPerRole bounds the combined required and always-on skill list.
	MaxSkillsPerRole = 64
	// MaxSkillURLBytes is the portable action limit for one skill source.
	MaxSkillURLBytes = 4096
	// MaxModelSettingBytes is the portable limit for each model and effort setting.
	MaxModelSettingBytes = 128

	defaultReviewSkill        = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-review"
	defaultFixSkill           = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail"
	defaultAuditSkill         = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-audit"
	defaultSecurityAuditSkill = "https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"
	defaultAntiInjectionSkill = skills.BuiltinAntiPromptInjection
	defaultCavemanSkill       = "https://github.com/JuliusBrussee/caveman/tree/main/skills/caveman"
)

// CommandMapping selects the application action and required AI skills for a command.
type CommandMapping struct {
	Action string   `json:"action"`
	Skills []string `json:"skills"`
}

// SupportsProvider reports whether a CLI can execute the action's workflow.
//
// An empty provider selects Codex. Claude's Bash-only tool set cannot run the
// independent subagents required by the security-audit action.
func (m CommandMapping) SupportsProvider(provider string) bool {
	return provider == "" || provider == "codex" || provider == "claude" && m.Action != "security-audit"
}

func defaultCommands() map[string]CommandMapping {
	return map[string]CommandMapping{
		"review":         {Action: "review", Skills: []string{defaultReviewSkill}},
		"fix":            {Action: "fix", Skills: []string{defaultFixSkill}},
		"audit":          {Action: "comment", Skills: []string{defaultAuditSkill}},
		"security-audit": {Action: "security-audit", Skills: []string{defaultSecurityAuditSkill}},
	}
}

// Repository is an explicit boundary for both observation and actions.
type Repository struct {
	Name            string           `json:"name"`
	ID              int64            `json:"id"`
	OwnerID         int64            `json:"owner_id"`
	Actions         []string         `json:"actions"`
	AllowedActors   []string         `json:"allowed_actors,omitempty"`
	AllowedActorIDs map[string]int64 `json:"allowed_actor_ids,omitempty"`
}

// CodexConfig contains authentication, model settings, and job skill paths.
type CodexConfig struct {
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// APIKey overrides environment credentials and stored login when nonempty.
	APIKey string `json:"api_key,omitempty"`
	// Skills contains installed paths selected by the application, not config JSON.
	Skills []string `json:"-"`
	// ReadOnlyDirs grants read access to additional application-selected directories.
	ReadOnlyDirs []string `json:"-"`
	// OutputSchema constrains the final response using application-selected JSON Schema.
	OutputSchema string `json:"-"`
}

// ClaudeConfig contains authentication, model settings, and job skill paths.
type ClaudeConfig struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// APIKey overrides environment credentials and stored login when nonempty.
	APIKey string `json:"api_key,omitempty"`
	// AllowAllUnixSockets disables Unix-socket filtering to work around Claude's
	// sandbox startup failure on affected Linux hosts. Nil selects true on Linux
	// and false on macOS.
	AllowAllUnixSockets *bool `json:"allow_all_unix_sockets,omitempty"`
	// Skills contains installed paths selected by the application, not config JSON.
	Skills []string `json:"-"`
	// ReadOnlyDirs grants read access to additional application-selected directories.
	ReadOnlyDirs []string `json:"-"`
	// OutputSchema constrains the final response using application-selected JSON Schema.
	OutputSchema string `json:"-"`
}

// SecurityAuditSkill returns the selected security-audit skill path for this job.
func (c CodexConfig) SecurityAuditSkill() string {
	for _, path := range c.Skills {
		if filepath.Base(filepath.Dir(path)) == "security-audit" {
			return path
		}
	}
	return ""
}

// CriticStopping sets the proposal limit and conditions for accepting nonblocking findings.
type CriticStopping struct {
	// MaxRounds limits proposal/revision rounds; zero selects [DefaultCriticMaxRounds].
	MaxRounds int `json:"max_rounds,omitempty"`
	// NonblockingAfterRound defaults to MaxRounds-1; zero requires every finding cleared.
	NonblockingAfterRound *int `json:"nonblocking_after_round,omitempty"`
	// ConsecutiveNoBlockerRounds defaults to two; zero disables the streak requirement.
	ConsecutiveNoBlockerRounds *int `json:"consecutive_no_blocker_rounds,omitempty"`
}

// Resolve returns effective stopping settings without replacing omitted values.
func (s CriticStopping) Resolve() (maxRounds, nonblockingAfterRound, consecutiveNoBlockerRounds int) {
	maxRounds = s.MaxRounds
	if maxRounds == 0 {
		maxRounds = DefaultCriticMaxRounds
	}
	nonblockingAfterRound = maxRounds - 1
	if s.NonblockingAfterRound != nil {
		nonblockingAfterRound = *s.NonblockingAfterRound
	}
	consecutiveNoBlockerRounds = DefaultCriticConsecutiveNoBlockerRounds
	if s.ConsecutiveNoBlockerRounds != nil {
		consecutiveNoBlockerRounds = *s.ConsecutiveNoBlockerRounds
	}
	return
}

// Config defines monitored repositories and the commands they accept.
type Config struct {
	BotLogin string `json:"bot_login"`
	// Main selects codex or claude; an empty value selects codex.
	Main string `json:"main,omitempty"`
	// Adversarial optionally selects the other provider as a read-only critic.
	Adversarial    string         `json:"adversarial,omitempty"`
	CriticStopping CriticStopping `json:"critic_stopping"`
	Codex          CodexConfig    `json:"codex,omitempty"`
	Claude         ClaudeConfig   `json:"claude,omitempty"`
	// Nil Commands selects defaults; an empty map disables all commands.
	Commands map[string]CommandMapping `json:"commands"`
	// Nil AlwaysOnSkills selects anti-prompt-injection and caveman; an empty list disables always-on skills.
	AlwaysOnSkills      []string `json:"always_on_skills"`
	PollIntervalSeconds int      `json:"poll_interval_seconds,omitempty"`
	JobTimeoutMinutes   int      `json:"job_timeout_minutes,omitempty"`
	// MaxGitHubContextBytes limits the combined UTF-8 bytes of trimmed GitHub text in one action.
	//
	// Zero selects [DefaultMaxGitHubContextBytes].
	MaxGitHubContextBytes int              `json:"max_github_context_bytes,omitempty"`
	StatePath             string           `json:"state_path,omitempty"`
	WorkRoot              string           `json:"work_root,omitempty"`
	StartAt               string           `json:"start_at,omitempty"`
	InvitationOwners      []string         `json:"invitation_owners,omitempty"`
	InvitationOwnerIDs    map[string]int64 `json:"invitation_owner_ids,omitempty"`
	Repositories          []Repository     `json:"repositories"`
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

func validModelSetting(value string) bool {
	return len(value) <= MaxModelSettingBytes && utf8.ValidString(value) && !strings.ContainsAny(value, " \t\r\n\x00")
}

// Validate checks configuration and resolves paths relative to configDir.
//
// It normalizes repository names, account logins, identity pin keys, and skill URLs.
func (c *Config) Validate(configDir string) error {
	if c.BotLogin == "" {
		return fmt.Errorf("bot_login is required")
	}
	if strings.ContainsAny(c.BotLogin, " /\\@") {
		return fmt.Errorf("invalid bot_login %q", c.BotLogin)
	}
	c.Main = strings.TrimSpace(c.Main)
	if c.Main == "" {
		c.Main = "codex"
	}
	if c.Main != "codex" && c.Main != "claude" {
		return fmt.Errorf("main must be codex or claude")
	}
	c.Adversarial = strings.TrimSpace(c.Adversarial)
	if c.Adversarial != "" && c.Adversarial != "codex" && c.Adversarial != "claude" {
		return fmt.Errorf("adversarial must be codex or claude")
	}
	if c.Adversarial == c.Main {
		return fmt.Errorf("adversarial must differ from main")
	}
	maxRounds, nonblockingAfterRound, consecutiveNoBlockerRounds := c.CriticStopping.Resolve()
	c.CriticStopping.MaxRounds = maxRounds
	if maxRounds < 1 || maxRounds > MaxCriticMaxRounds {
		return fmt.Errorf("critic_stopping.max_rounds must be between 1 and %d", MaxCriticMaxRounds)
	}
	if nonblockingAfterRound < 0 || nonblockingAfterRound > maxRounds {
		return fmt.Errorf("critic_stopping.nonblocking_after_round must be between 0 and %d", maxRounds)
	}
	if consecutiveNoBlockerRounds < 0 || consecutiveNoBlockerRounds > MaxCriticConsecutiveNoBlockerRounds {
		return fmt.Errorf("critic_stopping.consecutive_no_blocker_rounds must be between 0 and %d", MaxCriticConsecutiveNoBlockerRounds)
	}
	if strings.ContainsAny(c.Codex.APIKey, " \t\r\n\x00") || strings.ContainsAny(c.Claude.APIKey, " \t\r\n\x00") {
		return fmt.Errorf("provider api_key must not contain whitespace or NUL")
	}
	c.Codex.Model = strings.TrimSpace(c.Codex.Model)
	c.Codex.ReasoningEffort = strings.TrimSpace(c.Codex.ReasoningEffort)
	if !validModelSetting(c.Codex.Model) || !validModelSetting(c.Codex.ReasoningEffort) {
		return fmt.Errorf("codex model and reasoning_effort must be at most %d UTF-8 bytes without whitespace or NUL", MaxModelSettingBytes)
	}
	c.Claude.Model = strings.TrimSpace(c.Claude.Model)
	c.Claude.Effort = strings.TrimSpace(c.Claude.Effort)
	if !validModelSetting(c.Claude.Model) || !validModelSetting(c.Claude.Effort) {
		return fmt.Errorf("claude model and effort must be at most %d UTF-8 bytes without whitespace or NUL", MaxModelSettingBytes)
	}
	if c.AlwaysOnSkills == nil {
		c.AlwaysOnSkills = []string{defaultAntiInjectionSkill, defaultCavemanSkill}
	}
	if len(c.AlwaysOnSkills) > MaxSkillsPerRole {
		return fmt.Errorf("always_on_skills exceeds %d skills", MaxSkillsPerRole)
	}
	if err := validateSkillURLs(c.AlwaysOnSkills); err != nil {
		return fmt.Errorf("always_on_skills: %w", err)
	}
	if c.Commands == nil {
		c.Commands = defaultCommands()
	}
	for name, mapping := range c.Commands {
		if !commandPattern.MatchString(name) {
			return fmt.Errorf("invalid command name %q", name)
		}
		switch mapping.Action {
		case "review", "fix", "comment", "security-audit":
		default:
			return fmt.Errorf("command %q has unsupported action %q", name, mapping.Action)
		}
		if len(mapping.Skills) == 0 {
			return fmt.Errorf("command %q must have at least one skill", name)
		}
		if len(mapping.Skills)+len(c.AlwaysOnSkills) > MaxSkillsPerRole {
			return fmt.Errorf("command %q exceeds %d required and always-on skills", name, MaxSkillsPerRole)
		}
		if err := validateSkillURLs(mapping.Skills); err != nil {
			return fmt.Errorf("command %q: %w", name, err)
		}
		if mapping.Action == "security-audit" {
			found := false
			for _, rawURL := range mapping.Skills {
				skillName, _ := skills.Name(rawURL)
				if skillName == "security-audit" {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("command %q requires a security-audit skill", name)
			}
		}
	}
	sourceByName := make(map[string]struct{ name, url string })
	addSkill := func(rawURL string) error {
		name, _ := skills.Name(rawURL)
		key := strings.ToLower(name)
		if prior, exists := sourceByName[key]; exists {
			if prior.name != name {
				return fmt.Errorf("skill names %q and %q collide on case-insensitive filesystems", prior.name, name)
			}
			if prior.url != rawURL {
				return fmt.Errorf("skill %q has conflicting source URLs", name)
			}
		}
		sourceByName[key] = struct{ name, url string }{name, rawURL}
		return nil
	}
	for _, mapping := range c.Commands {
		for _, rawURL := range mapping.Skills {
			if err := addSkill(rawURL); err != nil {
				return err
			}
		}
	}
	for _, rawURL := range c.AlwaysOnSkills {
		if err := addSkill(rawURL); err != nil {
			return err
		}
	}
	if c.PollIntervalSeconds == 0 {
		c.PollIntervalSeconds = 60
	}
	if c.PollIntervalSeconds < 60 {
		return fmt.Errorf("poll_interval_seconds must be at least 60")
	}
	if c.JobTimeoutMinutes == 0 {
		c.JobTimeoutMinutes = int(DefaultJobTimeout / time.Minute)
	}
	if c.JobTimeoutMinutes < 1 || c.JobTimeoutMinutes > int(MaxJobTimeout/time.Minute) {
		return fmt.Errorf("job_timeout_minutes must be between 1 and %d", int(MaxJobTimeout/time.Minute))
	}
	if c.MaxGitHubContextBytes == 0 {
		c.MaxGitHubContextBytes = DefaultMaxGitHubContextBytes
	}
	if c.MaxGitHubContextBytes < 0 || c.MaxGitHubContextBytes > workspace.MaxBytes {
		return fmt.Errorf("max_github_context_bytes must be between 1 and %d", workspace.MaxBytes)
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
	}
	if len(c.InvitationOwnerIDs) != len(c.InvitationOwners) {
		return fmt.Errorf("invitation_owner_ids must match invitation_owners exactly")
	}
	seen := map[string]bool{}
	for i := range c.Repositories {
		repo := &c.Repositories[i]
		if !workspace.ValidRepository(repo.Name) {
			return fmt.Errorf("invalid repository name %q (use owner/repo)", repo.Name)
		}
		repo.Name = strings.ToLower(repo.Name)
		if repo.ID <= 0 || repo.OwnerID <= 0 {
			return fmt.Errorf("%s: id and owner_id must be positive GitHub IDs", repo.Name)
		}
		owner, _, _ := strings.Cut(repo.Name, "/")
		if pinned, ok := c.InvitationOwnerIDs[owner]; ok && pinned != repo.OwnerID {
			return fmt.Errorf("%s: owner_id conflicts with invitation_owner_ids pin for %s", repo.Name, owner)
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
		if len(repo.Actions) == 0 && len(c.Commands) != 0 {
			return fmt.Errorf("%s: actions must not be empty", repo.Name)
		}
		actions := map[string]bool{}
		for _, action := range repo.Actions {
			if _, ok := c.Commands[action]; !ok {
				return fmt.Errorf("%s: unsupported action %q", repo.Name, action)
			}
			if actions[action] {
				return fmt.Errorf("%s: duplicate action %q", repo.Name, action)
			}
			actions[action] = true
		}
	}
	return nil
}

func validateSkillURLs(urls []string) error {
	seenNames := make(map[string]bool, len(urls))
	for i, rawURL := range urls {
		normalized := strings.TrimSuffix(rawURL, "/")
		if len(normalized) > MaxSkillURLBytes {
			return fmt.Errorf("skill URL exceeds %d bytes", MaxSkillURLBytes)
		}
		name, err := skills.Name(rawURL)
		if err != nil {
			return fmt.Errorf("invalid skill URL %q: %w", rawURL, err)
		}
		key := strings.ToLower(name)
		if seenNames[key] {
			return fmt.Errorf("duplicate skill %q", name)
		}
		seenNames[key] = true
		urls[i] = normalized
	}
	return nil
}

// CommandNames returns configured command names in stable order.
func (c Config) CommandNames() []string {
	names := make([]string, 0, len(c.Commands))
	for name := range c.Commands {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// CommandInstalled reports whether a command is available.
//
// The main provider must support its action and have all required skills installed.
func (c Config) CommandInstalled(name string) (bool, error) {
	mapping, ok := c.Commands[name]
	if !ok || len(mapping.Skills) == 0 || !mapping.SupportsProvider(c.Main) {
		return false, nil
	}
	provider := c.Main
	if provider == "" {
		provider = "codex"
	}
	for _, rawURL := range mapping.Skills {
		_, installed, err := skills.InstalledPathFor(rawURL, provider)
		if err != nil || !installed {
			return false, err
		}
	}
	return true, nil
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
