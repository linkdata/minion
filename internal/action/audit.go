package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

var auditValidators = []struct{ script, artifact string }{
	{"validate-findings.cjs", "findings.json"},
	{"validate-coverage-ledger.cjs", "coverage-ledger.json"},
}

var auditCleanClaim = regexp.MustCompile(`\b(?:(?:the|this) audit (?:found|identified|detected|reported) )?(?:no|zero|0) (?:findings|vulnerabilities|security issues)(?: (?:were )?(?:found|identified|detected|reported))?(?:[,.!?;:]|$)`)
var auditNoConfirmedClaim = regexp.MustCompile(`\b(?:(?:the|this) audit (?:found|identified|detected|reported) )?(?:no|zero|0) confirmed (?:findings|vulnerabilities|security issues)(?: (?:were )?(?:found|identified|detected|reported))?(?:[,.!?;:]|$)`)

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

func rejectDuplicateAuditJSONKeys(data []byte, canonicalKeys ...string) error {
	canonical := make(map[string]string, len(canonicalKeys))
	for _, key := range canonicalKeys {
		canonical[workspace.FoldPath(key)] = key
	}
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
				if want := canonical[folded]; want != "" && key != want {
					return fmt.Errorf("audit JSON uses noncanonical key %q", key)
				}
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
	if err := validateAuditCoverage(byPath["coverage-ledger.json"], byPath["findings.json"], byPath["REPORT.md"], byPath["FINDINGS-DETAIL.md"], byPath["NEEDS-VALIDATION.md"], nil); err != nil {
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
	Wave               float64              `json:"wave"`
	Status             string               `json:"status"`
	AgentID            string               `json:"agent_id"`
	ReviewedPaths      []string             `json:"reviewed_paths"`
	LocalChecks        []auditCoverageCheck `json:"local_checks"`
	ResultFingerprints []string             `json:"result_fingerprints"`
	Unresolved         []string             `json:"unresolved"`
	ReassignmentReason string               `json:"reassignment_reason"`
}

// This checks the worker's report for internal consistency and links source claims to the assigned snapshot; it cannot prove a reviewer ran.
func validateAuditCoverage(ledgerBytes, findingsBytes, reportBytes, detailsBytes, needsBytes []byte, source *workspace.Snapshot) error {
	if err := rejectDuplicateAuditJSONKeys(ledgerBytes,
		"coverage_id", "canonical_refs", "surface", "boundary", "subsystem", "attack_class", "lifecycle", "starting_paths",
		"ordinary_attack_class_block", "selected_companion_blocks", "excluded_blocks", "prior_status", "attempts", "wave", "status",
		"block", "reason", "agent_id", "reviewed_paths", "local_checks", "result_fingerprints", "unresolved", "reassignment_reason",
		"invariant", "method", "result", "artifact"); err != nil {
		return fmt.Errorf("audit coverage-ledger.json: %w", err)
	}
	if err := rejectDuplicateAuditJSONKeys(findingsBytes,
		"verdict", "fingerprint", "title", "description", "root_cause", "intended_behavior", "trace", "evidence", "conditions",
		"execution", "remediation", "severity", "confidence", "claimed_root_cause", "blockers", "validation_plan", "reason",
		"kind", "file", "line", "scope", "attacker_perspective", "payloads", "instructions", "observed_result", "strategy",
		"code_changes", "file_name", "fixed_code", "likelihood", "impact", "score", "overall_severity", "local", "deployment"); err != nil {
		return fmt.Errorf("audit findings.json: %w", err)
	}
	var units []struct {
		auditCoverageEvidence
		CoverageID               string                           `json:"coverage_id"`
		CanonicalRefs            map[string]string                `json:"canonical_refs"`
		Surface                  string                           `json:"surface"`
		Boundary                 string                           `json:"boundary"`
		Subsystem                string                           `json:"subsystem"`
		AttackClass              string                           `json:"attack_class"`
		StartingPaths            []string                         `json:"starting_paths"`
		Lifecycle                string                           `json:"lifecycle"`
		OrdinaryAttackClassBlock json.RawMessage                  `json:"ordinary_attack_class_block"`
		SelectedCompanionBlocks  []string                         `json:"selected_companion_blocks"`
		ExcludedBlocks           []struct{ Block, Reason string } `json:"excluded_blocks"`
		PriorStatus              string                           `json:"prior_status"`
		Attempts                 []auditCoverageEvidence          `json:"attempts"`
	}
	if err := json.Unmarshal(ledgerBytes, &units); err != nil {
		return fmt.Errorf("decode audit coverage-ledger.json: %w", err)
	}
	if len(units) == 0 {
		return errors.New("audit coverage-ledger.json has no coverage units")
	}
	var findings []struct {
		Fingerprint      string                             `json:"fingerprint"`
		Verdict          string                             `json:"verdict"`
		Title            string                             `json:"title"`
		Description      string                             `json:"description"`
		RootCause        string                             `json:"root_cause"`
		IntendedBehavior string                             `json:"intended_behavior"`
		ClaimedRootCause string                             `json:"claimed_root_cause"`
		Blockers         []string                           `json:"blockers"`
		ValidationPlan   struct{ Local, Deployment string } `json:"validation_plan"`
		Reason           string                             `json:"reason"`
		Trace            []struct {
			Kind, File, Scope, Description string
			Line                           float64
		} `json:"trace"`
		Evidence []struct {
			File, Description string
			Line              float64
		} `json:"evidence"`
		Conditions []struct {
			Kind        string `json:"kind"`
			Description string `json:"description"`
		} `json:"conditions"`
		Execution struct {
			AttackerPerspective string   `json:"attacker_perspective"`
			Payloads            []string `json:"payloads"`
			Instructions        []string `json:"instructions"`
			ObservedResult      string   `json:"observed_result"`
		} `json:"execution"`
		Remediation struct {
			Strategy string `json:"strategy"`
		} `json:"remediation"`
		Severity struct {
			Likelihood struct{ Score, Reason string } `json:"likelihood"`
			Impact     struct{ Score, Reason string } `json:"impact"`
			Overall    string                         `json:"overall_severity"`
		} `json:"severity"`
		Confidence struct{ Score, Reason string } `json:"confidence"`
	}
	if err := json.Unmarshal(findingsBytes, &findings); err != nil {
		return fmt.Errorf("decode audit findings: %w", err)
	}
	if findings == nil {
		return errors.New("audit findings.json is not an array")
	}
	if len(findings) > 1000 { // The installed findings validator uses the same cap.
		return errors.New("audit findings.json exceeds 1000 findings")
	}
	sourcePaths, sourceLines := auditSourceInventory(source)
	final := make(map[string]bool, len(findings))
	findingPaths := make(map[string]map[string]bool, len(findings))
	for _, finding := range findings {
		if strings.TrimSpace(finding.Fingerprint) == "" {
			return errors.New("audit findings.json has an empty fingerprint")
		}
		if final[finding.Fingerprint] {
			return errors.New("audit findings.json has duplicate fingerprint")
		}
		if finding.Verdict != "confirmed" && finding.Verdict != "needs_validation" && finding.Verdict != "rejected" ||
			!auditVisibleText(finding.Title) || !auditVisibleText(finding.Description) || len(finding.Trace) == 0 || len(finding.Evidence) == 0 {
			return errors.New("audit findings.json has an incomplete finding")
		}
		switch finding.Verdict {
		case "confirmed":
			invalid := !auditVisibleText(finding.RootCause) || !auditVisibleText(finding.IntendedBehavior) ||
				finding.Conditions == nil || !auditVisibleText(finding.Execution.AttackerPerspective) || len(finding.Execution.Payloads) == 0 ||
				len(finding.Execution.Instructions) == 0 || !auditVisibleText(finding.Execution.ObservedResult) ||
				!auditVisibleText(finding.Remediation.Strategy) ||
				!auditVisibleText(finding.Severity.Likelihood.Reason) || !auditVisibleText(finding.Severity.Impact.Reason) ||
				!auditVisibleText(finding.Confidence.Reason) ||
				(finding.Confidence.Score != "low" && finding.Confidence.Score != "medium" && finding.Confidence.Score != "high")
			for _, condition := range finding.Conditions {
				invalid = invalid || !auditConditionKind(condition.Kind) || !auditVisibleText(condition.Description)
			}
			for _, instruction := range finding.Execution.Instructions {
				invalid = invalid || !auditVisibleText(instruction)
			}
			impact := auditSeverityRank(finding.Severity.Impact.Score)
			if invalid || auditSeverityRank(finding.Severity.Likelihood.Score) < 0 || impact < 0 ||
				auditSeverityRank(finding.Severity.Overall) < 0 || auditSeverityRank(finding.Severity.Overall) > impact {
				return errors.New("audit findings.json has an incomplete confirmed finding")
			}
		case "needs_validation":
			if !auditVisibleText(finding.ClaimedRootCause) || len(finding.Blockers) == 0 ||
				!auditVisibleText(finding.ValidationPlan.Local) && !auditVisibleText(finding.ValidationPlan.Deployment) {
				return errors.New("audit findings.json has an incomplete needs_validation finding")
			}
			for _, blocker := range finding.Blockers {
				if !auditVisibleText(blocker) {
					return errors.New("audit findings.json has an incomplete needs_validation finding")
				}
			}
		case "rejected":
			if !auditVisibleText(finding.ClaimedRootCause) || !auditVisibleText(finding.Reason) {
				return errors.New("audit findings.json has an incomplete rejected finding")
			}
		}
		citedPaths := make(map[string]bool)
		for i, trace := range finding.Trace {
			if (trace.Kind != "entrypoint" && trace.Kind != "propagation" && trace.Kind != "sink") || !workspace.ValidPath(trace.File) ||
				!auditPositiveInteger(trace.Line) || !auditVisibleText(trace.Scope) || !auditVisibleText(trace.Description) ||
				len(finding.Trace) == 1 && trace.Kind == "propagation" ||
				len(finding.Trace) > 1 && (i == 0 && trace.Kind != "entrypoint" || i == len(finding.Trace)-1 && trace.Kind != "sink" ||
					i > 0 && i < len(finding.Trace)-1 && trace.Kind != "propagation") {
				return errors.New("audit findings.json has invalid trace evidence")
			}
			if source != nil && (sourceLines[trace.File] == 0 || trace.Line > float64(sourceLines[trace.File])) {
				return errors.New("audit findings.json has trace outside current source")
			}
			citedPaths[trace.File] = true
		}
		for _, evidence := range finding.Evidence {
			if !workspace.ValidPath(evidence.File) || !auditPositiveInteger(evidence.Line) || !auditVisibleText(evidence.Description) {
				return errors.New("audit findings.json has invalid source evidence")
			}
			if source != nil && (sourceLines[evidence.File] == 0 || evidence.Line > float64(sourceLines[evidence.File])) {
				return errors.New("audit findings.json has evidence outside current source")
			}
			citedPaths[evidence.File] = true
		}
		final[finding.Fingerprint] = true
		findingPaths[finding.Fingerprint] = citedPaths
	}
	candidates := make(map[string]bool)
	checkedCitations := make(map[string]map[string]map[string]bool)
	candidateOwners := make(map[string]map[string]bool)
	checkCandidate := func(e auditCoverageEvidence) error {
		if len(e.ResultFingerprints) == 0 {
			return errors.New("audit coverage-ledger.json has candidate without fingerprint")
		}
		hasVerifier := false
		for _, check := range e.LocalChecks {
			if check.AgentID != e.AgentID {
				hasVerifier = true
			}
		}
		for _, reason := range e.Unresolved {
			if strings.Contains(reason, "validation_budget_exhausted") {
				return errors.New("audit coverage-ledger.json has unvalidated candidate")
			}
		}
		for _, fingerprint := range e.ResultFingerprints {
			if strings.TrimSpace(fingerprint) == "" {
				return errors.New("audit coverage-ledger.json has candidate without fingerprint")
			}
			candidates[fingerprint] = true
			if !final[fingerprint] {
				return errors.New("audit coverage-ledger.json has a candidate without final finding")
			}
			if candidateOwners[fingerprint] == nil {
				candidateOwners[fingerprint] = make(map[string]bool)
			}
			candidateOwners[fingerprint][e.AgentID] = true
			if checkedCitations[fingerprint] == nil {
				checkedCitations[fingerprint] = make(map[string]map[string]bool)
			}
			for _, check := range e.LocalChecks {
				if check.AgentID == e.AgentID {
					continue
				}
				if checkedCitations[fingerprint][check.AgentID] == nil {
					checkedCitations[fingerprint][check.AgentID] = make(map[string]bool, len(findingPaths[fingerprint]))
				}
				for _, path := range check.ReviewedPaths {
					if findingPaths[fingerprint][path] {
						checkedCitations[fingerprint][check.AgentID][path] = true
					}
				}
			}
		}
		if !hasVerifier {
			return errors.New("audit coverage-ledger.json has candidate without separately attributed source check")
		}
		return nil
	}
	seenIDs := make(map[string]bool, len(units))
	blocked := 0
	reviewedCoverage := false
	for _, unit := range units {
		switch unit.Status {
		case "planned", "in_progress", "deferred":
			return fmt.Errorf("audit coverage-ledger.json has unfinished %s unit", unit.Status)
		case "not_applicable", "out_of_scope", "covered", "candidate", "blocked":
		default:
			return errors.New("audit coverage-ledger.json has invalid unit status")
		}
		reviewedCoverage = reviewedCoverage || unit.Status == "covered" || unit.Status == "candidate"
		for _, attempt := range unit.Attempts {
			if attempt.Status != "covered" && attempt.Status != "candidate" && attempt.Status != "blocked" {
				return errors.New("audit coverage-ledger.json has invalid attempt status")
			}
			reviewedCoverage = reviewedCoverage || attempt.Status == "covered" || attempt.Status == "candidate"
			if attempt.Status == "candidate" {
				if err := checkCandidate(attempt); err != nil {
					return err
				}
			}
		}
		if unit.Status == "candidate" {
			if err := checkCandidate(unit.auditCoverageEvidence); err != nil {
				return err
			}
		}
		if unit.CoverageID == "" || seenIDs[unit.CoverageID] || len(unit.StartingPaths) == 0 ||
			strings.TrimSpace(unit.Surface) == "" || strings.TrimSpace(unit.Boundary) == "" ||
			strings.TrimSpace(unit.Subsystem) == "" || strings.TrimSpace(unit.AttackClass) == "" {
			return errors.New("audit coverage-ledger.json lacks unique coverage identity or starting paths")
		}
		seenIDs[unit.CoverageID] = true
		if unit.Status == "blocked" {
			blocked++
		}
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
			if !workspace.ValidPath(path) || source != nil && !sourcePaths[path] {
				return errors.New("audit coverage-ledger.json has invalid starting path")
			}
		}
		if len(unit.OrdinaryAttackClassBlock) == 0 || unit.SelectedCompanionBlocks == nil || unit.ExcludedBlocks == nil ||
			unit.Attempts == nil || unit.PriorStatus == "" {
			return errors.New("audit coverage-ledger.json lacks required coverage context")
		}
		if unit.PriorStatus != "none" {
			return errors.New("audit coverage-ledger.json claims a prior run absent from metadata")
		}
		if !bytes.Equal(bytes.TrimSpace(unit.OrdinaryAttackClassBlock), []byte("null")) {
			var block string
			if json.Unmarshal(unit.OrdinaryAttackClassBlock, &block) != nil || !auditVisibleText(block) {
				return errors.New("audit coverage-ledger.json has invalid ordinary attack class block")
			}
		}
		for _, block := range unit.SelectedCompanionBlocks {
			if !auditVisibleText(block) {
				return errors.New("audit coverage-ledger.json has invalid selected companion block")
			}
		}
		for _, block := range unit.ExcludedBlocks {
			if !auditVisibleText(block.Block) || !auditVisibleText(block.Reason) {
				return errors.New("audit coverage-ledger.json has invalid excluded block")
			}
		}
		if err := validateAuditCoverageEvidence(unit.auditCoverageEvidence, sourcePaths); err != nil {
			return err
		}
		if unit.Status == "covered" || unit.Status == "candidate" {
			if !auditSeedsChecked(unit.StartingPaths, unit.ReviewedPaths, sourceLines) {
				return errors.New("audit coverage-ledger.json has starting path without a declared reviewed file")
			}
		}
		if !auditPositiveInteger(unit.Wave) {
			return errors.New("audit coverage-ledger.json has invalid current wave")
		}
		previousWave := float64(0)
		priorOwners := make(map[string]bool, len(unit.Attempts))
		for _, attempt := range unit.Attempts {
			if err := validateAuditCoverageEvidence(attempt, sourcePaths); err != nil {
				return err
			}
			if attempt.Status == "covered" || attempt.Status == "candidate" {
				if !auditSeedsChecked(unit.StartingPaths, attempt.ReviewedPaths, sourceLines) {
					return errors.New("audit coverage-ledger.json has archived starting path without a declared reviewed file")
				}
			}
			if !auditPositiveInteger(attempt.Wave) || attempt.Wave <= previousWave || attempt.Wave >= unit.Wave || strings.TrimSpace(attempt.ReassignmentReason) == "" {
				return errors.New("audit coverage-ledger.json has invalid archived attempt wave or reassignment")
			}
			if priorOwners[attempt.AgentID] {
				return errors.New("audit coverage-ledger.json reuses an archived assignment owner")
			}
			for _, check := range attempt.LocalChecks {
				if priorOwners[check.AgentID] {
					return errors.New("audit coverage-ledger.json reuses an archived assignment check owner")
				}
			}
			previousWave = attempt.Wave
			priorOwners[attempt.AgentID] = true
			for _, check := range attempt.LocalChecks {
				priorOwners[check.AgentID] = true
			}
		}
		if priorOwners[unit.AgentID] {
			return errors.New("audit coverage-ledger.json reuses an archived assignment owner")
		}
		for _, check := range unit.LocalChecks {
			if priorOwners[check.AgentID] {
				return errors.New("audit coverage-ledger.json reuses an archived assignment check owner")
			}
		}
	}
	if !reviewedCoverage {
		return errors.New("audit coverage-ledger.json declares no reviewed coverage")
	}
	for fingerprint := range final {
		if !candidates[fingerprint] {
			return errors.New("audit findings.json has a finding without candidate")
		}
		verified := false
		for agentID, paths := range checkedCitations[fingerprint] {
			if !candidateOwners[fingerprint][agentID] && len(paths) == len(findingPaths[fingerprint]) {
				verified = true
				break
			}
		}
		if !verified {
			return errors.New("audit findings.json has a cited file without one declared verifier path set")
		}
	}
	// shortcut: Free Markdown can still contradict records in other words; render public prose from records when exact agreement is required.
	boundaries := make(map[string]map[string]bool)
	addBoundaries := func(boundary string, evidence auditCoverageEvidence) {
		for _, fingerprint := range evidence.ResultFingerprints {
			if boundaries[fingerprint] == nil {
				boundaries[fingerprint] = make(map[string]bool)
			}
			boundaries[fingerprint][auditReportCell(boundary)] = true
		}
	}
	for _, unit := range units {
		if unit.Status == "candidate" {
			addBoundaries(unit.Boundary, unit.auditCoverageEvidence)
		}
		for _, attempt := range unit.Attempts {
			if attempt.Status == "candidate" {
				addBoundaries(unit.Boundary, attempt)
			}
		}
	}
	report := strings.ToLower(string(reportBytes))
	reportable := false
	confirmed := false
	var requiredDetails, requiredNeeds [][]string
	for _, finding := range findings {
		if finding.Verdict == "rejected" {
			continue
		}
		reportable = true
		confirmed = confirmed || finding.Verdict == "confirmed"
		title := auditReportCell(finding.Title)
		found := false
		for _, line := range strings.Split(report, "\n") {
			if finding.Verdict == "confirmed" {
				cells := auditTableCells(line)
				if len(cells) != 4 || strings.Trim(strings.TrimSpace(cells[0]), "*_` ") != finding.Severity.Overall ||
					auditReportCell(cells[1]) != title || !boundaries[finding.Fingerprint][auditReportCell(cells[2])] ||
					auditReportCell(cells[3]) != auditReportCell(finding.Execution.ObservedResult) {
					continue
				}
				found = true
				break
			}
			if strings.Contains(strings.Join(strings.Fields(line), " "), title) {
				found = true
				break
			}
		}
		if !found {
			if finding.Verdict == "confirmed" {
				return errors.New("audit REPORT.md omits or contradicts a confirmed finding table row")
			}
			return errors.New("audit REPORT.md omits a final finding title")
		}
		if finding.Verdict == "confirmed" && auditSeverityRank(finding.Severity.Overall) >= auditSeverityRank("medium") {
			requiredDetails = append(requiredDetails, []string{finding.Fingerprint + ": " + finding.Title, finding.Trace[0].File,
				finding.Execution.ObservedResult, finding.Remediation.Strategy})
		}
		if finding.Verdict == "needs_validation" {
			fields := []string{finding.Fingerprint + ": " + finding.Title, finding.Trace[0].File}
			fields = append(fields, finding.Blockers...)
			if finding.ValidationPlan.Local != "" {
				fields = append(fields, finding.ValidationPlan.Local)
			}
			if finding.ValidationPlan.Deployment != "" {
				fields = append(fields, finding.ValidationPlan.Deployment)
			}
			requiredNeeds = append(requiredNeeds, fields)
		}
	}
	for _, line := range strings.Split(report, "\n") {
		cells := auditTableCells(line)
		if len(cells) != 4 || auditReportCell(cells[0]) == "severity" {
			continue
		}
		title := auditReportCell(cells[1])
		mentioned, matched := false, false
		for _, finding := range findings {
			if finding.Verdict != "confirmed" || auditReportCell(finding.Title) != title {
				continue
			}
			mentioned = true
			if strings.Trim(strings.TrimSpace(cells[0]), "*_` ") == finding.Severity.Overall &&
				boundaries[finding.Fingerprint][auditReportCell(cells[2])] &&
				auditReportCell(cells[3]) == auditReportCell(finding.Execution.ObservedResult) {
				matched = true
				break
			}
		}
		if (mentioned || auditSeverityRank(strings.Trim(strings.TrimSpace(cells[0]), "*_` ")) >= 0) && !matched {
			return errors.New("audit REPORT.md contradicts a confirmed finding table row")
		}
	}
	flatReport := strings.Join(strings.Fields(report), " ")
	if (reportable || blocked > 0) && auditReportHasClaim(report, auditCleanClaim) || confirmed && auditReportHasClaim(report, auditNoConfirmedClaim) {
		return errors.New("audit REPORT.md contradicts final findings")
	}
	if blocked > 0 && !regexp.MustCompile(fmt.Sprintf(`\b(?:blocked:\s*%d|%d\s+blocked)\b`, blocked, blocked)).MatchString(flatReport) {
		return errors.New("audit REPORT.md omits blocked coverage count")
	}
	if err := validateAuditProse(detailsBytes, requiredDetails); err != nil {
		return fmt.Errorf("audit FINDINGS-DETAIL.md %w", err)
	}
	if err := validateAuditProse(needsBytes, requiredNeeds); err != nil {
		return fmt.Errorf("audit NEEDS-VALIDATION.md %w", err)
	}
	return nil
}

