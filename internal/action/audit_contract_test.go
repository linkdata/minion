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
	archivedCandidate := strings.Replace(archived, `"status":"covered"`, `"status":"candidate"`, 1)
	archivedCandidate = strings.Replace(archivedCandidate, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	archivedCandidate = strings.Replace(archivedCandidate, `"local_checks":[{"agent_id":"helper"`, `"local_checks":[{"agent_id":"older"`, 1)
	withSelfCheckedAttempt := strings.Replace(validLedger, `"attempts":[]`, `"attempts":[`+archivedCandidate+`]`, 1)
	withSelfCheckedAttempt = strings.Replace(withSelfCheckedAttempt, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	sourceCheck := `"method":"source","result":"The guard is present","artifact":null`
	localCheck := `"method":"local","result":"The guard is present","artifact":"agents/hunter/artifacts/proof.txt"`
	archivedLocalCheck := `"method":"local","result":"The guard is present","artifact":"agents/helper/artifacts/proof.txt"`
	confirmedLedger := strings.Replace(validLedger, `"status":"covered"`, `"status":"candidate"`, 1)
	confirmedLedger = strings.Replace(confirmedLedger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	selfCheckedCandidate := strings.Replace(confirmedLedger, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"hunter"`, 1)
	rejectedFinding := `[{"verdict":"rejected","fingerprint":"F","title":"Guard bypass refuted","description":"The alleged bypass does not reach the operation.","claimed_root_cause":"The guard was claimed to be absent.","trace":[{"kind":"entrypoint","file":"source.txt","line":1,"scope":"source","description":"The request reaches the guard."}],"evidence":[{"file":"source.txt","line":1,"description":"The guard executes before the operation."}],"reason":"The request is stopped by the guard."}]`
	withContext := strings.Replace(validLedger, `"ordinary_attack_class_block":null`, `"ordinary_attack_class_block":"The ordinary attack path is covered."`, 1)
	withContext = strings.Replace(withContext, `"selected_companion_blocks":[]`, `"selected_companion_blocks":["Companion A"]`, 1)
	withContext = strings.Replace(withContext, `"excluded_blocks":[]`, `"excluded_blocks":[{"block":"Companion B","reason":"Outside this boundary."}]`, 1)
	for _, test := range []struct {
		name, ledger, findings, report, want string
	}{
		{name: "covered"},
		{name: "covered with populated context", ledger: withContext},
		{name: "covered with archived attempt and distinct check owners", ledger: withAttempt},
		{name: "unsupported local check in current evidence", ledger: strings.Replace(validLedger, sourceCheck, localCheck, 1), want: "unsupported local check method"},
		{name: "unsupported local check in archived evidence", ledger: strings.Replace(withAttempt, sourceCheck, archivedLocalCheck, 1), want: "unsupported local check method"},
		{name: "decimal spelling of integer wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":1.0`, 1)},
		{name: "exponent spelling of integer wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":1e0`, 1)},
		{name: "decimal spelling of archived wave", ledger: strings.Replace(withAttempt, `"wave":1`, `"wave":1.0`, 1)},
		{name: "fractional archived wave", ledger: strings.Replace(withAttempt, `"wave":1`, `"wave":1.5`, 1), want: "invalid archived attempt wave"},
		{name: "fractional current wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":1.5`, 1), want: "invalid current wave"},
		{name: "zero current wave", ledger: strings.Replace(validLedger, `"wave":1`, `"wave":0`, 1), want: "invalid current wave"},
		{name: "confirmed finding", ledger: confirmedLedger, findings: auditConfirmedFindingFixture},
		{name: "needs_validation finding", ledger: confirmedLedger, findings: auditCandidateFindingFixture},
		{name: "confirmed severity belongs to another line", ledger: confirmedLedger, findings: auditConfirmedFindingFixture, report: "Missing guard (low).\nMedium confidence.", want: "omits a final finding title or severity"},
		{name: "confirmed severity is not confidence text", ledger: confirmedLedger, findings: auditConfirmedFindingFixture, report: "| low | Missing guard | Boundary | medium confidence |", want: "omits a final finding title or severity"},
		{name: "no findings were omitted is not a clean claim", ledger: confirmedLedger, findings: auditCandidateFindingFixture, report: "No findings were omitted. Unverified boundary."},
		{name: "candidate lacks independent verifier", ledger: selfCheckedCandidate, findings: auditCandidateFindingFixture, want: "without independent verifier check"},
		{name: "archived candidate lacks independent verifier", ledger: withSelfCheckedAttempt, findings: auditCandidateFindingFixture, want: "without independent verifier check"},
		{name: "decimal spelling of integer line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":1.0`, 1)},
		{name: "exponent spelling of integer line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":1e0`, 1)},
		{name: "fractional trace line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":1.5`, 1), want: "invalid trace evidence"},
		{name: "zero trace line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"line":1`, `"line":0`, 1), want: "invalid trace evidence"},
		{name: "fractional evidence line", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"evidence":[{"file":"source.txt","line":1`, `"evidence":[{"file":"source.txt","line":1.5`, 1), want: "invalid source evidence"},
		{name: "rejected finding", ledger: confirmedLedger, findings: rejectedFinding},
		{name: "needs_validation without claimed cause", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"claimed_root_cause":"The guard may be absent.",`, ``, 1), want: "incomplete needs_validation finding"},
		{name: "needs_validation without blockers", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"blockers":["Runtime validation is unavailable."],`, ``, 1), want: "incomplete needs_validation finding"},
		{name: "needs_validation without validation plan", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `,"validation_plan":{"local":"Inspect the guard path in an approved test."}`, ``, 1), want: "incomplete needs_validation finding"},
		{name: "needs_validation invisible blocker", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"blockers":["Runtime validation is unavailable."]`, `"blockers":["\u200b"]`, 1), want: "incomplete needs_validation finding"},
		{name: "rejected without claimed cause", ledger: confirmedLedger, findings: strings.Replace(rejectedFinding, `"claimed_root_cause":"The guard was claimed to be absent.",`, ``, 1), want: "incomplete rejected finding"},
		{name: "rejected without reason", ledger: confirmedLedger, findings: strings.Replace(rejectedFinding, `,"reason":"The request is stopped by the guard."`, ``, 1), want: "incomplete rejected finding"},
		{name: "single propagation trace", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"kind":"entrypoint"`, `"kind":"propagation"`, 1), want: "invalid trace evidence"},
		{name: "wrong ordered trace", ledger: confirmedLedger, findings: strings.Replace(auditCandidateFindingFixture, `"trace":[{"kind":"entrypoint","file":"source.txt","line":1,"scope":"source","description":"Input reaches the boundary."}]`, `"trace":[{"kind":"sink","file":"source.txt","line":1,"scope":"source","description":"The request reaches the sink."},{"kind":"entrypoint","file":"source.txt","line":2,"scope":"source","description":"The request enters later."}]`, 1), want: "invalid trace evidence"},
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
		{name: "unchecked path", ledger: strings.Replace(validLedger, `"local_checks":[{"agent_id":"verifier","reviewed_paths":["source.txt"]`, `"local_checks":[{"agent_id":"verifier","reviewed_paths":["other.txt"]`, 1), want: "local check outside reviewed paths"},
		{name: "check without result", ledger: strings.Replace(validLedger, `"result":"The guard is present"`, `"result":""`, 1), want: "incomplete local check"},
		{name: "covered with unresolved", ledger: strings.Replace(validLedger, `"unresolved":[]}]`, `"unresolved":["ignored"]}]`, 1), want: "inconsistent status evidence"},
		{name: "missing ordinary block", ledger: strings.Replace(validLedger, `"ordinary_attack_class_block":null,`, ``, 1), want: "required coverage context"},
		{name: "invalid ordinary block type", ledger: strings.Replace(validLedger, `"ordinary_attack_class_block":null`, `"ordinary_attack_class_block":[]`, 1), want: "invalid ordinary attack class block"},
		{name: "missing selected blocks", ledger: strings.Replace(validLedger, `"selected_companion_blocks":[],`, ``, 1), want: "required coverage context"},
		{name: "null selected blocks", ledger: strings.Replace(validLedger, `"selected_companion_blocks":[]`, `"selected_companion_blocks":null`, 1), want: "required coverage context"},
		{name: "missing excluded blocks", ledger: strings.Replace(validLedger, `"excluded_blocks":[],`, ``, 1), want: "required coverage context"},
		{name: "null excluded blocks", ledger: strings.Replace(validLedger, `"excluded_blocks":[]`, `"excluded_blocks":null`, 1), want: "required coverage context"},
		{name: "missing prior status", ledger: strings.Replace(validLedger, `"prior_status":"none",`, ``, 1), want: "required coverage context"},
		{name: "prior run claim without metadata", ledger: strings.Replace(validLedger, `"prior_status":"none"`, `"prior_status":"prior_confirmed_same_source"`, 1), want: "prior run absent from metadata"},
		{name: "new claim without prior ledger", ledger: strings.Replace(validLedger, `"prior_status":"none"`, `"prior_status":"new"`, 1), want: "prior run absent from metadata"},
		{name: "invalid prior status", ledger: strings.Replace(validLedger, `"prior_status":"none"`, `"prior_status":"invented"`, 1), want: "prior run absent from metadata"},
		{name: "missing attempts", ledger: strings.Replace(validLedger, `"attempts":[],`, ``, 1), want: "required coverage context"},
		{name: "null attempts", ledger: strings.Replace(validLedger, `"attempts":[]`, `"attempts":null`, 1), want: "required coverage context"},
		{name: "archived attempt without evidence", ledger: strings.Replace(validLedger, `"attempts":[]`, `"attempts":[{"status":"covered"}]`, 1), want: "assigned review evidence"},
		{name: "archived attempt without wave", ledger: strings.Replace(withAttempt, `"wave":1,"status":"covered"`, `"status":"covered"`, 1), want: "invalid archived attempt wave"},
		{name: "archived attempt without reassignment", ledger: strings.Replace(withAttempt, `,"reassignment_reason":"Rechecked"`, ``, 1), want: "invalid archived attempt wave or reassignment"},
		{name: "reused archived owner", ledger: strings.Replace(withAttempt, `"agent_id":"older"`, `"agent_id":"hunter"`, 1), want: "reuses an archived assignment owner"},
		{name: "reused archived check owner", ledger: strings.Replace(withAttempt, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"older"`, 1), want: "reuses an archived assignment check owner"},
		{name: "reused archived verifier", ledger: strings.Replace(withAttempt, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"helper"`, 1), want: "reuses an archived assignment check owner"},
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
			if test.findings != "" {
				result.Answer = "Reviewed audit report: Unverified boundary; Missing guard (medium)."
			}
			if strings.Contains(test.findings, `"verdict":"confirmed"`) {
				result.Answer += "\n| Severity | Title | Boundary | Observed result |\n| --- | --- | --- | --- |\n| medium | Missing guard | Boundary | Result |"
			}
			if test.report != "" {
				result.Answer = test.report
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
