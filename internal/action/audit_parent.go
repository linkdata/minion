package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/linkdata/minion/internal/workspace"
)

var errAuditPublicationUnavailable = errors.New("audit publishing validator is unavailable")
var removeAuditValidationDir = os.RemoveAll

// validateAuditForPublication checks received data with the publishing host's installed skill.
func (e *Orchestrator) validateAuditForPublication(ctx context.Context, job Job, files []workspace.File) error {
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
		if err := removeAuditValidationDir(validationDir); err != nil {
			e.logf("audit validation scratch cleanup failed at %q: %v", validationDir, err)
		}
	}()
	tools, err := checkAuditTools(ctx, validationDir, skill)
	if err != nil {
		return fmt.Errorf("%w: audit prerequisites: %w", errAuditPublicationUnavailable, err)
	}
	if err := validateAuditFiles(ctx, files, validationDir, skill, tools, job.Identity.Repository, sourceIdentity(job), job.Source.Head); err != nil {
		if auditValidationUnavailable(err) {
			return fmt.Errorf("%w: audit validator execution: %w", errAuditPublicationUnavailable, err)
		}
		if scriptErr := checkAuditValidatorScripts(skill); scriptErr != nil {
			return fmt.Errorf("%w: audit validator changed during validation: %w", errAuditPublicationUnavailable, errors.Join(err, scriptErr))
		}
		return err
	}
	return nil
}

func auditValidationUnavailable(err error) bool {
	var exit *exec.ExitError
	return errors.Is(err, os.ErrNotExist) || retryableRetentionIO(err) ||
		errors.As(err, &exit) && exit.ProcessState != nil && exit.ProcessState.ExitCode() < 0
}