func auditReportCell(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func auditTableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	var cells []string
	var cell strings.Builder
	escaped := false
	trailingSeparator := false
	for _, r := range line {
		if escaped {
			if r != '|' {
				cell.WriteByte('\\')
			}
			cell.WriteRune(r)
			escaped = false
			trailingSeparator = false
			continue
		}
		switch r {
		case '\\':
			escaped = true
			trailingSeparator = false
		case '|':
			cells = append(cells, cell.String())
			cell.Reset()
			trailingSeparator = true
		default:
			cell.WriteRune(r)
			trailingSeparator = false
		}
	}
	if escaped {
		cell.WriteByte('\\')
	}
	if !trailingSeparator {
		cells = append(cells, cell.String())
	}
	return cells
}

func auditReportHasClaim(report string, claim *regexp.Regexp) bool {
	startsWithClaim := func(text string) bool {
		text = strings.TrimFunc(text, func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune("#*_|>`-", r)
		})
		text = strings.TrimSpace(strings.TrimPrefix(text, "summary:"))
		location := claim.FindStringIndex(text)
		return location != nil && location[0] == 0
	}
	var paragraph strings.Builder
	flush := func() bool {
		found := startsWithClaim(strings.Join(strings.Fields(paragraph.String()), " "))
		paragraph.Reset()
		return found
	}
	inFence := false
	for _, line := range strings.Split(report, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if flush() {
				return true
			}
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" || strings.HasPrefix(trimmed, ">") {
			if flush() {
				return true
			}
			continue
		}
		if startsWithClaim(trimmed) {
			return true
		}
		if strings.HasPrefix(trimmed, "#") {
			if flush() {
				return true
			}
			continue
		}
		paragraph.WriteString(line)
		paragraph.WriteByte(' ')
	}
	return flush()
}

