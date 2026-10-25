package action

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/claudecli"
	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	preflightRunnerUnavailable          = "worker runner unavailable"
	preflightSelectionUnavailable       = "frozen worker selection unavailable"
	preflightWorkRootUnavailable        = "worker work root unavailable"
	preflightCanceled                   = "worker preflight canceled"
	preflightWorkRootInvalid            = "worker work root invalid"
	preflightCommandSkillUnavailable    = "selected command skill unavailable"
	preflightCriticSkillUnavailable     = "selected critic skill unavailable"
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
	case preflightRunnerUnavailable, preflightSelectionUnavailable, preflightWorkRootUnavailable,
		preflightCanceled, preflightWorkRootInvalid, preflightCommandSkillUnavailable,
		preflightCriticSkillUnavailable, preflightAuditSkillUnavailable,
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
			if errors.Is(resultErr, context.Canceled) || errors.Is(resultErr, context.DeadlineExceeded) {
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
	if r.selection.Critic != nil {
		message = preflightCriticSkillUnavailable
		if err := configured.ConfigureRole(*r.selection.Critic); err != nil {
			return fmt.Errorf("worker critic: %w", err)
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
	roles := []RoleSettings{r.selection.Main}
	if r.selection.Critic != nil {
		roles = append(roles, *r.selection.Critic)
	}
	for _, role := range roles {
		switch role.Provider {
		case "codex":
			settings := configured.Codex
			settings.Model, settings.ReasoningEffort = role.Model, role.Effort
			message = preflightCodexLoginUnavailable
			if err := codexcli.CheckLogin(ctx, settings); err != nil {
				return fmt.Errorf("codex login: %w", err)
			}
			message = preflightCodexBoundaryUnavailable
			if err := codexcli.ValidateInstructionBoundary(ctx); err != nil {
				return fmt.Errorf("codex instruction boundary: %w", err)
			}
			message = preflightCodexInvocationUnsupported
			if _, err := codexcli.Validate(ctx, settings); err != nil {
				return fmt.Errorf("codex settings: %w", err)
			}
		case "claude":
			settings := configured.Claude
			settings.Model, settings.Effort = role.Model, role.Effort
			message = preflightClaudeSettingsUnavailable
			if _, err := claudecli.Validate(ctx, settings); err != nil {
				return fmt.Errorf("claude settings: %w", err)
			}
		}
	}
	return nil
}
