package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/executil"
	"github.com/linkdata/minion/internal/workspace"
)

var errAuditPublicationUnavailable = errors.New("audit publishing validator is unavailable")
var removeAuditValidationDir = os.RemoveAll

const auditValidationDirPrefix = ".minion-audit-validation-"

// CheckAuditPublisher checks tools needed on the publishing host for configured remote audits.
func CheckAuditPublisher(ctx context.Context, workRoot string, cfg config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, httpControlTimeout)
	defer cancel()
	checked := make(map[string]bool)
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			mapping := cfg.Commands[name]
			if checked[name] || mapping.Action != string(command.SecurityAudit) ||
				!mapping.SupportsProvider(cfg.Main) || !mapping.SupportedOnHost() {
				continue
			}
			checked[name] = true
			configured := Runner{}
			role := RoleSettings{Provider: "codex", Skills: mapping.Skills, AlwaysOnSkills: cfg.AlwaysOnSkills}
			if err := configured.ConfigureRole(role); err != nil {
				return fmt.Errorf("audit publishing skill for %q: %w", name, err)
			}
			skill := configured.Codex.SecurityAuditSkill()
			if skill == "" {
				return fmt.Errorf("audit publishing skill for %q is not installed", name)
			}
			if err := checkAuditPublisherSkill(ctx, workRoot, skill); err != nil {
				return fmt.Errorf("audit publishing prerequisites for %q: %w", name, err)
			}
		}
	}
	return ctx.Err()
}

func checkAuditPublisherSkill(ctx context.Context, workRoot, skill string) (resultErr error) {
	dir, err := os.MkdirTemp(workRoot, ".minion-audit-preflight-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	tools, err := checkAuditTools(ctx, dir, skill)
	if err != nil {
		return err
	}
	return checkAuditCoverageModule(ctx, dir, skill, tools)
}

func auditValidationTempPattern(attemptID string) string {
	return auditValidationDirPrefix + attemptID + "+"
}

// validateAuditForPublication checks received data with the publishing host's installed skill.
func (e *Orchestrator) validateAuditForPublication(ctx context.Context, job Job, files []workspace.File) error {
	byPath, err := validateAuditArtifactInput(files, job.Identity.Repository, sourceIdentity(job), job.Source.Head)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: audit validators require POSIX file-open protections", errAuditPublicationUnavailable)
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
	validationDir, err := os.MkdirTemp(root, auditValidationTempPattern(job.Identity.AttemptID))
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
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		return fmt.Errorf("%w: audit prerequisites: %w", errAuditPublicationUnavailable, err)
	}
	if err := runAuditValidators(ctx, byPath, validationDir, skill, tools, nil); err != nil {
		if auditValidationUnavailable(err) {
			return fmt.Errorf("%w: audit validator execution: %w", errAuditPublicationUnavailable, err)
		}
		if scriptErr := checkAuditValidatorPrerequisites(ctx, validationDir, skill, tools); scriptErr != nil {
			return fmt.Errorf("%w: audit validator unavailable after execution failure: %w", errAuditPublicationUnavailable, errors.Join(err, scriptErr))
		}
		return err
	}
	return nil
}

func checkAuditValidatorPrerequisites(ctx context.Context, dir, skill string, tools auditTools) error {
	scripts, findingsSchema, err := checkAuditValidatorScripts(skill)
	if err != nil {
		return err
	}
	for _, script := range scripts {
		cmd := auditNodeCommand(ctx, dir, tools.binary, tools.permissionFlag,
			"--allow-fs-read="+filepath.Dir(skill), "--check", script)
		if err := executil.Run(cmd); err != nil {
			return fmt.Errorf("check audit validator %q syntax: %w", script, err)
		}
	}
	if findingsSchema {
		probe := filepath.Join(dir, "schema-probe.json")
		if err := os.WriteFile(probe, []byte("[]"), 0600); err != nil {
			return err
		}
		cmd := auditNodeCommand(ctx, dir, tools.binary, tools.permissionFlag,
			"--allow-fs-read="+filepath.Dir(skill), "--allow-fs-read="+dir,
			scripts[0], probe, "--source-only")
		checkErr := executil.Run(cmd)
		removeErr := os.Remove(probe)
		if checkErr != nil {
			return errors.Join(fmt.Errorf("check audit findings schema: %w", checkErr), removeErr)
		}
		if removeErr != nil {
			return fmt.Errorf("remove audit schema probe: %w", removeErr)
		}
	}
	return nil
}

func checkAuditCoverageModule(ctx context.Context, dir, skill string, tools auditTools) error {
	probe := filepath.Join(dir, "coverage-probe.json")
	if err := os.WriteFile(probe, []byte("[]"), 0600); err != nil {
		return err
	}
	cmd := auditNodeCommand(ctx, dir, tools.binary, tools.permissionFlag,
		"--allow-fs-read="+filepath.Dir(skill), "--allow-fs-read="+dir,
		filepath.Join(filepath.Dir(skill), auditValidators[1].script), probe)
	var diagnostic bounded.Tail
	cmd.Stderr = &diagnostic
	checkErr := executil.Run(cmd)
	removeErr := os.Remove(probe)
	if cause := ctx.Err(); cause != nil {
		return errors.Join(cause, removeErr)
	}
	if checkErr != nil && (strings.Contains(diagnostic.String(), "MODULE_NOT_FOUND") || strings.Contains(diagnostic.String(), "code: 'ENOENT'")) {
		return errors.Join(fmt.Errorf("check audit coverage validator dependencies: %w", checkErr), removeErr)
	}
	return removeErr
}

func auditValidationUnavailable(err error) bool {
	if errors.Is(err, errAuditValidatorRuntime) {
		return true
	}
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