func validateAuditProse(prose []byte, records [][]string) error {
	if len(records) == 0 {
		return nil
	}
	sections := make(map[string]string, len(records))
	var body strings.Builder
	current := ""
	finish := func() error {
		if current == "" {
			return nil
		}
		if _, duplicate := sections[current]; duplicate {
			return errors.New("has duplicate finding section")
		}
		sections[current] = auditReportCell(body.String())
		body.Reset()
		return nil
	}
	for _, line := range strings.Split(string(prose), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if err := finish(); err != nil {
				return err
			}
			current = auditReportCell(strings.TrimPrefix(trimmed, "## "))
			continue
		}
		if current != "" {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if err := finish(); err != nil {
		return err
	}
	const maxScanBytes = 1 << 30
	budget := int64(maxScanBytes)
	for _, record := range records {
		section, present := sections[auditReportCell(record[0])]
		if !present {
			return errors.New("omits required finding section")
		}
		for _, field := range record[1:] {
			if int64(len(section)) > budget {
				return errors.New("detail scan limit exceeded")
			}
			budget -= int64(len(section))
			if !strings.Contains(section, auditReportCell(field)) {
				return errors.New("omits required finding detail")
			}
		}
	}
	return nil
}

func auditSourceInventory(source *workspace.Snapshot) (map[string]bool, map[string]int) {
	if source == nil {
		return nil, nil
	}
	paths := make(map[string]bool, len(source.Files))
	lines := make(map[string]int, len(source.Files))
	add := func(path string) {
		paths[path] = true
		for i := strings.LastIndexByte(path, '/'); i >= 0; i = strings.LastIndexByte(path[:i], '/') {
			parent := path[:i]
			if paths[parent] {
				break
			}
			paths[parent] = true
		}
	}
	for _, file := range source.Files {
		add(file.Path)
		lines[file.Path] = bytes.Count(file.Data, []byte{'\n'})
		if len(file.Data) != 0 && file.Data[len(file.Data)-1] != '\n' {
			lines[file.Path]++
		}
	}
	// shortcut: Omitted mixes head and base links; add head-only link identity before accepting link coverage.
	return paths, lines
}

func auditSeedsChecked(starting, reviewed []string, sourceLines map[string]int) bool {
	for _, seed := range starting {
		checked := false
		for _, path := range reviewed {
			_, isFile := sourceLines[path]
			if (sourceLines == nil || isFile) && (path == seed || strings.HasPrefix(path, seed+"/")) {
				checked = true
				break
			}
		}
		if !checked {
			return false
		}
	}
	return true
}

func auditSeverityRank(score string) int {
	switch score {
	case "informational":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "critical":
		return 4
	default:
		return -1
	}
}

func auditConditionKind(kind string) bool {
	switch kind {
	case "authentication_level", "authorization_role", "user_interaction", "system_configuration", "network_routing",
		"environmental_dependency", "data_state", "timing_dependency", "third_party_dependency":
		return true
	default:
		return false
	}
}

func auditVisibleText(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) &&
			!unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) && !unicode.Is(unicode.Variation_Selector, r) {
			return true
		}
	}
	return false
}

