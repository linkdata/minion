package action

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/claudecli"
	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	preflightRunnerUnavailable          = "worker runner unavailable"
	preflightSelectionUnavailable       = "frozen worker selection unavailable"
	preflightRolePairUnavailable        = nativeRolePairUnverified
	preflightWorkRootUnavailable        = "worker work root unavailable"
	preflightCanceled                   = "worker preflight canceled"
	preflightWorkRootInvalid            = "worker work root invalid"
	preflightEvidenceDirsUnsupported    = "worker evidence directories unsupported"
	preflightCommandSkillUnavailable    = "selected command skill unavailable"
	preflightAuditSkillUnavailable      = "selected audit skill unavailable"
	preflightAuditToolchainUnavailable  = "audit toolchain unavailable"
	preflightGitReadinessUnavailable    = "Git readiness unavailable"
	preflightCodexLoginUnavailable      = "Codex login unavailable"
	preflightCodexBoundaryUnavailable   = "Codex instruction boundary unavailable"
	preflightCodexInvocationUnsupported = "Codex model or invocation unsupported"
	preflightClaudeSettingsUnavailable  = "Claude settings check failed"
)

func validPreflightMessage(message string) bool {
	switch message {
	case preflightRunnerUnavailable, preflightSelectionUnavailable, preflightRolePairUnavailable, preflightWorkRootUnavailable,
		preflightCanceled, preflightWorkRootInvalid, preflightEvidenceDirsUnsupported,
		preflightCommandSkillUnavailable, preflightAuditSkillUnavailable,
		preflightAuditToolchainUnavailable, preflightGitReadinessUnavailable,
		preflightCodexLoginUnavailable, preflightCodexBoundaryUnavailable,
		preflightCodexInvocationUnsupported, preflightClaudeSettingsUnavailable:
		return true
	}
	return false
}

type preflightError struct {
	message string
	cause   error
}

func (e preflightError) Error() string { return e.message }
func (e preflightError) Unwrap() error { return e.cause }

// PreflightSelection checks one direct worker's frozen tools before it requests work.
func (r *Runner) PreflightSelection(ctx context.Context) (resultErr error) {
	message := preflightRunnerUnavailable
	defer func() {
		if resultErr != nil {
			if ctx.Err() != nil && (errors.Is(resultErr, context.Canceled) || errors.Is(resultErr, context.DeadlineExceeded)) {
				message = preflightCanceled
			}
			resultErr = preflightError{message: message, cause: resultErr}
		}
	}()
	if r == nil {
		return errors.New("worker runner is missing")
	}
	message = preflightSelectionUnavailable
	if r.selection == nil {
		return errors.New("worker runner has no frozen selection")
	}
	if r.selection.Critic != nil {
		if err := CheckNativeRolePair(r.selection.Main.Provider, r.selection.Critic.Provider); err != nil && r.runModel == nil {
			message = preflightRolePairUnavailable
			return err
		}
	}
	message = preflightWorkRootUnavailable
	if r.WorkRoot == "" {
		return errors.New("worker work root is required")
	}
	message = preflightCanceled
	if err := ctx.Err(); err != nil {
		return err
	}
	message = preflightWorkRootInvalid
	workRoot, err := workspace.CanonicalPath(r.WorkRoot)
	if err != nil {
		return fmt.Errorf("worker work root: %w", err)
	}
	if err := workspace.CheckRoot(workRoot); err != nil {
		return fmt.Errorf("worker work root: %w", err)
	}
	message = preflightEvidenceDirsUnsupported
	if len(r.Codex.ReadOnlyDirs) != 0 || len(r.Claude.ReadOnlyDirs) != 0 {
		return errUnsupportedEvidenceDirs
	}
	configured := *r
	configured.Logger = nil
	var auditSkill string
	for name, mapping := range r.selection.Commands {
		role := r.selection.Main
		role.Skills = mapping.Skills
		message = preflightCommandSkillUnavailable
		if err := configured.ConfigureRole(role); err != nil {
			return fmt.Errorf("worker command %q: %w", name, err)
		}
		if mapping.Action == string(command.SecurityAudit) {
			auditSelected := false
			for _, source := range mapping.Skills {
				name, _ := skills.Name(source) // NewSelectedRunner already validated sources.
				if name == "security-audit" {
					auditSelected = true
					break
				}
			}
			skill := configured.Codex.SecurityAuditSkill()
			message = preflightAuditSkillUnavailable
			if !auditSelected || skill == "" {
				return fmt.Errorf("worker command %q has no selected security-audit skill", name)
			}
			auditSkill = skill
		}
	}
	if auditSkill != "" {
		message = preflightAuditToolchainUnavailable
		dir, err := os.MkdirTemp(workRoot, ".minion-audit-preflight-")
		if err != nil {
			return fmt.Errorf("audit prerequisites: %w", err)
		}
		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				if resultErr == nil {
					message = preflightAuditToolchainUnavailable
				}
				resultErr = errors.Join(resultErr, err)
			}
		}()
		if _, err := checkAuditTools(ctx, dir, auditSkill); err != nil {
			return fmt.Errorf("audit prerequisites: %w", err)
		}
	}
	message = preflightGitReadinessUnavailable
	if err := gitwork.Check(ctx, workRoot); err != nil {
		return fmt.Errorf("git readiness: %w", err)
	}
	message, err = checkOfferedRolesCLI(ctx, &configured, offeredPreflightRoles(r.selection.Main, r.selection.MainChoices, r.selection.Critic, r.selection.CriticChoices))
	if err != nil {
		return err
	}
	return nil
}

