package action

import (
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestCompleteAuditResultValidatesCoverage(t *testing.T) {
	validLedger := auditCoveredLedgerFixture
	for _, test := range []struct {
		name, ledger, findings, want string
	}{
		{name: "covered"},
		{name: "empty ledger", ledger: `[]`, want: "no coverage units"},
		{name: "false covered claim", ledger: `[{"status":"covered"}]`, want: "coverage identity"},
		{name: "contradictory duplicate status", ledger: strings.Replace(validLedger, `"status":"covered"`, `"status":"planned","status":"covered"`, 1), want: "duplicate object keys"},
		{name: "duplicate finding key", findings: `[{"fingerprint":"F","fingerprint":"F"}]`, want: "duplicate object keys"},
		{name: "bare finding", findings: `[{"fingerprint":"F"}]`, want: "incomplete finding"},
		{name: "noncanonical identity", ledger: strings.Replace(validLedger, `"coverage_id":"source::boundary::subsystem::attack"`, `"coverage_id":"forged"`, 1), want: "noncanonical coverage identity"},
		{name: "missing owner", ledger: strings.Replace(validLedger, `"agent_id":"hunter","reviewed_paths"`, `"agent_id":null,"reviewed_paths"`, 1), want: "assigned review evidence"},
		{name: "unchecked path", ledger: strings.Replace(validLedger, `"reviewed_paths":["source.txt"],"local_checks"`, `"reviewed_paths":["other.txt"],"local_checks"`, 1), want: "local check outside reviewed paths"},
		{name: "check without result", ledger: strings.Replace(validLedger, `"result":"The guard is present"`, `"result":""`, 1), want: "incomplete local check"},
		{name: "covered with unresolved", ledger: strings.Replace(validLedger, `"unresolved":[]}]`, `"unresolved":["ignored"]}]`, 1), want: "inconsistent status evidence"},
		{name: "archived attempt without evidence", ledger: strings.Replace(validLedger, `"attempts":[]`, `"attempts":[{"status":"covered"}]`, 1), want: "assigned review evidence"},
		{name: "null unit", ledger: `[null]`, want: "invalid unit status"},
		{name: "unknown unit status", ledger: `[{"status":"unknown"}]`, want: "invalid unit status"},
		{name: "unfinished unit", ledger: `[{"status":"planned"}]`, want: "unfinished planned unit"},
		{name: "null findings", findings: `null`, want: "not an array"},
		{name: "empty finding fingerprint", findings: `[{"fingerprint":""}]`, want: "empty fingerprint"},
		{name: "unvalidated candidate", ledger: `[{"status":"candidate","result_fingerprints":["F"],"unresolved":["validation_budget_exhausted"]}]`, findings: auditCandidateFindingFixture, want: "unvalidated candidate"},
		{name: "empty candidate fingerprint", ledger: `[{"status":"candidate","result_fingerprints":[""]}]`, want: "candidate without fingerprint"},
		{name: "candidate without finding", ledger: `[{"status":"candidate","result_fingerprints":["F"]}]`, want: "candidate without final finding"},
		{name: "finding without candidate", findings: auditCandidateFindingFixture, want: "finding without candidate"},
		{name: "unvalidated attempt", ledger: `[{"status":"covered","attempts":[{"status":"candidate","result_fingerprints":["F"],"unresolved":["validation_budget_exhausted"]}]}]`, findings: auditCandidateFindingFixture, want: "unvalidated candidate"},
		{name: "unknown attempt status", ledger: `[{"status":"covered","attempts":[{"status":"unknown"}]}]`, want: "invalid attempt status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.SecurityAudit)
			result := completeAuditResult(job)
			for i := range result.Artifacts {
				switch result.Artifacts[i].Path {
				case "coverage-ledger.json":
					if test.ledger != "" {
						result.Artifacts[i].Data = []byte(test.ledger)
					}
				case "findings.json":
					if test.findings != "" {
						result.Artifacts[i].Data = []byte(test.findings)
					}
				}
			}
			err := result.Validate(job)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("complete audit validation = %v, want %q", err, test.want)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("partial audit evidence rejected: %v", err)
			}
		})
	}
}
