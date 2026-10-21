package action

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

// ProtocolVersion identifies the complete-history Job and Result contract.
const ProtocolVersion = 2

const (
	maxDiagnostics            = 100
	maxDiagnosticMessageBytes = 8 << 10
)

// Identity travels between hosts; authentication is supplied separately.
type Identity struct {
	JobID        string `json:"job_id"`
	AttemptID    string `json:"attempt_id"`
	ExecutorID   string `json:"executor_id"`
	Repository   string `json:"repository"`
	RepositoryID int64  `json:"repository_id"`
	OwnerID      int64  `json:"owner_id"`
	Requester    string `json:"requester"`
	RequesterID  int64  `json:"requester_id"`
	Number       int    `json:"number"`
	SubjectType  string `json:"subject_type"`
}

// RoleSettings names installed skills by their source, never by a host path.
type RoleSettings struct {
	Provider       string   `json:"provider"`
	Model          string   `json:"model,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	AlwaysOnSkills []string `json:"always_on_skills,omitempty"`
}

const maxActionTitleBytes = 256 * utf8.UTFMax // Allow 256 four-byte code points.
const maxAuditArtifactBytes = 5 << 20

type Job struct {
	Version         int                `json:"version"`
	Identity        Identity           `json:"identity"`
	Command         string             `json:"command"`
	Action          command.Action     `json:"action"`
	Title           string             `json:"title,omitempty"`
	Description     string             `json:"description,omitempty"`
	Notes           string             `json:"notes,omitempty"`
	Source          workspace.Snapshot `json:"source"`
	Main            RoleSettings       `json:"main"`
	Critic          *RoleSettings      `json:"critic,omitempty"`
	CriticMaxRounds int                `json:"critic_max_rounds"`
	// CriticNonblockingRounds is resolved by the orchestrator; zero keeps strict approval.
	CriticNonblockingRounds int `json:"critic_nonblocking_rounds"`
	// RuntimeBudget is remaining local time; a future transport must subtract transit time.
	RuntimeBudget time.Duration `json:"runtime_budget_ns"`
}

type CallUsage struct {
	Provider    string       `json:"provider"`
	Role        string       `json:"role"`
	Round       int          `json:"round"`
	ReviewPhase string       `json:"review_phase,omitempty"`
	Model       string       `json:"model,omitempty"`
	Effort      string       `json:"effort,omitempty"`
	Tokens      usage.Tokens `json:"tokens"`
	// Approved attests a parsed approving critic verdict; false is not proof of rejection.
	Approved bool `json:"approved,omitempty"`
	// NoBlockers attests an explicit has_blockers=false in a valid critic verdict.
	NoBlockers bool `json:"no_blockers,omitempty"`
	// HasBlockers attests an explicit has_blockers=true; both false means no valid verdict.
	HasBlockers bool `json:"has_blockers,omitempty"`
	// Complete confirms accounting coverage; interrupted or failed native runs are conservative.
	Complete bool `json:"complete"`
}

type Diagnostic struct {
	Code string `json:"code"`
	// Message is private; journals expose only Code and identity.
	Message string `json:"message"`
}

type Result struct {
	Version     int                `json:"version"`
	Identity    Identity           `json:"identity"`
	SourceRef   string             `json:"source_ref"`
	Answer      string             `json:"answer"`
	Changes     []workspace.Change `json:"changes,omitempty"`
	Artifacts   []workspace.File   `json:"artifacts,omitempty"`
	Calls       []CallUsage        `json:"calls,omitempty"`
	Refutations []Refutation       `json:"refutations,omitempty"`
	Diagnostics []Diagnostic       `json:"diagnostics,omitempty"`
}

func (id Identity) validate() error {
	for _, value := range []string{id.JobID, id.AttemptID, id.ExecutorID, strings.TrimSuffix(id.Requester, "[bot]")} {
		if !workspace.ValidIdentitySegment(value) {
			return errors.New("action identity is missing or invalid")
		}
	}
	if !workspace.ValidRepository(id.Repository) || id.RepositoryID <= 0 || id.OwnerID <= 0 || id.RequesterID <= 0 || id.Number <= 0 || (id.SubjectType != "Issue" && id.SubjectType != "PullRequest") {
		return errors.New("action repository or requester identity is invalid")
	}
	return nil
}

func boundedText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// Validate checks the portable model settings and skill-source limits.
func (r RoleSettings) Validate() error {
	if r.Provider != "codex" && r.Provider != "claude" {
		return errors.New("unsupported action provider")
	}
	if !boundedText(r.Model, config.MaxModelSettingBytes) || !boundedText(r.Effort, config.MaxModelSettingBytes) || strings.ContainsAny(r.Model+r.Effort, " \t\r\n") || len(r.Skills)+len(r.AlwaysOnSkills) > config.MaxSkillsPerRole {
		return errors.New("invalid action model or skills")
	}
	for _, source := range slices.Concat(r.Skills, r.AlwaysOnSkills) {
		if len(source) > config.MaxSkillURLBytes {
			return errors.New("skill source exceeds limit")
		}
		if _, err := skills.Name(source); err != nil {
			return err
		}
	}
	return nil
}

func (j Job) Validate() error {
	if j.Version != ProtocolVersion {
		return fmt.Errorf("unsupported action protocol version %d", j.Version)
	}
	if err := j.Identity.validate(); err != nil {
		return err
	}
	if !workspace.ValidIdentitySegment(j.Command) {
		return errors.New("invalid action command")
	}
	if j.RuntimeBudget <= 0 || j.RuntimeBudget > config.MaxJobTimeout {
		return errors.New("invalid action runtime budget")
	}
	if (j.Identity.SubjectType == "PullRequest") != (j.Source.Base != "") {
		return errors.New("action subject and supplied comparison differ")
	}
	switch j.Action {
	case command.Review:
		if j.Identity.SubjectType != "PullRequest" {
			return errors.New("review requires a supplied pull request comparison")
		}
	case command.Fix:
		if j.Identity.SubjectType != "Issue" {
			return errors.New("fix requires an issue")
		}
	case command.Comment, command.SecurityAudit:
	default:
		return errors.New("unsupported action")
	}
	// Bound fields before adding their lengths so a 32-bit int cannot wrap.
	if len(j.Title) > maxActionTitleBytes || len(j.Description) > workspace.MaxBytes || len(j.Notes) > workspace.MaxBytes || len(j.Title)+len(j.Description)+len(j.Notes) > workspace.MaxBytes || !utf8.ValidString(j.Title) || !utf8.ValidString(j.Description) || !utf8.ValidString(j.Notes) {
		return errors.New("action text is invalid or exceeds limits")
	}
	if err := j.Main.Validate(); err != nil {
		return err
	}
	if len(j.Main.Skills) == 0 {
		return errors.New("main role requires at least one skill")
	}
	if !(config.CommandMapping{Action: string(j.Action)}).SupportsProvider(j.Main.Provider) {
		return errors.New("action is unsupported by main provider")
	}
	if j.Critic != nil {
		if err := j.Critic.Validate(); err != nil {
			return err
		}
		if len(j.Critic.Skills) != 0 {
			return errors.New("critic role accepts only always-on skills")
		}
		if j.Critic.Provider == j.Main.Provider {
			return errors.New("main and critic currently require different providers")
		}
	}
	unexpectedRounds := j.Critic == nil && (j.CriticMaxRounds != 0 || j.CriticNonblockingRounds != 0)
	invalidRounds := j.Critic != nil && (j.CriticMaxRounds < 1 || j.CriticMaxRounds > config.MaxCriticMaxRounds || j.CriticNonblockingRounds < 0 || j.CriticNonblockingRounds > j.CriticMaxRounds)
	if unexpectedRounds || invalidRounds {
		return errors.New("invalid critic round limit")
	}
	if len(j.Source.Repository) == 0 {
		return errors.New("action requires a complete repository bundle")
	}
	if err := workspace.ValidateSnapshot(j.Source); err != nil {
		return err
	}
	if j.Action == command.Fix {
		if err := workspace.ValidateFixPaths(j.Source.Files, j.Source.Omitted); err != nil {
			return fmt.Errorf("%w: %w", gitwork.ErrUnsupportedSource, err)
		}
	}
	return nil
}

func (j Job) rounds() int {
	if j.Critic == nil {
		return 1
	}
	return j.CriticMaxRounds
}

func (j Job) acceptsCritique(round int, approved, noBlockers bool) bool {
	return noBlockers && (approved || (j.CriticNonblockingRounds > 0 && round >= j.CriticNonblockingRounds))
}

func sourceIdentity(j Job) string { return j.Identity.Repository + "@" + j.Source.Head }

func (r Result) Validate(j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	return r.validate(j, true)
}

// validate requires an already validated Job; Validate is the standalone contract entry point.
func (r Result) validate(j Job, complete bool) error {
	if !r.matchesJob(j) {
		return errors.New("action result version, identity or source differs from job")
	}
	requiresAnswer := j.Action != command.Fix || len(r.Changes) == 0
	invalidCompleteAnswer := complete && (!publishableAnswer(r.Answer) || (requiresAnswer && strings.TrimSpace(r.Answer) == ""))
	if (!complete && !boundedText(r.Answer, maxOutputBytes)) || invalidCompleteAnswer {
		return errors.New("action answer is blank, invalid or exceeds limit")
	}
	if j.Action != command.Fix && len(r.Changes) != 0 {
		return errors.New("read-only action returned source changes")
	}
	if err := j.validateChanges(r.Changes); err != nil {
		return err
	}
	if j.Action != command.SecurityAudit && len(r.Artifacts) != 0 {
		return errors.New("unexpected action report artifacts")
	}
	if err := validateResultArtifacts(j, r.Artifacts, complete); err != nil {
		return err
	}
	if complete && j.Action == command.SecurityAudit {
		for _, file := range r.Artifacts {
			if file.Path == "REPORT.md" && r.Answer != string(file.Data) {
				return errors.New("audit answer differs from REPORT.md")
			}
		}
	}
	if err := r.validateCalls(j, complete); err != nil {
		return err
	}
	if err := validateRefutations(j, r.Calls, r.Refutations); err != nil {
		return err
	}
	return validDiagnostics(r.Diagnostics)
}

func (r Result) matchesJob(j Job) bool {
	return r.Version == ProtocolVersion && r.Identity == j.Identity && r.SourceRef == j.Source.Head
}

func (r Result) validateCalls(j Job, complete bool) error {
	rounds := j.rounds()
	for i, call := range r.Calls {
		role := j.Main
		if call.Role == roleCritic && j.Critic != nil {
			role = *j.Critic
		} else if call.Role != roleMain {
			return errors.New("invalid model call role")
		}
		validOrder := i == 0 && call.Role == roleMain && call.Round == 1 && call.ReviewPhase == ""
		if i > 0 && j.Critic != nil {
			previous := r.Calls[i-1]
			accepted := j.acceptsCritique(previous.Round, previous.Approved, previous.NoBlockers)
			if call.Role == roleMain {
				validOrder = previous.Role == roleCritic && (previous.NoBlockers || previous.HasBlockers) && !accepted && call.Round == previous.Round+1 && call.ReviewPhase == ""
			} else if previous.Role == roleMain {
				validOrder = call.Round == previous.Round && call.ReviewPhase == reviewPhases(call.Round)[0]
			} else {
				validOrder = previous.ReviewPhase == reviewVerification && accepted && call.Round == previous.Round && call.ReviewPhase == reviewFull
			}
		}
		invalidVerdict := ((call.Approved || call.NoBlockers || call.HasBlockers) && call.Role != roleCritic) || (call.Approved && !call.NoBlockers) || (call.HasBlockers && call.NoBlockers)
		wrongModel := call.Provider != role.Provider || call.Model != role.Model || call.Effort != role.Effort
		if !validOrder || call.Round > rounds || invalidVerdict || wrongModel || !call.Tokens.Valid() {
			return errors.New("invalid model call usage")
		}
	}
	if complete && j.Critic != nil {
		if len(r.Calls) == 0 {
			return errors.New("action result lacks a final full review")
		}
		last := r.Calls[len(r.Calls)-1]
		if last.Role != roleCritic || last.ReviewPhase != reviewFull || !j.acceptsCritique(last.Round, last.Approved, last.NoBlockers) {
			return errors.New("action result lacks an accepted final full review")
		}
	}
	if complete && j.Critic == nil && len(r.Calls) != 1 {
		return errors.New("action result requires one main call")
	}
	return nil
}

func validDiagnostics(diagnostics []Diagnostic) error {
	if len(diagnostics) > maxDiagnostics {
		return errors.New("too many action diagnostics")
	}
	for _, diagnostic := range diagnostics {
		if !workspace.ValidIdentitySegment(diagnostic.Code) || !boundedText(diagnostic.Message, maxDiagnosticMessageBytes) {
			return errors.New("invalid action diagnostic")
		}
	}
	return nil
}

func (j Job) validateChanges(changes []workspace.Change) error {
	if len(changes) == 0 {
		return nil
	}
	if err := workspace.ValidateChanges(changes); err != nil {
		return err
	}
	paths := make([]string, 0, len(j.Source.Files)+len(j.Source.Omitted))
	for _, file := range j.Source.Files {
		paths = append(paths, file.Path)
	}
	omittedPaths := make(map[string]string, len(j.Source.Omitted))
	for _, name := range j.Source.Omitted {
		omittedPaths[workspace.FoldPath(name)] = name
		paths = append(paths, name)
	}
	for _, change := range changes {
		if omitted, exists := omittedPaths[workspace.FoldPath(change.Path)]; exists {
			return fmt.Errorf("action change %q overlaps omitted source path %q", change.Path, omitted)
		}
	}
	return workspace.ValidateChangedPaths(paths, changes)
}

var auditArtifacts = []string{"run-metadata.json", "architecture.md", "coverage-ledger.json", "findings.json", "REPORT.md", "FINDINGS-DETAIL.md", "NEEDS-VALIDATION.md"}

func validateResultArtifacts(j Job, files []workspace.File, complete bool) error {
	if len(files) == 0 {
		if complete && j.Action == command.SecurityAudit {
			return errors.New("audit result is missing report artifacts")
		}
		return nil
	}
	if complete && len(files) != len(auditArtifacts) {
		return errors.New("action result has incomplete audit artifacts")
	}
	seen := make(map[string][]byte, len(files))
	for _, file := range files {
		_, duplicate := seen[file.Path]
		if !slices.Contains(auditArtifacts, file.Path) || duplicate || file.Mode != workspace.Regular || len(file.Data) > maxAuditArtifactBytes {
			return errors.New("invalid action report artifact")
		}
		if complete && len(file.Data) == 0 && !auditArtifactMayBeEmpty(file.Path) {
			return fmt.Errorf("empty action report artifact %q", file.Path)
		}
		if (complete || file.Path != "REPORT.md") && !utf8.Valid(file.Data) {
			return fmt.Errorf("invalid action report artifact %q encoding", file.Path)
		}
		seen[file.Path] = file.Data
	}
	if len(seen["REPORT.md"]) > maxOutputBytes {
		return errors.New("action report artifacts exceed limits")
	}
	if !complete {
		return nil
	}
	companions, err := validateAuditMetadata(seen["run-metadata.json"], j.Identity.Repository, sourceIdentity(j), j.Source.Head)
	if err != nil {
		return err
	}
	for _, companion := range companions {
		if !workspace.ValidPath(companion) {
			return errors.New("audit companion file path is not portable")
		}
	}
	return nil
}
