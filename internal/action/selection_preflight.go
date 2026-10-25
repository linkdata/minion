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

// PreflightSelection checks one direct worker's frozen tools before it requests work.
func (r *Runner) PreflightSelection(ctx context.Context) (resultErr error) {
	if r == nil {
		return errors.New("worker runner is missing")
	}
	defer func() {
		if resultErr == nil {
			return
		}
		resultErr = redactActionError(resultErr, r.Codex.APIKey, r.Claude.APIKey)
		if message := httpFailureDiagnostic(resultErr.Error()); message != resultErr.Error() {
			resultErr = redactedActionError{cause: resultErr, message: message}
		}
	}()
	if r.selection == nil {
		return errors.New("worker runner has no frozen selection")
	}
	if r.WorkRoot == "" {
		return errors.New("worker work root is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	workRoot, err := workspace.CanonicalPath(r.WorkRoot)
	if err != nil {
		return fmt.Errorf("worker work root: %w", err)
	}
	configured := *r
	configured.Logger = nil
	var auditSkills []string
	for name, mapping := range r.selection.Commands {
		role := r.selection.Main
		role.Skills = mapping.Skills
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
			if !auditSelected || skill == "" {
				return fmt.Errorf("worker command %q has no selected security-audit skill", name)
			}
			auditSkills = append(auditSkills, skill)
		}
	}
	if r.selection.Critic != nil {
		if err := configured.ConfigureRole(*r.selection.Critic); err != nil {
			return fmt.Errorf("worker critic: %w", err)
		}
	}
	if len(auditSkills) != 0 {
		dir, err := os.MkdirTemp(workRoot, ".minion-audit-preflight-")
		if err != nil {
			return fmt.Errorf("audit prerequisites: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
		for _, skill := range auditSkills {
			if _, err := checkAuditTools(ctx, dir, skill); err != nil {
				return fmt.Errorf("audit prerequisites: %w", err)
			}
		}
	}
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
			if err := codexcli.CheckLogin(ctx, settings); err != nil {
				return fmt.Errorf("codex login: %w", err)
			}
			if err := codexcli.ValidateInstructionBoundary(ctx); err != nil {
				return fmt.Errorf("codex instruction boundary: %w", err)
			}
			if _, err := codexcli.Validate(ctx, settings); err != nil {
				return fmt.Errorf("codex settings: %w", err)
			}
		case "claude":
			settings := configured.Claude
			settings.Model, settings.Effort = role.Model, role.Effort
			if _, err := claudecli.Validate(ctx, settings); err != nil {
				return fmt.Errorf("claude settings: %w", err)
			}
		}
	}
	return nil
}
