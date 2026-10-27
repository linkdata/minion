package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/workspace"
)

var errAuditPublicationUnavailable = errors.New("audit publishing validator is unavailable")

// validateAuditForPublication checks received data with the publishing host's installed skill.
func (e *Orchestrator) validateAuditForPublication(ctx context.Context, job Job, files []workspace.File) (resultErr error) {
	configured := e.Runner
	if err := configured.ConfigureRole(job.Main); err != nil {
		return fmt.Errorf("%w: resolve audit skill: %w", errAuditPublicationUnavailable, err)
	}
	skill := configured.Codex.SecurityAuditSkill()
	if skill == "" {
		return fmt.Errorf("%w: publishing host has no installed security-audit skill", errAuditPublicationUnavailable)
	}
	root, err := workspace.CanonicalPath(e.WorkRoot)
	if err != nil {
		return fmt.Errorf("%w: %w", errAuditPublicationUnavailable, err)
	}
	validationDir, err := os.MkdirTemp(root, ".minion-audit-validation-")
	if err != nil {
		return fmt.Errorf("%w: create validation directory: %w", errAuditPublicationUnavailable, err)
	}
	defer func() {
		if err := os.RemoveAll(validationDir); err != nil {
			if resultErr == nil {
				resultErr = fmt.Errorf("%w: remove validation directory: %w", errAuditPublicationUnavailable, err)
			} else {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	tools, err := checkAuditTools(ctx, validationDir, skill)
	if err != nil {
		return fmt.Errorf("%w: audit prerequisites: %w", errAuditPublicationUnavailable, err)
	}
	if err := validateAuditFiles(ctx, files, validationDir, skill, tools, job.Identity.Repository, sourceIdentity(job), job.Source.Head); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w: audit validator execution: %w", errAuditPublicationUnavailable, err)
		}
		for _, validator := range auditValidators {
			script := filepath.Join(filepath.Dir(skill), validator.script)
			info, statErr := os.Stat(script)
			if statErr != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("%w: audit validator %q changed during validation: %w", errAuditPublicationUnavailable, script, errors.Join(err, statErr))
			}
		}
		return err
	}
	return nil
}
