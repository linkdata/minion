package action

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

const ProtocolVersion = 1

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
const maxAuditArtifactsBytes = 7 * maxAuditArtifactBytes

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
	RuntimeBudget   time.Duration      `json:"runtime_budget_ns"`
}

type CallUsage struct {
	Provider    string       `json:"provider"`
	Role        string       `json:"role"`
	Round       int          `json:"round"`
	ReviewPhase string       `json:"review_phase,omitempty"`
	Model       string       `json:"model,omitempty"`
	Effort      string       `json:"effort,omitempty"`
	Tokens      usage.Tokens `json:"tokens"`
	Complete    bool         `json:"complete"`
}

type Diagnostic struct {
	Code    string `json:"code"`
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
	Diagnostics []Diagnostic       `json:"diagnostics,omitempty"`
}

type StatusState string
type StatusPhase string

const (
	StatusReady       StatusState = "ready"
	StatusRunning     StatusState = "running"
	StatusStopping    StatusState = "stopping"
	StatusCompleted   StatusState = "completed"
	StatusFailed      StatusState = "failed"
	StatusUnavailable StatusState = "unavailable"
	PhasePreparing    StatusPhase = "preparing"
	PhaseMain         StatusPhase = "main"
	PhaseCritic       StatusPhase = "critic"
	PhaseCollecting   StatusPhase = "collecting"
	PhaseUploading    StatusPhase = "uploading"
	PhaseCleanup      StatusPhase = "cleanup"
	PhaseFinished     StatusPhase = "finished"
)

type Status struct {
	Version          int           `json:"version"`
	Identity         Identity      `json:"identity"`
	State            StatusState   `json:"state"`
	Phase            StatusPhase   `json:"phase"`
	Round            int           `json:"round"`
	ReviewPhase      string        `json:"review_phase,omitempty"`
	ProgressSequence uint64        `json:"progress_sequence"`
	LogSequence      uint64        `json:"log_sequence"`
	ProgressAge      time.Duration `json:"progress_age_ns"`
	Diagnostics      []Diagnostic  `json:"diagnostics,omitempty"`
}

func (id Identity) validate() error {
	for _, value := range []string{id.JobID, id.AttemptID, id.ExecutorID, id.Requester} {
		if !boundedText(value, 128) || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("action identity is missing or invalid")
		}
	}
	parts := strings.Split(id.Repository, "/")
	if len(parts) != 2 || !identitySegment(parts[0]) || !identitySegment(parts[1]) || id.RepositoryID <= 0 || id.OwnerID <= 0 || id.RequesterID <= 0 || id.Number <= 0 || (id.SubjectType != "Issue" && id.SubjectType != "PullRequest") {
		return errors.New("action repository or requester identity is invalid")
	}
	return nil
}

func identitySegment(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 100 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-", c) {
			return false
		}
	}
	return true
}

