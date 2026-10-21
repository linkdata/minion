package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/workspace"
)

var auditValidators = []struct{ script, artifact string }{
	{"validate-findings.cjs", "findings.json"},
	{"validate-coverage-ledger.cjs", "coverage-ledger.json"},
}

type auditTools struct {
	binary, permissionFlag string
}

func checkAuditTools(ctx context.Context, dir, skill string) (auditTools, error) {
	for _, validator := range auditValidators {
		script := filepath.Join(filepath.Dir(skill), validator.script)
		info, err := os.Stat(script)
		if err != nil {
			return auditTools{}, fmt.Errorf("audit validator %q: %w", script, err)
		}
		if !info.Mode().IsRegular() {
			return auditTools{}, fmt.Errorf("audit validator %q is not a regular file", script)
		}
	}
	binary, err := aicli.AbsoluteExecutable("node")
	if err != nil {
		return auditTools{}, fmt.Errorf("audit requires node: %w", err)
	}
	flag, err := auditNodePermissionFlag(ctx, dir, binary)
	return auditTools{binary: binary, permissionFlag: flag}, err
}

func readAuditArtifacts(outputDir string, partial bool, secrets ...string) ([]workspace.File, []Diagnostic, error) {
	var files []workspace.File
	var diagnostics []Diagnostic
	for _, name := range auditArtifacts {
		limit := int64(maxAuditArtifactBytes)
		if name == "REPORT.md" {
			limit = maxOutputBytes
		}
		data, err := workspace.ReadRegular(filepath.Join(outputDir, name), limit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			if strings.HasSuffix(name, ".json") {
				data, err = redactAuditJSON(data, secrets...)
			} else if (name == "REPORT.md" && partial) || utf8.Valid(data) {
				data = []byte(redactActionText(string(data), secrets...))
			} else {
				err = errors.New("audit Markdown is not valid UTF-8")
			}
			if err == nil && int64(len(data)) > limit {
				err = errors.New("redacted artifact exceeds byte limit")
			} else if err == nil && name == "REPORT.md" && !partial && (!publishableAnswer(string(data)) || strings.TrimSpace(string(data)) == "") {
				err = errors.New("audit REPORT.md is invalid or exceeds publication limit")
			}
		}
		if err != nil {
			if !partial {
				return nil, nil, fmt.Errorf("invalid audit %s: %w", name, err)
			}
			diagnostics = append(diagnostics, Diagnostic{Code: "artifact_unavailable", Message: "Incomplete audit artifact " + name + " could not be safely collected."})
			continue
		}
		files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
	}
	return files, diagnostics, nil
}

// Decode JSON escapes before redaction so consumers cannot reconstruct a configured key.
func redactAuditJSON(data []byte, secrets ...string) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("audit JSON is not valid UTF-8")
	}
	if !json.Valid(data) {
		return nil, errors.New("audit JSON is invalid")
	}
	if err := rejectDuplicateAuditJSONKeys(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	redacted, changed, err := redactAuditJSONValue(value, secrets...)
	if err != nil {
		return nil, err
	}
	if changed {
		var output bytes.Buffer
		encoder := json.NewEncoder(&output)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(redacted); err != nil {
			return nil, err
		}
		data = bytes.TrimSuffix(output.Bytes(), []byte("\n"))
	}
	final := string(data)
	if redactActionText(final, secrets...) != final {
		return nil, errors.New("audit JSON contains a configured key in source syntax")
	}
	return data, nil
}

func rejectDuplicateAuditJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := make(map[string]bool)
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok {
					return errors.New("audit JSON object key is invalid")
				}
				folded := workspace.FoldPath(key)
				if seen[folded] {
					return errors.New("audit JSON has duplicate object keys")
				}
				seen[folded] = true
				if err := visit(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := visit(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	return visit()
}

func redactAuditJSONValue(value any, secrets ...string) (any, bool, error) {
	switch value := value.(type) {
	case string:
		redacted := redactActionText(value, secrets...)
		return redacted, redacted != value, nil
	case json.Number, bool, nil:
		literal := fmt.Sprint(value)
		if value == nil {
			literal = "null"
		}
		if redacted := redactActionText(literal, secrets...); redacted != literal {
			return redacted, true, nil
		}
	case []any:
		changed := false
		for i := range value {
			redacted, dirty, err := redactAuditJSONValue(value[i], secrets...)
			if err != nil {
				return nil, false, err
			}
			value[i] = redacted
			changed = changed || dirty
		}
		return value, changed, nil
	case map[string]any:
		redacted := make(map[string]any, len(value))
		changed := false
		for key, entry := range value {
			redactedKey := redactActionText(key, secrets...)
			if _, present := redacted[redactedKey]; present {
				return nil, false, errors.New("audit JSON keys collide after redaction")
			}
			redactedEntry, dirty, err := redactAuditJSONValue(entry, secrets...)
			if err != nil {
				return nil, false, err
			}
			redacted[redactedKey] = redactedEntry
			changed = changed || redactedKey != key || dirty
		}
		return redacted, changed, nil
	}
	return value, false, nil
}

func auditArtifactMayBeEmpty(name string) bool {
	return name == "FINDINGS-DETAIL.md" || name == "NEEDS-VALIDATION.md"
}

func validateAuditFiles(ctx context.Context, files []workspace.File, validationDir, skill string, tools auditTools, repo, target, head string) (string, error) {
	byPath := make(map[string][]byte, len(files))
	for _, file := range files {
		byPath[file.Path] = file.Data
	}
	for _, name := range auditArtifacts {
		data, present := byPath[name]
		if !present || len(data) == 0 && !auditArtifactMayBeEmpty(name) || name == "REPORT.md" && strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("audit %s is missing or empty", name)
		}
	}
	if _, err := validateAuditMetadata(byPath["run-metadata.json"], repo, target, head); err != nil {
		return "", err
	}
	for _, validator := range auditValidators {
		if err := os.WriteFile(filepath.Join(validationDir, validator.artifact), byPath[validator.artifact], 0600); err != nil {
			return "", fmt.Errorf("copy audit %s for validation: %w", validator.artifact, err)
		}
		script := filepath.Join(filepath.Dir(skill), validator.script)
		// #nosec G204 -- the installed skill path and validation directory are checked paths; arguments bypass a shell.
		cmd := auditNodeCommand(ctx, validationDir, tools.binary, tools.permissionFlag,
			"--allow-fs-read="+filepath.Dir(skill), "--allow-fs-read="+validationDir,
			script, filepath.Join(validationDir, validator.artifact))
		if err := processgroup.Run(cmd); err != nil {
			if cause := ctx.Err(); cause != nil && !errors.Is(err, cause) {
				err = errors.Join(cause, err)
			}
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
	if err := json.Unmarshal(byPath["coverage-ledger.json"], &units); err != nil || len(units) == 0 {
		return "", errors.New("audit coverage-ledger.json has no coverage units")
	}
	var findings []struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(byPath["findings.json"], &findings); err != nil {
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
				return errors.New("audit coverage-ledger.json has a candidate without final finding")
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
			return "", errors.New("audit findings.json has a finding without candidate")
		}
	}
	return string(byPath["REPORT.md"]), nil
}

func validateAuditMetadata(metadataBytes []byte, repo, target, head string) ([]string, error) {
	if err := rejectDuplicateAuditJSONKeys(metadataBytes); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("decode audit metadata: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadataBytes, &fields); err != nil {
		return nil, fmt.Errorf("decode audit metadata fields: %w", err)
	}
	for name := range fields {
		for _, canonical := range []string{"run_id", "repo", "target", "source_ref", "profile", "scope_paths", "budget", "selected_companion_files", "prior_run_paths", "shared_file_owners", "execution_policy", "run_status"} {
			if name != canonical && strings.EqualFold(name, canonical) {
				return nil, fmt.Errorf("audit metadata uses noncanonical key %q", name)
			}
		}
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
	return metadata.SelectedCompanionFiles, nil
}

func auditNodePermissionFlag(ctx context.Context, dir, binary string) (string, error) {
	// Prefer the stable flag; Node 20 requires the experimental spelling.
	for _, flag := range []string{"--permission", "--experimental-permission"} {
		// #nosec G204 -- the flags and probe program are fixed strings, passed without a shell.
		cmd := auditNodeCommand(ctx, dir, binary, flag, "-e", `if (!process.permission || process.permission.has('fs.read', '/etc/passwd')) process.exit(1)`)
		if err := processgroup.Run(cmd); err == nil {
			return flag, nil
		} else if errors.Is(err, processgroup.ErrDescendants) || errors.Is(err, processgroup.ErrUnconfirmed) || errors.Is(err, processgroup.ErrExhausted) {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return "", errors.New("node permission model is unavailable")
}

func auditNodeCommand(ctx context.Context, dir, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
	return cmd
}