func offeredPreflightRoles(main RoleSettings, mainChoices []config.RoleModelConfig, critic *RoleSettings, criticChoices []config.RoleModelConfig) []RoleSettings {
	roles := []RoleSettings{main}
	for _, choice := range mainChoices {
		if choice.Model == main.Model && choice.Effort == main.Effort {
			continue
		}
		role := main
		role.Model, role.Effort = choice.Model, choice.Effort
		roles = append(roles, role)
	}
	if critic != nil {
		roles = append(roles, *critic)
		for _, choice := range criticChoices {
			if choice.Model == critic.Model && choice.Effort == critic.Effort {
				continue
			}
			role := *critic
			role.Model, role.Effort = choice.Model, choice.Effort
			roles = append(roles, role)
		}
	}
	return roles
}

func checkCodexPrerequisites(ctx context.Context, settings config.CodexConfig) (string, error) {
	if err := codexcli.CheckLogin(ctx, settings); err != nil {
		return preflightCodexLoginUnavailable, fmt.Errorf("codex login: %w", err)
	}
	if err := codexcli.ValidateInstructionBoundary(ctx); err != nil {
		return preflightCodexBoundaryUnavailable, fmt.Errorf("codex instruction boundary: %w", err)
	}
	return "", nil
}

func checkOfferedRolesCLI(ctx context.Context, configured *Runner, roles []RoleSettings) (string, error) {
	codexChecked := false
	checked := make(map[[3]string]bool, len(roles))
	for _, role := range roles {
		pair := [3]string{role.Provider, role.Model, role.Effort}
		if checked[pair] {
			continue
		}
		checked[pair] = true
		if role.Provider == "codex" && !codexChecked {
			if message, err := checkCodexPrerequisites(ctx, configured.Codex); err != nil {
				return message, err
			}
			codexChecked = true
		}
		if message, err := checkSelectedRoleChoiceCLI(ctx, configured, role); err != nil {
			return message, err
		}
	}
	return "", nil
}

func checkSelectedRoleChoiceCLI(ctx context.Context, configured *Runner, role RoleSettings) (string, error) {
	switch role.Provider {
	case "codex":
		settings := configured.Codex
		settings.Model, settings.ReasoningEffort = role.Model, role.Effort
		validated, err := codexcli.Validate(ctx, settings)
		if err != nil {
			return preflightCodexInvocationUnsupported, fmt.Errorf("codex settings: %w", err)
		}
		if validated.Model != role.Model || validated.ReasoningEffort != role.Effort {
			return preflightCodexInvocationUnsupported, errors.New("selected Codex model or effort was not resolved at startup")
		}
	case "claude":
		if role.Effort == "" {
			return preflightClaudeSettingsUnavailable, errors.New("selected Claude effort is not explicit")
		}
		settings := configured.Claude
		settings.Model, settings.Effort = role.Model, role.Effort
		validated, err := claudecli.Validate(ctx, settings)
		if err != nil {
			return preflightClaudeSettingsUnavailable, fmt.Errorf("claude settings: %w", err)
		}
		if validated.Effort != role.Effort || validated.Model != role.Model {
			return preflightClaudeSettingsUnavailable, errors.New("selected Claude model or effort was not resolved before dispatch")
		}
	default:
		return preflightSelectionUnavailable, errors.New("unsupported action provider")
	}
	return "", nil
}
