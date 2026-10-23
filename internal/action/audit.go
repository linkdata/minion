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
	if err := validateAuditCoverage(byPath["coverage-ledger.json"], byPath["findings.json"]); err != nil {
		return "", err
	}
	return string(byPath["REPORT.md"]), nil
}

type auditCoverageCheck struct {
	AgentID       string          `json:"agent_id"`
	ReviewedPaths []string        `json:"reviewed_paths"`
	Invariant     string          `json:"invariant"`
	Method        string          `json:"method"`
	Result        string          `json:"result"`
	Artifact      json.RawMessage `json:"artifact"`
}

type auditCoverageEvidence struct {
	Status             string               `json:"status"`
	AgentID            string               `json:"agent_id"`
	ReviewedPaths      []string             `json:"reviewed_paths"`
	LocalChecks        []auditCoverageCheck `json:"local_checks"`
	ResultFingerprints []string             `json:"result_fingerprints"`
	Unresolved         []string             `json:"unresolved"`
}

func validateAuditCoverage(ledgerBytes, findingsBytes []byte) error {
	if err := rejectDuplicateAuditJSONKeys(ledgerBytes); err != nil {
		return fmt.Errorf("audit coverage-ledger.json: %w", err)
	}
	if err := rejectDuplicateAuditJSONKeys(findingsBytes); err != nil {
		return fmt.Errorf("audit findings.json: %w", err)
	}
	var units []struct {
		auditCoverageEvidence
		CoverageID    string                  `json:"coverage_id"`
		CanonicalRefs map[string]string       `json:"canonical_refs"`
		Surface       string                  `json:"surface"`
		Boundary      string                  `json:"boundary"`
		Subsystem     string                  `json:"subsystem"`
		AttackClass   string                  `json:"attack_class"`
		StartingPaths []string                `json:"starting_paths"`
		Lifecycle     string                  `json:"lifecycle"`
		Attempts      []auditCoverageEvidence `json:"attempts"`
	}
	if err := json.Unmarshal(ledgerBytes, &units); err != nil || len(units) == 0 {
		return errors.New("audit coverage-ledger.json has no coverage units")
	}
	var findings []struct {
		Fingerprint string `json:"fingerprint"`
		Verdict     string `json:"verdict"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Trace       []struct {
			Kind, File, Scope, Description string
			Line                           int
		} `json:"trace"`
		Evidence []struct {
			File, Description string
			Line              int
		} `json:"evidence"`
	}
	if err := json.Unmarshal(findingsBytes, &findings); err != nil {
		return fmt.Errorf("decode audit findings: %w", err)
	}
	if findings == nil {
		return errors.New("audit findings.json is not an array")
	}
	final := make(map[string]bool, len(findings))
	for _, finding := range findings {
		if strings.TrimSpace(finding.Fingerprint) == "" {
			return errors.New("audit findings.json has an empty fingerprint")
		}
		if final[finding.Fingerprint] {
			return errors.New("audit findings.json has duplicate fingerprint")
		}
		if finding.Verdict != "confirmed" && finding.Verdict != "needs_validation" && finding.Verdict != "rejected" ||
			strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Description) == "" || len(finding.Trace) == 0 || len(finding.Evidence) == 0 {
			return errors.New("audit findings.json has an incomplete finding")
		}
		for _, trace := range finding.Trace {
			if (trace.Kind != "entrypoint" && trace.Kind != "propagation" && trace.Kind != "sink") || !workspace.ValidPath(trace.File) ||
				trace.Line < 1 || strings.TrimSpace(trace.Scope) == "" || strings.TrimSpace(trace.Description) == "" {
				return errors.New("audit findings.json has invalid trace evidence")
			}
		}
		for _, evidence := range finding.Evidence {
			if !workspace.ValidPath(evidence.File) || evidence.Line < 1 || strings.TrimSpace(evidence.Description) == "" {
				return errors.New("audit findings.json has invalid source evidence")
			}
		}
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
			if strings.TrimSpace(fingerprint) == "" {
				return errors.New("audit coverage-ledger.json has candidate without fingerprint")
			}
			candidates[fingerprint] = true
			if !final[fingerprint] {
				return errors.New("audit coverage-ledger.json has a candidate without final finding")
			}
		}
		return nil
	}
	seenIDs := make(map[string]bool, len(units))
	for _, unit := range units {
		switch unit.Status {
		case "planned", "in_progress", "deferred":
			return fmt.Errorf("audit coverage-ledger.json has unfinished %s unit", unit.Status)
		case "not_applicable", "out_of_scope", "covered", "candidate", "blocked":
		default:
			return errors.New("audit coverage-ledger.json has invalid unit status")
		}
		for _, attempt := range unit.Attempts {
			if attempt.Status != "covered" && attempt.Status != "candidate" && attempt.Status != "blocked" {
				return errors.New("audit coverage-ledger.json has invalid attempt status")
			}
			if attempt.Status == "candidate" {
				if err := checkCandidate(attempt.ResultFingerprints, attempt.Unresolved); err != nil {
					return err
				}
			}
		}
		if unit.Status == "candidate" {
			if err := checkCandidate(unit.ResultFingerprints, unit.Unresolved); err != nil {
				return err
			}
		}
		if unit.CoverageID == "" || seenIDs[unit.CoverageID] || len(unit.StartingPaths) == 0 ||
			strings.TrimSpace(unit.Surface) == "" || strings.TrimSpace(unit.Boundary) == "" ||
			strings.TrimSpace(unit.Subsystem) == "" || strings.TrimSpace(unit.AttackClass) == "" {
			return errors.New("audit coverage-ledger.json lacks unique coverage identity or starting paths")
		}
		seenIDs[unit.CoverageID] = true
		fields := []string{"surface", "boundary", "subsystem", "attack_class"}
		if _, hasLifecycle := unit.CanonicalRefs["lifecycle"]; hasLifecycle {
			fields = append(fields, "lifecycle")
			if strings.TrimSpace(unit.Lifecycle) == "" {
				return errors.New("audit coverage-ledger.json lacks lifecycle identity")
			}
		} else if unit.Lifecycle != "" {
			return errors.New("audit coverage-ledger.json lacks lifecycle reference")
		}
		if len(unit.CanonicalRefs) != len(fields) {
			return errors.New("audit coverage-ledger.json lacks canonical coverage references")
		}
		var id strings.Builder
		for i, field := range fields {
			ref := unit.CanonicalRefs[field]
			if ref == "" || strings.TrimSpace(ref) != ref {
				return errors.New("audit coverage-ledger.json lacks canonical coverage references")
			}
			if i > 0 {
				id.WriteString("::")
			}
			for _, b := range []byte(ref) {
				if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~' {
					id.WriteByte(b)
				} else {
					fmt.Fprintf(&id, "%%%02X", b)
				}
			}
		}
		if unit.CoverageID != id.String() {
			return errors.New("audit coverage-ledger.json has noncanonical coverage identity")
		}
		for _, path := range unit.StartingPaths {
			if !workspace.ValidPath(path) {
				return errors.New("audit coverage-ledger.json has invalid starting path")
			}
		}
		if err := validateAuditCoverageEvidence(unit.auditCoverageEvidence); err != nil {
			return err
		}
		for _, attempt := range unit.Attempts {
			if err := validateAuditCoverageEvidence(attempt); err != nil {
				return err
			}
		}
	}
	for fingerprint := range final {
		if !candidates[fingerprint] {
			return errors.New("audit findings.json has a finding without candidate")
		}
	}
	return nil
}

func validateAuditCoverageEvidence(e auditCoverageEvidence) error {
	for _, reason := range e.Unresolved {
		if strings.TrimSpace(reason) == "" {
			return errors.New("audit coverage-ledger.json has empty unresolved reason")
		}
	}
	switch e.Status {
	case "not_applicable", "out_of_scope":
		if e.AgentID != "" || len(e.ReviewedPaths) != 0 || len(e.LocalChecks) != 0 || len(e.ResultFingerprints) != 0 || len(e.Unresolved) == 0 {
			return errors.New("audit coverage-ledger.json has inconsistent excluded unit evidence")
		}
		return nil
	case "covered", "candidate", "blocked":
		if !validAuditCoverageAgent(e.AgentID) || len(e.ReviewedPaths) == 0 || len(e.LocalChecks) == 0 {
			return errors.New("audit coverage-ledger.json lacks assigned review evidence")
		}
	default:
		return errors.New("audit coverage-ledger.json has invalid evidence status")
	}
	if e.Status != "candidate" && len(e.ResultFingerprints) != 0 || e.Status == "covered" && len(e.Unresolved) != 0 || e.Status == "blocked" && len(e.Unresolved) == 0 {
		return errors.New("audit coverage-ledger.json has inconsistent status evidence")
	}
	paths := make(map[string]bool, len(e.ReviewedPaths))
	for _, path := range e.ReviewedPaths {
		_, duplicate := paths[path]
		if !workspace.ValidPath(path) || duplicate {
			return errors.New("audit coverage-ledger.json has invalid reviewed path")
		}
		paths[path] = false
	}
	for _, check := range e.LocalChecks {
		if !validAuditCoverageAgent(check.AgentID) || strings.TrimSpace(check.Invariant) == "" || strings.TrimSpace(check.Result) == "" || len(check.ReviewedPaths) == 0 {
			return errors.New("audit coverage-ledger.json has incomplete local check")
		}
		if check.Method != "source" && check.Method != "local" {
			return errors.New("audit coverage-ledger.json has invalid local check method")
		}
		if check.Method == "source" && !bytes.Equal(bytes.TrimSpace(check.Artifact), []byte("null")) {
			return errors.New("audit coverage-ledger.json has source check artifact")
		}
		if check.Method == "local" {
			var artifact string
			if json.Unmarshal(check.Artifact, &artifact) != nil || !workspace.ValidPath(artifact) || !strings.HasPrefix(artifact, "agents/"+check.AgentID+"/artifacts/") {
				return errors.New("audit coverage-ledger.json has unowned local check artifact")
			}
		}
		for _, path := range check.ReviewedPaths {
			if _, ok := paths[path]; !ok {
				return errors.New("audit coverage-ledger.json has local check outside reviewed paths")
			}
			paths[path] = true
		}
	}
	for _, checked := range paths {
		if !checked {
			return errors.New("audit coverage-ledger.json has reviewed path without local check")
		}
	}
	return nil
}

func validAuditCoverageAgent(agent string) bool {
	if len(agent) == 0 || len(agent) > 64 || !(agent[0] >= 'a' && agent[0] <= 'z' || agent[0] >= '0' && agent[0] <= '9') {
		return false
	}
	return strings.Trim(agent, "abcdefghijklmnopqrstuvwxyz0123456789_-") == "" && workspace.ValidPath("agents/"+agent+"/artifacts/check")
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
