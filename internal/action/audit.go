package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/workspace"
)

type auditReadMode uint8

const (
	auditForReview auditReadMode = iota
	auditIncomplete
)

var auditValidators = []struct{ script, artifact string }{
	{"validate-findings.cjs", "findings.json"},
	{"validate-coverage-ledger.cjs", "coverage-ledger.json"},
}

func checkAuditTools(ctx context.Context, dir, skill string) error {
	for _, validator := range auditValidators {
		script := filepath.Join(filepath.Dir(skill), validator.script)
		info, err := os.Stat(script)
		if err != nil {
			return fmt.Errorf("audit validator %q: %w", script, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("audit validator %q is not a regular file", script)
		}
	}
	_, err := auditNodePermissionFlag(ctx, dir)
	return err
}

func readAuditArtifacts(outputDir string, mode auditReadMode) ([]workspace.File, []Diagnostic, error) {
	var files []workspace.File
	var diagnostics []Diagnostic
	for _, name := range auditArtifacts {
		data, err := workspace.ReadRegular(filepath.Join(outputDir, name), auditArtifactLimit(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && mode == auditForReview && name == "REPORT.md" && !modelOutputFits(string(data)) {
			err = errors.New("quoted audit report exceeds publication limit")
		}
		if err != nil {
			if mode != auditIncomplete {
				return nil, nil, fmt.Errorf("invalid audit %s: %w", name, err)
			}
			diagnostics = append(diagnostics, Diagnostic{Code: "artifact_unavailable", Message: "Incomplete audit artifact " + name + " could not be safely collected."})
			continue
		}
		files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
	}
	return files, diagnostics, nil
}

func auditArtifactMayBeEmpty(name string) bool {
	return name == "FINDINGS-DETAIL.md" || name == "NEEDS-VALIDATION.md"
}

func auditArtifactLimit(name string) int64 {
	if name == "REPORT.md" {
		return maxOutputBytes
	}
	return maxAuditArtifactBytes
}

func validateAuditFiles(ctx context.Context, files []workspace.File, validationRoot, skill, repo, target, head string) (report string, resultErr error) {
	var metadataBytes, ledgerBytes, findingsBytes, reportBytes []byte
	for _, file := range files {
		switch file.Path {
		case "run-metadata.json":
			metadataBytes = file.Data
		case "coverage-ledger.json":
			ledgerBytes = file.Data
		case "findings.json":
			findingsBytes = file.Data
		case "REPORT.md":
			reportBytes = file.Data
		}
	}
	if _, err := validateAuditMetadata(metadataBytes, reportBytes, repo, target, head); err != nil {
		return "", err
	}
	for name, data := range map[string][]byte{"findings.json": findingsBytes, "coverage-ledger.json": ledgerBytes} {
		if len(data) == 0 {
			return "", fmt.Errorf("audit %s is missing or empty", name)
		}
	}
	validationDir, err := os.MkdirTemp(validationRoot, ".audit-validation-")
	if err != nil {
		return "", fmt.Errorf("create audit validation directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(validationDir); err != nil {
			report = ""
			resultErr = errors.Join(resultErr, fmt.Errorf("remove audit validation directory: %w", err))
		}
	}()
	for name, data := range map[string][]byte{"findings.json": findingsBytes, "coverage-ledger.json": ledgerBytes} {
		if err := os.WriteFile(filepath.Join(validationDir, name), data, 0600); err != nil {
			return "", fmt.Errorf("copy audit %s for validation: %w", name, err)
		}
	}
	permissionFlag, err := auditNodePermissionFlag(ctx, validationDir)
	if err != nil {
		return "", err
	}
	for _, validator := range auditValidators {
		script := filepath.Join(filepath.Dir(skill), validator.script)
		// #nosec G204 -- the installed skill path and validation directory are checked paths; arguments bypass a shell.
		cmd, err := auditNodeCommand(ctx, validationDir, permissionFlag,
			"--allow-fs-read="+filepath.Dir(skill), "--allow-fs-read="+validationDir,
			script, filepath.Join(validationDir, validator.artifact))
		if err != nil {
			return "", err
		}
		stderr := bounded.Tail{Limit: 8 << 10}
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("audit %s failed: %w: %q", validator.script, err, stderr.String())
		}
	}
	var units []struct {
		Status             string   `json:"status"`
		ResultFingerprints []string `json:"result_fingerprints"`
		Unresolved         []string `json:"unresolved"`
		Attempts           []struct {
			Status             string   `json:"status"`
			ResultFingerprints []string `json:"result_fingerprints"`
			Unresolved         []string `json:"unresolved"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(ledgerBytes, &units); err != nil || len(units) == 0 {
		return "", errors.New("audit coverage-ledger.json has no coverage units")
	}
	var findings []struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(findingsBytes, &findings); err != nil {
		return "", fmt.Errorf("decode audit findings: %w", err)
	}
	final := make(map[string]bool, len(findings))
	for _, finding := range findings {
		final[finding.Fingerprint] = true
	}
	candidates := make(map[string]bool)
	checkCandidate := func(fingerprints, unresolved []string) error {
		if len(fingerprints) == 0 {
			return errors.New("audit coverage-ledger.json has candidate without fingerprint")
		}
		for _, reason := range unresolved {
			if strings.Contains(reason, "validation_budget_exhausted") {
				return errors.New("audit coverage-ledger.json has unvalidated candidate")
			}
		}
		for _, fingerprint := range fingerprints {
			candidates[fingerprint] = true
			if !final[fingerprint] {
				return fmt.Errorf("audit coverage-ledger.json has candidate %q without final finding", fingerprint)
			}
		}
		return nil
	}
	for _, unit := range units {
		if unit.Status == "planned" || unit.Status == "in_progress" || unit.Status == "deferred" {
			return "", fmt.Errorf("audit coverage-ledger.json has unfinished %s unit", unit.Status)
		}
		for _, attempt := range unit.Attempts {
			if attempt.Status == "candidate" {
				if err := checkCandidate(attempt.ResultFingerprints, attempt.Unresolved); err != nil {
					return "", err
				}
			}
		}
		if unit.Status == "candidate" {
			if err := checkCandidate(unit.ResultFingerprints, unit.Unresolved); err != nil {
				return "", err
			}
		}
	}
	for fingerprint := range final {
		if !candidates[fingerprint] {
			return "", fmt.Errorf("audit findings.json has finding %q without candidate", fingerprint)
		}
	}
	return string(reportBytes), nil
}

func validateAuditMetadata(metadataBytes, reportBytes []byte, repo, target, head string) ([]string, error) {
	var metadata struct {
		RunID                  string            `json:"run_id"`
		Repo                   string            `json:"repo"`
		Target                 string            `json:"target"`
		SourceRef              string            `json:"source_ref"`
		Profile                string            `json:"profile"`
		ScopePaths             []string          `json:"scope_paths"`
		Budget                 json.RawMessage   `json:"budget"`
		SelectedCompanionFiles []string          `json:"selected_companion_files"`
		PriorRunPaths          []string          `json:"prior_run_paths"`
		SharedFileOwners       map[string]string `json:"shared_file_owners"`
		ExecutionPolicy        string            `json:"execution_policy"`
		RunStatus              string            `json:"run_status"`
	}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("decode audit metadata: %w", err)
	}
	if strings.TrimSpace(metadata.RunID) == "" || metadata.Repo != repo || metadata.Target != target || metadata.SourceRef != head ||
		metadata.Profile != "standard" || len(metadata.ScopePaths) != 1 || metadata.ScopePaths[0] != "." ||
		string(metadata.Budget) != "null" || metadata.SelectedCompanionFiles == nil || metadata.PriorRunPaths == nil || len(metadata.PriorRunPaths) != 0 ||
		metadata.ExecutionPolicy != "sandboxed-source-and-local-only" || metadata.RunStatus != "complete" {
		return nil, fmt.Errorf("audit metadata does not confirm a complete standard run of %s at %s", repo, head)
	}
	for _, name := range auditArtifacts {
		if metadata.SharedFileOwners[name] != "parent" {
			return nil, fmt.Errorf("audit metadata lacks parent ownership of %s", name)
		}
	}
	if strings.TrimSpace(string(reportBytes)) == "" {
		return nil, errors.New("audit REPORT.md is empty")
	}
	return metadata.SelectedCompanionFiles, nil
}

func auditNodePermissionFlag(ctx context.Context, dir string) (string, error) {
	// Prefer the stable flag; Node 20 requires the experimental spelling.
	for _, flag := range []string{"--permission", "--experimental-permission"} {
		// #nosec G204 -- the flags and probe program are fixed strings, passed without a shell.
		cmd, err := auditNodeCommand(ctx, dir, flag, "-e", `if (!process.permission || process.permission.has('fs.read', '/etc/passwd')) process.exit(1)`)
		if err != nil {
			return "", err
		}
		if err := cmd.Run(); err == nil {
			return flag, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return "", errors.New("node permission model is unavailable")
}

func auditNodeCommand(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	binary, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("audit requires node: %w", err)
	}
	if !filepath.IsAbs(binary) {
		return nil, errors.New("audit node binary must have an absolute path")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
	return cmd, nil
}
