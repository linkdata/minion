package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/linkdata/minion/internal/workspace"
)

var errAuditPublicationUnavailable = errors.New("audit publishing validator is unavailable")
var removeAuditValidationDir = os.RemoveAll

// validateAuditForPublication checks received data with the publishing host's installed skill.
func (e *Orchestrator) validateAuditForPublication(ctx context.Context, job Job, files []workspace.File) error {
	byPath, err := validateAuditArtifactInput(files, job.Identity.Repository, sourceIdentity(job), job.Source.Head)
	if err != nil {
		return err
	}
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
	if err := runAuditValidators(ctx, byPath, validationDir, skill, tools); err != nil {
		if auditValidationUnavailable(err) {
			return fmt.Errorf("%w: audit validator execution: %w", errAuditPublicationUnavailable, err)
		}
		if scriptErr := checkAuditValidatorScripts(skill); scriptErr != nil {
			return fmt.Errorf("%w: audit validator unavailable after execution failure: %w", errAuditPublicationUnavailable, errors.Join(err, scriptErr))
		}
		return err
	}
	return nil
}

func auditValidationUnavailable(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !auditValidationUnavailable(cause) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return auditValidationUnavailable(wrapped)
	}
	var exit *exec.ExitError
	if errors.Is(err, os.ErrNotExist) || retryableHostIO(err) {
		return true
	}
	if !errors.As(err, &exit) || exit.ProcessState == nil {
		return false
	}
	code := exit.ProcessState.ExitCode()
	return code < 0 || runtime.GOOS == "windows" && uint32(code)&0xc0000000 == 0xc0000000
}
