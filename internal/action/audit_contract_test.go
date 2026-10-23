package action

import (
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestCompleteAuditResultValidatesCoverage(t *testing.T) {
	validLedger := auditCoveredLedgerFixture
	archived := `{"wave":1,"status":"covered","agent_id":"older","reviewed_paths":["source.txt"],"local_checks":[{"agent_id":"helper","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}],"result_fingerprints":[],"unresolved":[],"reassignment_reason":"Rechecked"}`
	withAttempt := strings.Replace(validLedger, `"attempts":[]`, `"attempts":[`+archived+`]`, 1)
	withAttempt = strings.Replace(withAttempt, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	withAttempt = strings.Replace(withAttempt, `"local_checks":[{"agent_id":"hunter"`, `"local_checks":[{"agent_id":"verifier"`, 1)
	sourceCheck := `"method":"source","result":"The guard is present","artifact":null`
	oldCheck := `"method":"local","result":"The guard is present","artifact":"agents/helper/artifacts/old.txt"`
	newCheck := `"method":"local","result":"The guard is present","artifact":"agents/helper/artifacts/new.txt"`
	withLocalChecks := strings.Replace(withAttempt, `"agent_id":"verifier"`, `"agent_id":"helper"`, 1)
	withLocalChecks = strings.Replace(withLocalChecks, sourceCheck, oldCheck, 1)
	withLocalChecks = strings.Replace(withLocalChecks, sourceCheck, newCheck, 1)
	secondAttempt := strings.Replace(archived, `"wave":1,"status":"covered","agent_id":"older"`, `"wave":2,"status":"covered","agent_id":"newer"`, 1)
	withTwoAttempts := strings.Replace(validLedger, `"attempts":[]`, `"attempts":[`+archived+`,`+secondAttempt+`]`, 1)
	withTwoAttempts = strings.Replace(withTwoAttempts, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":3,"status":"covered","agent_id":"hunter"`, 1)
	withTwoAttempts = strings.Replace(withTwoAttempts, sourceCheck, oldCheck, 1)
	withTwoAttempts = strings.Replace(withTwoAttempts, sourceCheck, newCheck, 1)
	confirmedLedger := strings.Replace(validLedger, `"status":"covered"`, `"status":"candidate"`, 1)
	confirmedLedger = strings.Replace(confirmedLedger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	for _, test := range []struct {
		name, ledger, findings, want string
	}{
		{name: "covered"},
		{name: "covered with archived attempt and distinct check owners", ledger: withAttempt},
		{name: "distinct local artifacts across archived and current", ledger: withLocalChecks},
		{name: "distinct local artifacts across two archives", ledger: withTwoAttempts},
		{name: "confirmed finding", ledger: confirmedLedger, findings: auditConfirmedFindingFixture},
		{name: "confirmed with typed condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[{"kind":"authentication_level","description":"The attacker has an ordinary account."}]`, 1)},
		{name: "confirmed null condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[null]`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed unknown condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[{"kind":"unknown","description":"The attacker has an ordinary account."}]`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed invisible condition", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"conditions":[]`, `"conditions":[{"kind":"authentication_level","description":"\u200b"}]`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed invisible root cause", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"root_cause":"The operation omits the guard before changing the object."`, `"root_cause":"\u200b"`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed control-only observed result", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"observed_result":"The other user's object changes."`, `"observed_result":"\u0001"`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed missing observed result", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"observed_result":"The other user's object changes."`, `"observed_result":""`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed missing remediation", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"remediation":{"strategy":"Check ownership before the state change."}`, `"remediation":{}`, 1), want: "incomplete confirmed finding"},
		{name: "confirmed missing severity", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"severity":{"likelihood":{"score":"medium","reason":"The operation is directly reachable."},"impact":{"score":"medium","reason":"One protected object can change."},"overall_severity":"medium"},`, ``, 1), want: "incomplete confirmed finding"},
		{name: "empty ledger", ledger: `[]`, want: "no coverage units"},
		{name: "false covered claim", ledger: `[{"status":"covered"}]`, want: "coverage identity"},
		{name: "contradictory duplicate status", ledger: strings.Replace(validLedger, `"status":"covered"`, `"status":"planned","status":"covered"`, 1), want: "duplicate object keys"},
		{name: "sole uppercase status", ledger: strings.Replace(validLedger, `"status":"covered"`, `"STATUS":"covered"`, 1), want: "noncanonical key"},
		{name: "sole uppercase verdict", ledger: confirmedLedger, findings: strings.Replace(auditConfirmedFindingFixture, `"verdict":"confirmed"`, `"VERDICT":"confirmed"`, 1), want: "noncanonical key"},
		{name: "duplicate finding key", findings: `[{"fingerprint":"F","fingerprint":"F"}]`, want: "duplicate object keys"},
		{name: "bare finding", findings: `[{"fingerprint":"F"}]`, want: "incomplete finding"},
		{name: "noncanonical identity", ledger: strings.Replace(validLedger, `"coverage_id":"source::boundary::subsystem::attack"`, `"coverage_id":"forged"`, 1), want: "noncanonical coverage identity"},
		{name: "missing owner", ledger: strings.Replace(validLedger, `"agent_id":"hunter","reviewed_paths"`, `"agent_id":null,"reviewed_paths"`, 1), want: "assigned review evidence"},
		{name: "unchecked path", ledger: strings.Replace(validLedger, `"reviewed_paths":["source.txt"],"local_checks"`, `"reviewed_paths":["other.txt"],"local_checks"`, 1), want: "local check outside reviewed paths"},
		{name: "check without result", ledger: strings.Replace(validLedger, `"result":"The guard is present"`, `"result":""`, 1), want: "incomplete local check"},
		{name: "covered with unresolved", ledger: strings.Replace(validLedger, `"unresolved":[]}]`, `"unresolved":["ignored"]}]`, 1), want: "inconsistent status evidence"},
		{name: "archived attempt without evidence", ledger: strings.Replace(validLedger, `"attempts":[]`, `"attempts":[{"status":"covered"}]`, 1), want: "assigned review evidence"},
		{name: "archived attempt without wave", ledger: strings.Replace(withAttempt, `"wave":1,"status":"covered"`, `"status":"covered"`, 1), want: "invalid archived attempt wave"},
		{name: "archived attempt without reassignment", ledger: strings.Replace(withAttempt, `,"reassignment_reason":"Rechecked"`, ``, 1), want: "invalid archived attempt wave or reassignment"},
		{name: "reused archived owner", ledger: strings.Replace(withAttempt, `"agent_id":"older"`, `"agent_id":"hunter"`, 1), want: "reuses an archived assignment owner"},
		{name: "reused archived check owner", ledger: strings.Replace(withAttempt, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"older"`, 1), want: "reuses an archived assignment check owner"},
		{name: "reused archived artifact in current wave", ledger: strings.Replace(withLocalChecks, `"artifact":"agents/helper/artifacts/new.txt"`, `"artifact":"agents/helper/artifacts/old.txt"`, 1), want: "reuses an archived local check artifact"},
		{name: "reused archived artifact in later attempt", ledger: strings.Replace(withTwoAttempts, `"artifact":"agents/helper/artifacts/new.txt"`, `"artifact":"agents/helper/artifacts/old.txt"`, 1), want: "reuses an archived local check artifact"},
		{name: "current wave precedes archive", ledger: strings.Replace(withAttempt, `"wave":2,"status":"covered"`, `"wave":1,"status":"covered"`, 1), want: "invalid archived attempt wave"},
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
