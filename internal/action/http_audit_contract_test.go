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
	for _, test := range []struct {
		name, artifact, data, want string
		candidate                  bool
	}{
		{name: "missing candidate validation plan", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`,"validation_plan":{"local":"Inspect the guard path in an approved test."}`, ``, 1), want: "incomplete needs_validation finding", candidate: true},
		{name: "rejected without reason", artifact: "findings.json", data: rejectedWithoutReason, want: "incomplete rejected finding", candidate: true},
		{name: "invalid trace order", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`"kind":"entrypoint"`, `"kind":"propagation"`, 1), want: "invalid trace evidence", candidate: true},
		{name: "ghost current trace", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`"trace":[{"kind":"entrypoint","file":"source.txt"`, `"trace":[{"kind":"entrypoint","file":"ghost.go"`, 1), want: "trace outside current source", candidate: true},
		{name: "line beyond current source", artifact: "findings.json", data: strings.Replace(auditCandidateFindingFixture,
			`"evidence":[{"file":"source.txt","line":1`, `"evidence":[{"file":"source.txt","line":999999`, 1), want: "evidence outside current source", candidate: true},
		{name: "missing coverage context", artifact: "coverage-ledger.json", data: strings.Replace(auditCoveredLedgerFixture,
			`"ordinary_attack_class_block":null,`, ``, 1), want: "required coverage context"},
		{name: "unverifiable local proof", artifact: "coverage-ledger.json", data: strings.Replace(auditCoveredLedgerFixture,
			`"method":"source","result":"The guard is present","artifact":null`,
			`"method":"local","result":"The guard is present","artifact":"agents/hunter/artifacts/proof.txt"`, 1), want: "unsupported local check method"},
	} {
		t.Run(test.name, func(t *testing.T) {
			installActionTestSkills(t, testAuditSkillURL)
			gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
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
				f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
				f.pull(t)
				before := f.call(1)
				f.ack(t, "/before", before, http.StatusOK, 1)
				call := before.Checkpoint.Call
				call.Tokens, call.Complete = usage.Tokens{Input: 9, Output: 2}, true
				f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
				outcome := f.outcome(3, nil)
				outcome.Result = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Hostile audit report", Calls: []CallUsage{call},
					Artifacts: auditArtifactsFixture(job, sourceIdentity(job), "Hostile audit report")}
				if test.candidate {
					candidate := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
					candidate = strings.Replace(candidate, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
					for i := range outcome.Result.Artifacts {
						if outcome.Result.Artifacts[i].Path == "coverage-ledger.json" {
							outcome.Result.Artifacts[i].Data = []byte(candidate)
						}
					}
				}
				for i := range outcome.Result.Artifacts {
					if outcome.Result.Artifacts[i].Path == test.artifact {
						outcome.Result.Artifacts[i].Data = []byte(test.data)
					}
				}
				if err := httpResultShape(outcome.Result); err != nil {
					t.Fatalf("hostile audit failed wire shape: %v", err)
				}
				if err := outcome.Result.Validate(job); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("hostile audit passed parent validation: %v", err)
				}
				f.ack(t, "/outcome", outcome, http.StatusOK, 3)
				got := f.result(t)
				return got.result, got.err
			})
			request := usageJob()
			request.Command = command.Command{Name: "security-audit", Action: command.SecurityAudit}
			request.Repository.Actions = []string{"security-audit"}
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
		})
	}
}
