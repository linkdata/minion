package action

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/workspace"
)

// validateAuditForPublication checks received data with the publishing host's installed skill.
func (e *Orchestrator) validateAuditForPublication(ctx context.Context, job Job, files []workspace.File) (resultErr error) {
	configured := e.Runner
	if err := configured.ConfigureRole(job.Main); err != nil {
		return fmt.Errorf("resolve audit skill: %w", err)
	}
	skill := configured.Codex.SecurityAuditSkill()
	if skill == "" {
		return errors.New("publishing host has no installed security-audit skill")
	}
	root, err := workspace.CanonicalPath(e.WorkRoot)
	if err != nil {
		return err
	}
	validationDir, err := os.MkdirTemp(root, ".minion-audit-validation-")
	if err != nil {
		return err
	}
	defer func() {
		if !errors.Is(resultErr, processgroup.ErrScopeUnconfirmed) {
			resultErr = errors.Join(resultErr, os.RemoveAll(validationDir))
		}
	}()
	tools, err := checkAuditTools(ctx, validationDir, skill)
	if err != nil {
		return fmt.Errorf("audit prerequisites: %w", err)
	}
	return validateAuditFiles(ctx, files, validationDir, skill, tools, job.Identity.Repository, sourceIdentity(job), job.Source.Head)
}