func auditPositiveInteger(n float64) bool {
	return n >= 1 && n <= 1<<53-1 && math.Trunc(n) == n
}

func validateAuditCoverageEvidence(e auditCoverageEvidence, sourcePaths map[string]bool) error {
	for _, reason := range e.Unresolved {
		if !auditVisibleText(reason) {
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
		if !workspace.ValidPath(path) || duplicate || sourcePaths != nil && !sourcePaths[path] {
			return errors.New("audit coverage-ledger.json has invalid reviewed path")
		}
		paths[path] = false
	}
	for _, check := range e.LocalChecks {
		if !validAuditCoverageAgent(check.AgentID) || !auditVisibleText(check.Invariant) || !auditVisibleText(check.Result) || len(check.ReviewedPaths) == 0 {
			return errors.New("audit coverage-ledger.json has incomplete local check")
		}
		if check.Method != "source" {
			return errors.New("audit coverage-ledger.json has unsupported local check method in source-only audit")
		}
		if !bytes.Equal(bytes.TrimSpace(check.Artifact), []byte("null")) {
			return errors.New("audit coverage-ledger.json has source check artifact")
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
	return strings.Trim(agent, "abcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}

func auditSkillSource(role RoleSettings) string {
	for _, sources := range [][]string{role.Skills, role.AlwaysOnSkills} {
		for _, source := range sources {
			if name, err := skills.Name(source); err == nil && name == "security-audit" {
				return source
			}
		}
	}
	return ""
}

func validateAuditSkillSource(metadataBytes []byte, expected string) error {
	var metadata struct {
		SkillSource string `json:"skill_source"`
	}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil || expected == "" || metadata.SkillSource != expected {
		return errors.New("audit metadata has unexpected skill source")
	}
	return nil
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
		for _, canonical := range []string{"run_id", "repo", "target", "source_ref", "profile", "scope_paths", "budget", "selected_companion_files", "prior_run_paths", "shared_file_owners", "execution_policy", "run_status", "incomplete_reason", "skill_source"} {
			if name != canonical && strings.EqualFold(name, canonical) {
				return nil, fmt.Errorf("audit metadata uses noncanonical key %q", name)
			}
		}
	}
	if _, incomplete := fields["incomplete_reason"]; incomplete {
		return nil, errors.New("audit metadata declares an incomplete reason for a complete run")
	}
	if !auditVisibleText(metadata.RunID) || metadata.Repo != repo || metadata.Target != target || metadata.SourceRef != head ||
		metadata.Profile != "standard" || len(metadata.ScopePaths) != 1 || metadata.ScopePaths[0] != "." ||
		string(metadata.Budget) != "null" || metadata.SelectedCompanionFiles == nil || metadata.PriorRunPaths == nil || len(metadata.PriorRunPaths) != 0 ||
		metadata.ExecutionPolicy != "sandboxed-source-and-local-only" || metadata.RunStatus != "complete" {
		return nil, fmt.Errorf("audit metadata does not declare a complete standard run of %s at %s", repo, head)
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