func boundedText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func (r RoleSettings) validate() error {
	if r.Provider != "codex" && r.Provider != "claude" {
		return errors.New("unsupported action provider")
	}
	if !boundedText(r.Model, 128) || !boundedText(r.Effort, 128) || strings.ContainsAny(r.Model+r.Effort, "\r\n") || len(r.Skills)+len(r.AlwaysOnSkills) > 64 {
		return errors.New("invalid action model or skills")
	}
	for _, source := range append(append([]string(nil), r.Skills...), r.AlwaysOnSkills...) {
		if len(source) > 4096 {
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
	if !identitySegment(j.Command) {
		return errors.New("invalid action command")
	}
	if j.RuntimeBudget <= 0 || j.RuntimeBudget > config.MaxJobTimeout {
		return errors.New("invalid action runtime budget")
	}
	switch j.Action {
	case command.Review:
		if j.Identity.SubjectType != "PullRequest" || j.Source.Base == "" {
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
	if !boundedText(j.Title, maxActionTitleBytes) || !boundedText(j.Description, workspace.MaxBytes) || !boundedText(j.Notes, workspace.MaxBytes) || len(j.Title)+len(j.Description)+len(j.Notes) > workspace.MaxBytes {
		return errors.New("action text exceeds limits")
	}
	if err := j.Main.validate(); err != nil {
		return err
	}
	if !(config.CommandMapping{Action: string(j.Action)}).SupportsProvider(j.Main.Provider) {
		return errors.New("action is unsupported by main provider")
	}
	if j.Critic != nil {
		if err := j.Critic.validate(); err != nil {
			return err
		}
		if j.Critic.Provider == j.Main.Provider {
			return errors.New("main and critic currently require different providers")
		}
	}
	if j.CriticMaxRounds < 0 || j.CriticMaxRounds > config.MaxCriticMaxRounds {
		return errors.New("invalid critic round limit")
	}
	if err := workspace.ValidateSnapshot(j.Source); err != nil {
		return err
	}
	if j.Action == command.Fix {
		return workspace.ValidateFixPaths(j.Source.Files, j.Source.Omitted)
	}
	return nil
}

func (j Job) rounds() int { return cmp.Or(j.CriticMaxRounds, config.DefaultCriticMaxRounds) }

func sourceIdentity(j Job) string { return j.Identity.Repository + "@" + j.Source.Head }

func (r Result) Validate(j Job) error { return r.validate(j, true) }

func (r Result) validate(j Job, complete bool) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if r.Version != ProtocolVersion || r.Identity != j.Identity || r.SourceRef != j.Source.Head {
		return errors.New("action result version, identity or source differs from job")
	}
	if !boundedText(r.Answer, maxOutputBytes) || (complete && (j.Action != command.Fix || j.Critic != nil) && strings.TrimSpace(r.Answer) == "") {
		return errors.New("action answer exceeds limit or is invalid text")
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
	rounds := j.rounds()
	for i, call := range r.Calls {
		role := j.Main
		if call.Role == "critic" && j.Critic != nil {
			role = *j.Critic
		} else if call.Role != "main" {
			return errors.New("invalid model call role")
		}
		validOrder := i == 0 && call.Role == "main" && call.Round == 1 && call.ReviewPhase == ""
		if i > 0 && j.Critic != nil {
			previous := r.Calls[i-1]
			if call.Role == "main" {
				validOrder = previous.Role == "critic" && call.Round == previous.Round+1 && call.ReviewPhase == ""
			} else if previous.Role == "main" {
				phase := "verification"
				if call.Round == 1 {
					phase = "full"
				}
				validOrder = call.Round == previous.Round && call.ReviewPhase == phase
			} else {
				validOrder = previous.ReviewPhase == "verification" && call.Round == previous.Round && call.ReviewPhase == "full"
			}
		}
		if !validOrder || call.Round > rounds || call.Provider != role.Provider || call.Model != role.Model || call.Effort != role.Effort || call.Tokens.Input < 0 || call.Tokens.CachedInput < 0 || call.Tokens.Output < 0 || call.Tokens.CachedInput > call.Tokens.Input {
			return errors.New("invalid model call usage")
		}
	}
	if complete && j.Critic != nil {
		if len(r.Calls) == 0 {
			return errors.New("action result lacks a final full review")
		}
		last := r.Calls[len(r.Calls)-1]
		if last.Role != "critic" || last.ReviewPhase != "full" {
			return errors.New("action result lacks a final full review")
		}
	}
	if len(r.Diagnostics) > 100 {
		return errors.New("too many action diagnostics")
	}
	for _, diagnostic := range r.Diagnostics {
		if !identitySegment(diagnostic.Code) || !boundedText(diagnostic.Message, 8<<10) {
			return errors.New("invalid action diagnostic")
		}
	}
	return nil
}

func (j Job) validateChanges(changes []workspace.Change) error {
	if err := workspace.ValidateChanges(changes); err != nil {
		return err
	}
	omittedPaths := make([]string, len(j.Source.Omitted))
	for i, name := range j.Source.Omitted {
		omittedPaths[i] = workspace.FoldPath(name)
	}
	for _, change := range changes {
		name := workspace.FoldPath(change.Path)
		for i, path := range omittedPaths {
			if name == path || strings.HasPrefix(name, path+"/") || strings.HasPrefix(path, name+"/") {
				return fmt.Errorf("action change %q overlaps omitted source path %q", change.Path, j.Source.Omitted[i])
			}
		}
	}
	return workspace.ValidateChangedPaths(j.Source.Files, changes)
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
	total := 0
	for _, file := range files {
		allowed := false
		for _, name := range auditArtifacts {
			allowed = allowed || file.Path == name
		}
		_, duplicate := seen[file.Path]
		if !allowed || duplicate || file.Mode != workspace.Regular || len(file.Data) > maxAuditArtifactBytes {
			return errors.New("invalid action report artifact")
		}
		if complete && len(file.Data) == 0 && !auditArtifactMayBeEmpty(file.Path) {
			return fmt.Errorf("empty action report artifact %q", file.Path)
		}
		seen[file.Path] = file.Data
		total += len(file.Data)
	}
	if total > maxAuditArtifactsBytes || len(seen["REPORT.md"]) > maxOutputBytes {
		return errors.New("action report artifacts exceed limits")
	}
	if !complete {
		return nil
	}
	if err := validateAuditMetadata(seen["run-metadata.json"], seen["REPORT.md"], j.Identity.Repository, sourceIdentity(j), j.Source.Head); err != nil {
		return err
	}
	var metadata struct {
		SelectedCompanionFiles []string `json:"selected_companion_files"`
	}
	if err := json.Unmarshal(seen["run-metadata.json"], &metadata); err != nil {
		return err
	}
	for _, companion := range metadata.SelectedCompanionFiles {
		if !workspace.ValidPath(companion) {
			return errors.New("audit companion file path is not portable")
		}
	}
	return nil
}

func (s Status) Validate(j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if s.Version != ProtocolVersion || s.Identity != j.Identity {
		return errors.New("action status version or identity differs from job")
	}
	switch s.State {
	case StatusReady, StatusRunning, StatusStopping, StatusCompleted, StatusFailed, StatusUnavailable:
	default:
		return errors.New("invalid action status state")
	}
	switch s.Phase {
	case PhasePreparing, PhaseMain, PhaseCritic, PhaseCollecting, PhaseUploading, PhaseCleanup, PhaseFinished:
	default:
		return errors.New("invalid action status phase")
	}
	if s.Phase == PhaseCritic {
		if j.Critic == nil || s.Round < 1 || (s.ReviewPhase != "full" && s.ReviewPhase != "verification") || (s.ReviewPhase == "verification" && s.Round == 1) {
			return errors.New("invalid action status review phase")
		}
	} else if s.ReviewPhase != "" {
		return errors.New("unexpected action status review phase")
	}
	rounds := j.rounds()
	if j.Critic == nil {
		rounds = 1
	}
	if s.Round < 0 || s.Round > rounds || s.ProgressAge < 0 || len(s.Diagnostics) > 100 {
		return errors.New("invalid action status")
	}
	for _, diagnostic := range s.Diagnostics {
		if !identitySegment(diagnostic.Code) || !boundedText(diagnostic.Message, 8<<10) {
			return errors.New("invalid action status diagnostic")
		}
	}
	return nil
}
