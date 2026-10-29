package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHostileHTTPAuditSchemaCannotPublish(t *testing.T) {
	const rejectedWithoutReason = `[{"verdict":"rejected","fingerprint":"F","title":"Guard bypass refuted","description":"The alleged bypass does not reach the operation.","claimed_root_cause":"The guard was claimed to be absent.","trace":[{"kind":"entrypoint","file":"source.txt","line":1,"scope":"source","description":"The request reaches the guard."}],"evidence":[{"file":"source.txt","line":1,"description":"The guard executes before the operation."}] }]`
	candidateLedger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
	candidateLedger = strings.Replace(candidateLedger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	ownerless := strings.Replace(candidateLedger, `,{"agent_id":"hunter","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}`, "", 1)
	selfChecked := strings.Replace(candidateLedger, `"local_checks":[{"agent_id":"verifier"`, `"local_checks":[{"agent_id":"hunter"`, 1)
	archivedSelf := `{"wave":1,"status":"candidate","agent_id":"older","reviewed_paths":["source.txt"],"local_checks":[{"agent_id":"older","reviewed_paths":["source.txt"],"invariant":"Access is checked","method":"source","result":"The guard is present","artifact":null}],"result_fingerprints":["F"],"unresolved":[],"reassignment_reason":"Rechecked"}`
	archivedSelf = strings.Replace(auditCoveredLedgerFixture, `"attempts":[]`, `"attempts":[`+archivedSelf+`]`, 1)
	archivedSelf = strings.Replace(archivedSelf, `"wave":1,"status":"covered","agent_id":"hunter"`, `"wave":2,"status":"covered","agent_id":"hunter"`, 1)
	citesOther := strings.Replace(auditCandidateFindingFixture, `"description":"Input reaches the boundary."}]`, `"description":"Input reaches the boundary."},{"kind":"sink","file":"other.txt","line":1,"scope":"other","description":"The operation changes state."}]`, 1)
	blockedLedger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"blocked"`, 1)
	blockedLedger = strings.Replace(blockedLedger, `"unresolved":[]}]`, `"unresolved":["Investigate boundary"]}]`, 1)
	budgetBlocked := strings.Replace(blockedLedger, `"unresolved":["Investigate boundary"]`, `"unresolved":["validation_budget_exhausted"]`, 1)
	budgetBlocked = strings.Replace(budgetBlocked, `"coverage_id":"source::boundary::`, `"coverage_id":"source::budget::`, 1)
	budgetBlocked = strings.Replace(budgetBlocked, `"boundary":"boundary"`, `"boundary":"budget"`, 1)
	budgetBlocked = strings.Replace(budgetBlocked, `"boundary":"Boundary"`, `"boundary":"Budget boundary"`, 1)
	budgetBlocked = "[" + strings.Trim(budgetBlocked, "[]") + "," + strings.Trim(auditCoveredLedgerFixture, "[]") + "]"
	reservedBudgetBlocked := strings.Replace(budgetBlocked, "validation_budget_exhausted", "budget_cannot_reserve_critics_and_validation", 1)
	excludedLedger := auditOutOfScopeLedgerFixture()
	for _, test := range []struct {
		name, artifact, data, findings, want        string
		candidate, incompleteReason, retainEvidence bool
		sourceData                                  []byte
	}{
		{name: "directory-only review cannot claim coverage", artifact: "coverage-ledger.json", data: strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "src"), want: "starting path without a declared reviewed file"},
		{name: "blocked-only ledger has no reviewed coverage", artifact: "coverage-ledger.json", data: blockedLedger, want: "no reviewed coverage"},
		{name: "archived covered attempt cannot publish blocked current coverage", artifact: "coverage-ledger.json", data: auditArchivedCoveredBlockedLedgerFixture(), want: "no reviewed coverage"},
		{name: "complete ledger declares exhausted validation budget", artifact: "coverage-ledger.json", data: budgetBlocked, want: "incomplete audit budget"},
		{name: "complete ledger cannot reserve critics and validation", artifact: "coverage-ledger.json", data: reservedBudgetBlocked, want: "incomplete audit budget"},
		{name: "reserved audit agent ID", artifact: "coverage-ledger.json", data: strings.Replace(auditCoveredLedgerFixture, `"agent_id":"hunter"`, `"agent_id":"con"`, 1), want: "lacks assigned review evidence"},
		{name: "excluded-only ledger cannot be complete", artifact: "coverage-ledger.json", data: excludedLedger, want: "out-of-scope unit in complete audit"},
		{name: "covered ledger with current out-of-scope unit cannot publish", artifact: "coverage-ledger.json", data: auditCoveredAndOutOfScopeLedgerFixture(), want: "out-of-scope unit in complete audit"},
		{name: "blank architecture", artifact: "architecture.md", data: " \t\r\n", want: "architecture.md"},
		{name: "invisible report", artifact: "REPORT.md", data: "\u200b", want: "no visible content"},
		{name: "missing candidate validation plan", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`,"validation_plan":{"local":"Inspect the guard path in an approved test."}`, ``, 1), want: "incomplete needs_validation finding", candidate: true},
		{name: "rejected without reason", artifact: "findings.json", data: rejectedWithoutReason, want: "incomplete rejected finding", candidate: true},
		{name: "invalid trace order", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`"kind":"entrypoint"`, `"kind":"propagation"`, 1), want: "invalid trace evidence", candidate: true},
		{name: "ghost current trace", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`"trace":[{"kind":"entrypoint","file":"source.txt"`, `"trace":[{"kind":"entrypoint","file":"ghost.go"`, 1), want: "trace outside current source", candidate: true},
		{name: "line beyond current source", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`"evidence":[{"file":"source.txt","line":1`, `"evidence":[{"file":"source.txt","line":999999`, 1), want: "evidence outside current source", candidate: true},
		{name: "binary source citation", findings: auditCandidateFindingFixture, want: "trace outside current source", candidate: true, sourceData: []byte{'x', 0, '\n'}},
		{name: "missing coverage context", artifact: "coverage-ledger.json", data: strings.Replace(auditCoveredLedgerFixture,
			`"ordinary_attack_class_block":null,`, ``, 1), want: "required coverage context"},
		{name: "unverifiable local proof", artifact: "coverage-ledger.json", data: strings.Replace(auditCoveredLedgerFixture,
			`"method":"source","result":"The guard is present","artifact":null`,
			`"method":"local","result":"The guard is present","artifact":"agents/hunter/artifacts/proof.txt"`, 1), want: "unsupported local check method"},
		{name: "unreviewed cited sink", artifact: "findings.json", data: citesOther, want: "cited file without one declared verifier path set", candidate: true},
		{name: "candidate without owner check", artifact: "coverage-ledger.json", data: ownerless, findings: auditCandidateFindingFixture, want: "lacks assigned owner's local check", candidate: true},
		{name: "candidate without verifier", artifact: "coverage-ledger.json", data: selfChecked, findings: auditCandidateFindingFixture, want: "without separately attributed source check"},
		{name: "archived candidate without verifier", artifact: "coverage-ledger.json", data: archivedSelf, findings: auditCandidateFindingFixture, want: "without separately attributed source check"},
		{name: "archived candidate in not applicable unit", artifact: "coverage-ledger.json", data: auditArchivedCandidateNotApplicableLedgerFixture(), findings: auditCandidateFindingFixture, want: "without applicable candidate", retainEvidence: true},
		{name: "prior run unsupported", artifact: "coverage-ledger.json", data: strings.Replace(auditCoveredLedgerFixture, `"prior_status":"none"`, `"prior_status":"prior_confirmed_same_source"`, 1), want: "prior run absent from metadata"},
		{name: "audit JSON nesting exceeds decoder limit", artifact: "run-metadata.json", data: strings.Repeat("[", 10_001) + "0" + strings.Repeat("]", 10_001), want: "audit JSON is invalid"},
		{name: "complete metadata has incomplete reason", incompleteReason: true, want: "incomplete reason"},
	} {
		t.Run(test.name, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			sourceData := test.sourceData
			if sourceData == nil {
				sourceData = []byte("source\n")
			}
			gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: sourceData}, workspace.File{Path: "other.txt", Mode: workspace.Regular, Data: []byte("other\n")}, workspace.File{Path: "src/entry.go", Mode: workspace.Regular, Data: []byte("entry\n")})
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveIssueFixture(t, w, r) {
					return
				}
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			workRoot, usageDir := t.TempDir(), filepath.Join(t.TempDir(), "usage")
			e := &Orchestrator{WorkRoot: workRoot, Usage: &usage.Store{Dir: usageDir}, GitHub: testGitHub(server, "token"), BotLogin: "bot",
				Commands: map[string]config.CommandMapping{"security-audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}}},
			}
			executed := false
			e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
				job.Source.BaseFiles = nil // The issue audit has no base; preserve that across the HTTP JSON round trip.
				executed = true
				admitFixtureAssignment(t, checkpoints, job)
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				before := f.call(1)
				f.ack(t, "/before", before, http.StatusOK, 1)
				call := before.Checkpoint.Call
				call.Tokens, call.Complete = usage.Tokens{Input: 9, Output: 2}, true
				f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
				outcome := f.outcome(3, nil)
				outcome.Result = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Hostile audit report: Unverified boundary; Missing guard medium.", Calls: []CallUsage{call},
					Artifacts: auditArtifactsFixture(job, sourceIdentity(job), "Hostile audit report: Unverified boundary; Missing guard medium.")}
				if test.candidate {
					for i := range outcome.Result.Artifacts {
						if outcome.Result.Artifacts[i].Path == "coverage-ledger.json" {
							outcome.Result.Artifacts[i].Data = []byte(candidateLedger)
						}
					}
				}
				if test.findings != "" {
					for i := range outcome.Result.Artifacts {
						if outcome.Result.Artifacts[i].Path == "findings.json" {
							outcome.Result.Artifacts[i].Data = []byte(test.findings)
						}
						if outcome.Result.Artifacts[i].Path == "FINDINGS-DETAIL.md" && strings.Contains(test.findings, `"verdict":"confirmed"`) {
							outcome.Result.Artifacts[i].Data = []byte(auditConfirmedDetailFixture)
						}
						if outcome.Result.Artifacts[i].Path == "NEEDS-VALIDATION.md" && strings.Contains(test.findings, `"verdict":"needs_validation"`) {
							outcome.Result.Artifacts[i].Data = []byte(auditNeedsDetailFixture)
						}
					}
				}
				for i := range outcome.Result.Artifacts {
					if outcome.Result.Artifacts[i].Path == test.artifact {
						outcome.Result.Artifacts[i].Data = []byte(test.data)
					}
					if test.incompleteReason && outcome.Result.Artifacts[i].Path == "run-metadata.json" {
						outcome.Result.Artifacts[i].Data = []byte(strings.Replace(string(outcome.Result.Artifacts[i].Data), `"run_status":"complete"`, `"run_status":"complete","incomplete_reason":"validation_budget_exhausted"`, 1))
					}
				}
				if test.artifact == "REPORT.md" {
					outcome.Result.Answer = test.data
				}
				if err := httpResultShape(outcome.Result); err != nil {
					t.Fatalf("hostile audit failed wire shape: %v", err)
				}
				if err := outcome.Result.Validate(job); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("hostile audit passed parent validation: %v", err)
				}
				ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 3)
				if ack.Failure == nil || !ack.Failure.Terminal || !strings.Contains(ack.Failure.Message, test.want) {
					t.Fatalf("selected outcome did not reject invalid audit: %+v", ack.Failure)
				}
				got := f.result(t)
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: "security-audit", Action: command.SecurityAudit}
			request.Repository.Actions = []string{"security-audit"}
			selectFixtureRequest(t, e, &request, false)
			if err := e.Execute(testActionContext(t), request); !errors.Is(err, monitor.ErrTerminalJob) || writes.Load() != 0 {
				t.Fatalf("hostile audit crossed publication boundary: error=%v writes=%d", err, writes.Load())
			}
			if !executed {
				t.Fatal("the HTTP executor was not reached")
			}
			records, err := usage.ReadAll(usageDir)
			if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].Tokens != (usage.Tokens{Input: 9, Output: 2}) {
				t.Fatalf("private failed-usage evidence missing: records=%+v error=%v", records, err)
			}
			if complete, err := filepath.Glob(filepath.Join(workRoot, "audits", "complete-*")); err != nil || len(complete) != 0 {
				t.Fatalf("invalid audit became complete: paths=%v error=%v", complete, err)
			}
			incomplete, err := filepath.Glob(filepath.Join(workRoot, "audits", "incomplete-*"))
			if err != nil || len(incomplete) != 1 {
				t.Fatalf("private incomplete audit missing: paths=%v error=%v", incomplete, err)
			}
			marker, err := os.ReadFile(filepath.Join(incomplete[0], "INCOMPLETE.txt"))
			if err != nil || !strings.Contains(string(marker), "must not be published") {
				t.Fatalf("private failure marker missing: %q error=%v", marker, err)
			}
			if test.retainEvidence {
				for path, want := range map[string]string{"coverage-ledger.json": test.data, "findings.json": test.findings} {
					data, err := os.ReadFile(filepath.Join(incomplete[0], path))
					if err != nil || string(data) != want {
						t.Fatalf("incomplete audit lost %s: bytes=%d error=%v", path, len(data), err)
					}
				}
			}
		})
	}
}
