package action

import (
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

func TestCompleteAuditClaimsBindToCurrentSource(t *testing.T) {
	candidate := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
	candidate = strings.Replace(candidate, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	other := strings.Replace(candidate, `"coverage_id":"source::`, `"coverage_id":"other::`, 1)
	other = strings.Replace(other, `"canonical_refs":{"surface":"source"`, `"canonical_refs":{"surface":"other"`, 1)
	other = strings.ReplaceAll(other, "source.txt", "other.txt")
	otherVerifier := strings.Replace(other, `"agent_id":"verifier"`, `"agent_id":"other-verifier"`, 1)
	archived := `{"wave":1,"status":"candidate","agent_id":"older","reviewed_paths":["source.txt"],"local_checks":[{"agent_id":"helper","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}],"result_fingerprints":["F"],"unresolved":[],"reassignment_reason":"Rechecked"}`
	coveredAfterArchive := strings.Replace(auditCoveredLedgerFixture, `"attempts":[]`, `"attempts":[`+archived+`]`, 1)
	coveredAfterArchive = strings.Replace(coveredAfterArchive, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	archivedMissingSeed := strings.Replace(coveredAfterArchive, `"agent_id":"older","reviewed_paths":["source.txt"]`, `"agent_id":"older","reviewed_paths":["other.txt"]`, 1)
	archivedMissingSeed = strings.Replace(archivedMissingSeed, `"agent_id":"helper","reviewed_paths":["source.txt"]`, `"agent_id":"helper","reviewed_paths":["other.txt"]`, 1)
	citesOther := strings.Replace(auditCandidateFindingFixture, `"description":"Input reaches the boundary."}]`, `"description":"Input reaches the boundary."},{"kind":"sink","file":"other.txt","line":1,"scope":"other","description":"The operation changes state."}]`, 1)
	unrelatedVerifier := strings.Replace(candidate, `"reviewed_paths":["source.txt"],"local_checks":[{"agent_id":"verifier","reviewed_paths":["source.txt"]`, `"reviewed_paths":["source.txt","other.txt"],"local_checks":[{"agent_id":"hunter","reviewed_paths":["source.txt"]`, 1)
	unrelatedVerifier = strings.Replace(unrelatedVerifier, `"artifact":null}],"result_fingerprints"`, `"artifact":null},{"agent_id":"verifier","reviewed_paths":["other.txt"],"invariant":"Other path is checked","method":"source","result":"Other path is present","artifact":null}],"result_fingerprints"`, 1)
	unit := func(ledger string) string { return strings.TrimSuffix(strings.TrimPrefix(ledger, "["), "]") }
	for _, test := range []struct {
		name, ledger, findings, want string
		files                        []workspace.File
		omitted                      []string
	}{
		{name: "missing current starting path", ledger: strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "ghost.go"), want: "invalid starting path"},
		{name: "missing current reviewed path", ledger: strings.ReplaceAll(auditCoveredLedgerFixture, `"reviewed_paths":["source.txt"]`, `"reviewed_paths":["ghost.go"]`), want: "invalid reviewed path"},
		{name: "base-only omitted link is not current evidence", ledger: strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "old-link"), omitted: []string{"old-link"}, want: "invalid starting path"},
		{name: "missing current trace file", ledger: candidate, findings: strings.Replace(auditCandidateFindingFixture, `"trace":[{"kind":"entrypoint","file":"source.txt"`, `"trace":[{"kind":"entrypoint","file":"ghost.go"`, 1), want: "trace outside current source"},
		{name: "line beyond current file", ledger: candidate, findings: strings.Replace(auditCandidateFindingFixture, `"evidence":[{"file":"source.txt","line":1`, `"evidence":[{"file":"source.txt","line":999999`, 1), want: "evidence outside current source"},
		{name: "empty current file has no lines", ledger: candidate, findings: auditCandidateFindingFixture, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular}}, want: "trace outside current source"},
		{name: "unterminated final line exists", ledger: candidate, findings: strings.ReplaceAll(auditCandidateFindingFixture, `"line":1`, `"line":2`), files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("first\nsecond")}}},
		{name: "unconnected single candidate", ledger: other, findings: auditCandidateFindingFixture, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}, want: "cited file without one declared verifier path set"},
		{name: "shared root cause across entry paths", ledger: "[" + unit(other) + "," + unit(candidate) + "]", findings: auditCandidateFindingFixture, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}},
		{name: "unreviewed cited sink", ledger: candidate, findings: citesOther, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}, want: "cited file without one declared verifier path set"},
		{name: "hunter citation with unrelated verifier", ledger: unrelatedVerifier, findings: auditCandidateFindingFixture, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}, want: "cited file without one declared verifier path set"},
		{name: "linked candidates cover both citations", ledger: "[" + unit(other) + "," + unit(candidate) + "]", findings: citesOther, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}},
		{name: "different verifiers each cover only half the trace", ledger: "[" + unit(otherVerifier) + "," + unit(candidate) + "]", findings: citesOther, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}, want: "cited file without one declared verifier path set"},
		{name: "directory check does not declare cited file", ledger: strings.ReplaceAll(candidate, "source.txt", "src"), findings: strings.ReplaceAll(auditCandidateFindingFixture, "source.txt", "src/entry.go"), files: []workspace.File{{Path: "src/entry.go", Mode: workspace.Regular, Data: []byte("entry\n")}}, want: "starting path without a declared reviewed file"},
		{name: "archived candidate with current source evidence", ledger: coveredAfterArchive, findings: auditCandidateFindingFixture},
		{name: "archived candidate misses starting file", ledger: archivedMissingSeed, findings: auditCandidateFindingFixture, files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}, want: "archived starting path without a declared reviewed file"},
		{name: "covered unit misses starting file", ledger: strings.ReplaceAll(auditCoveredLedgerFixture, `"reviewed_paths":["source.txt"]`, `"reviewed_paths":["other.txt"]`), files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}, {Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}}, want: "starting path without a declared reviewed file"},
		{name: "directory seed with checked descendant", ledger: strings.Replace(strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "src/entry.go"), `"starting_paths":["src/entry.go"]`, `"starting_paths":["src"]`, 1), files: []workspace.File{{Path: "src/entry.go", Mode: workspace.Regular, Data: []byte("entry\n")}}},
		{name: "directory seed with only directory check", ledger: strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "src"), files: []workspace.File{{Path: "src/entry.go", Mode: workspace.Regular, Data: []byte("entry\n")}}, want: "starting path without a declared reviewed file"},
		{name: "directory prefix must end at component", ledger: strings.Replace(strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "src2/entry.go"), `"starting_paths":["src2/entry.go"]`, `"starting_paths":["src"]`, 1), files: []workspace.File{{Path: "src/other.go", Mode: workspace.Regular, Data: []byte("other\n")}, {Path: "src2/entry.go", Mode: workspace.Regular, Data: []byte("entry\n")}}, want: "starting path without a declared reviewed file"},
		{name: "unsafe huge wave", ledger: strings.Replace(auditCoveredLedgerFixture, `"wave":1`, `"wave":1e308`, 1), want: "invalid current wave"},
		{name: "maximum safe wave", ledger: strings.Replace(auditCoveredLedgerFixture, `"wave":1`, `"wave":9007199254740991`, 1)},
		{name: "first unsafe wave", ledger: strings.Replace(auditCoveredLedgerFixture, `"wave":1`, `"wave":9007199254740992`, 1), want: "invalid current wave"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.SecurityAudit)
			if test.files != nil {
				job.Source.Files = test.files
			}
			job.Source.Omitted = test.omitted
			result := completeAuditResult(job)
			if test.findings != "" {
				result.Answer = "Reviewed audit report: Unverified boundary."
			}
			for i := range result.Artifacts {
				switch result.Artifacts[i].Path {
				case "REPORT.md":
					result.Artifacts[i].Data = []byte(result.Answer)
				case "coverage-ledger.json":
					if test.ledger != "" {
						result.Artifacts[i].Data = []byte(test.ledger)
					}
				case "findings.json":
					if test.findings != "" {
						result.Artifacts[i].Data = []byte(test.findings)
					}
				case "NEEDS-VALIDATION.md":
					if strings.Contains(test.findings, `"verdict":"needs_validation"`) {
						result.Artifacts[i].Data = []byte(auditNeedsDetailFixture)
					}
				}
			}
			err := result.Validate(job)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("complete current audit = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompleteAuditSkillSourceMatchesJob(t *testing.T) {
	job := testJob(command.SecurityAudit)
	for _, test := range []struct{ name, replacement string }{
		{name: "missing"},
		{name: "forged", replacement: `,"skill_source":"https://github.com/other/skill/tree/main/skills/security-audit"`},
		{name: "case variant", replacement: `,"SKILL_SOURCE":"` + testAuditSkillURL + `"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := completeAuditResult(job)
			for i := range result.Artifacts {
				if result.Artifacts[i].Path == "run-metadata.json" {
					result.Artifacts[i].Data = []byte(strings.Replace(string(result.Artifacts[i].Data), `,"skill_source":"`+testAuditSkillURL+`"`, test.replacement, 1))
				}
			}
			if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "skill source") && !strings.Contains(err.Error(), "noncanonical key") {
				t.Fatalf("forged skill provenance passed: %v", err)
			}
		})
	}
}
