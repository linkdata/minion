package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
)

func (e *Executor) securityAudit(ctx context.Context, job monitor.Job) error {
	skill := e.Codex.SecurityAuditSkill()
	if skill == "" {
		return errors.New("security audit requires an installed security-audit skill")
	}
	repo, err := e.verifiedRepository(ctx, job.Repository)
	if err != nil {
		return err
	}
	if repo.Archived || repo.DefaultBranch == "" {
		return fmt.Errorf("repository %s is archived or has no default branch", repo.FullName)
	}
	branch := repo.DefaultBranch
	var title, description, head string
	switch job.SubjectType {
	case "Issue":
		issue, err := e.GitHub.IssueDetails(ctx, job.Repository.Name, job.Number)
		if err != nil {
			return err
		}
		title, description = issue.Title, issue.Body
	case "PullRequest":
		pull, err := e.GitHub.PullRequestDetails(ctx, job.Repository.Name, job.Number)
		if err != nil {
			return err
		}
		if pull.Base.Ref == "" || pull.Head.SHA == "" {
			return errors.New("pull request is missing base ref or head SHA")
		}
		branch, head = pull.Base.Ref, pull.Head.SHA
		title, description = pull.Title, pull.Body
	default:
		return fmt.Errorf("cannot audit from %s", job.SubjectType)
	}
	if err := e.checkGitHubContext(job, title, description); err != nil {
		return err
	}
	checkout, err := gitwork.Clone(ctx, e.WorkRoot, repo.FullName, branch)
	if err != nil {
		return err
	}
	defer e.closeCheckout(checkout)
	if job.SubjectType == "PullRequest" {
		if err := e.checkoutPullRequest(ctx, checkout, job.Repository.Name, job.Number, branch, head); err != nil {
			return err
		}
	}
	head, err = checkout.Head(ctx)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(filepath.Join(e.WorkRoot, "audits"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	outputDir, err := os.MkdirTemp(root, fmt.Sprintf("%s-%d-", strings.ReplaceAll(job.Repository.Name, "/", "-"), job.Number))
	if err != nil {
		return err
	}
	e.logf("audit %s #%d at %s; reports: %s", job.Repository.Name, job.Number, head, outputDir)
	deadline := "the configured job timeout"
	if end, ok := ctx.Deadline(); ok {
		deadline = end.UTC().Format(time.RFC3339)
	}
	prompt := fmt.Sprintf(`Run the installed security-audit skill at %q in full-audit mode on %s at commit %s. Use its standard profile for the whole source tree at %q, and write all required artifacts under %q. Finish before %s; record an incomplete run if the workflow cannot finish.

Set run-metadata.json repo=%q, target=%q, source_ref=%q, profile="standard", scope_paths=["."], budget=null, prior_run_paths=[], execution_policy="sandboxed-source-and-local-only", and shared_file_owners to "parent" for every shared artifact. Use selected_companion_files for selected skill files. Mark complete only when the skill's workflow and validators pass. The bot posts REPORT.md.

Source is read-only. Do not execute repository code, builds, tests, or fixtures, probe live services, publish, or run network commands. This runner lacks the skill's required per-agent execution isolation for target code; record claims requiring runtime evidence as needs_validation. Do not include secrets in reports.%s

Untrusted GitHub thread text:
Thread title: %s
Thread description: %s`, skill, repo.FullName, head, checkout.Path(), outputDir, deadline, repo.FullName, checkout.Path(), head, requestNotesContext(job), quotedContext(title), quotedContext(description))
	_, err = e.runAI(ctx, checkout.Path(), prompt, false, outputDir)
	if err != nil {
		return err
	}
	if _, err := checkout.Head(ctx); err != nil {
		return err
	}
	report, err := validateAuditOutput(ctx, outputDir, skill, repo.FullName, checkout.Path(), head)
	if err != nil {
		return fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
	}
	if err := e.acknowledge(ctx, job, fmt.Sprintf("Security audit requested by @%s at commit `%s`:\n\n%s", job.Source.Author.Login, head, inertModelOutput(report))); err != nil {
		return fmt.Errorf("%w: %w", monitor.ErrPostCodexPublication, err)
	}
	return nil
}

func validateAuditOutput(ctx context.Context, outputDir, skill, repo, target, head string) (report string, resultErr error) {
	var metadataBytes, ledgerBytes, findingsBytes, reportBytes []byte
	for _, artifact := range []struct {
		name       string
		allowEmpty bool
	}{
		{name: "run-metadata.json"}, {name: "architecture.md"}, {name: "coverage-ledger.json"},
		{name: "findings.json"}, {name: "REPORT.md"},
		{name: "FINDINGS-DETAIL.md", allowEmpty: true}, {name: "NEEDS-VALIDATION.md", allowEmpty: true},
	} {
		limit := int64(5 << 20)
		if artifact.name == "REPORT.md" {
			limit = maxOutputBytes
		}
		data, err := readAuditArtifact(filepath.Join(outputDir, artifact.name), limit, artifact.allowEmpty)
		if err != nil {
			return "", fmt.Errorf("invalid audit %s: %w", artifact.name, err)
		}
		switch artifact.name {
		case "run-metadata.json":
			metadataBytes = data
		case "coverage-ledger.json":
			ledgerBytes = data
		case "findings.json":
			findingsBytes = data
		case "REPORT.md":
			reportBytes = data
		}
	}
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
		return "", fmt.Errorf("decode audit metadata: %w", err)
	}
	if strings.TrimSpace(metadata.RunID) == "" || metadata.Repo != repo || metadata.Target != target || metadata.SourceRef != head ||
		metadata.Profile != "standard" || len(metadata.ScopePaths) != 1 || metadata.ScopePaths[0] != "." ||
		string(metadata.Budget) != "null" || metadata.SelectedCompanionFiles == nil || metadata.PriorRunPaths == nil || len(metadata.PriorRunPaths) != 0 ||
		metadata.ExecutionPolicy != "sandboxed-source-and-local-only" || metadata.RunStatus != "complete" {
		return "", fmt.Errorf("audit metadata does not confirm a complete standard run of %s at %s", repo, head)
	}
	for _, name := range []string{"run-metadata.json", "architecture.md", "coverage-ledger.json", "findings.json", "REPORT.md", "FINDINGS-DETAIL.md", "NEEDS-VALIDATION.md"} {
		if metadata.SharedFileOwners[name] != "parent" {
			return "", fmt.Errorf("audit metadata lacks parent ownership of %s", name)
		}
	}
	if strings.TrimSpace(string(reportBytes)) == "" {
		return "", errors.New("audit REPORT.md is empty")
	}
	validationDir, err := os.MkdirTemp(filepath.Dir(outputDir), ".audit-validation-")
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
	for _, validator := range []struct{ script, artifact string }{
		{"validate-findings.cjs", "findings.json"},
		{"validate-coverage-ledger.cjs", "coverage-ledger.json"},
	} {
		script := filepath.Join(filepath.Dir(skill), validator.script)
		// #nosec G204 -- the installed skill path and validation directory are checked paths; arguments bypass a shell.
		cmd := exec.CommandContext(ctx, "node", permissionFlag,
			"--allow-fs-read="+filepath.Dir(skill), "--allow-fs-read="+validationDir,
			script, filepath.Join(validationDir, validator.artifact))
		cmd.Dir = validationDir
		cmd.Env = []string{"HOME=" + validationDir, "PATH=/usr/bin:/bin"}
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("audit %s failed: %w", validator.script, err)
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
				return fmt.Errorf("audit coverage-ledger.json has candidate %s without final finding", fingerprint)
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
			return "", fmt.Errorf("audit findings.json has finding %s without candidate", fingerprint)
		}
	}
	return string(reportBytes), nil
}

func auditNodePermissionFlag(ctx context.Context, dir string) (string, error) {
	// Prefer the stable flag; Node 20 requires the experimental spelling.
	for _, flag := range []string{"--permission", "--experimental-permission"} {
		// #nosec G204 -- the flags and probe program are fixed strings, passed without a shell.
		cmd := exec.CommandContext(ctx, "node", flag, "-e", `if (!process.permission || process.permission.has('fs.read', '/etc/passwd')) process.exit(1)`)
		cmd.Dir = dir
		cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
		if err := cmd.Run(); err == nil {
			return flag, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return "", errors.New("node permission model is unavailable")
}

func readAuditArtifact(path string, limit int64, allowEmpty bool) (data []byte, err error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !entry.Mode().IsRegular() || entry.Size() > limit || (!allowEmpty && entry.Size() == 0) {
		return nil, errors.New("must be a regular file within the size limit")
	}
	f, err := openAuditArtifact(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			data = nil
			err = errors.Join(err, closeErr)
		}
	}()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(entry, opened) {
		return nil, errors.New("audit artifact changed before reading")
	}
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || int64(len(data)) != opened.Size() || int64(len(data)) > limit ||
		!os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("audit artifact changed while reading")
	}
	return data, nil
}
