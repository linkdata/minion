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
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHostileHTTPAuditRunsPublishingHostValidator(t *testing.T) {
	if err := processgroup.Check(); err != nil {
		t.Skipf("delegated process ownership unavailable: %v", err)
	}
	installActionTestSkills(t, testAuditSkillURL)
	skillDir := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit")
	validator := `const findings = JSON.parse(require("node:fs").readFileSync(process.argv[2], "utf8"));
if (findings.some(finding => finding.unexpected === true)) process.exit(17);
`
	if err := os.WriteFile(filepath.Join(skillDir, "validate-findings.cjs"), []byte(validator), 0600); err != nil {
		t.Fatal(err)
	}
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
	workRoot := t.TempDir()
	e := &Orchestrator{WorkRoot: workRoot, GitHub: testGitHub(server, "token"), BotLogin: "bot",
		Commands: map[string]config.CommandMapping{"security-audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}}},
	}
	e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
		job.Source.BaseFiles = nil // The issue audit has no base; preserve that across the HTTP JSON round trip.
		f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
		f.pull(t)
		before := f.call(1)
		f.ack(t, "/before", before, http.StatusOK, 1)
		call := before.Checkpoint.Call
		call.Tokens, call.Complete = usage.Tokens{Input: 9, Output: 2}, true
		f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
		outcome := f.outcome(3, nil)
		outcome.Result = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Hostile audit report: Unverified boundary", Calls: []CallUsage{call},
			Artifacts: auditArtifactsFixture(job, sourceIdentity(job), "Hostile audit report: Unverified boundary")}
		for i := range outcome.Result.Artifacts {
			switch outcome.Result.Artifacts[i].Path {
			case "findings.json":
				outcome.Result.Artifacts[i].Data = []byte(strings.Replace(auditCandidateFindingFixture, `"fingerprint":"F"`, `"fingerprint":"F","unexpected":true`, 1))
			case "coverage-ledger.json":
				ledger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
				outcome.Result.Artifacts[i].Data = []byte(strings.Replace(ledger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1))
			}
		}
		if err := outcome.Result.Validate(job); err != nil {
			t.Fatalf("test report should pass Go contract before host validator: %v", err)
		}
		f.ack(t, "/outcome", outcome, http.StatusOK, 3)
		got := f.result(t)
		return got.result, got.err
	})
	request := usageJob()
	request.Command = command.Command{Name: "security-audit", Action: command.SecurityAudit}
	request.Repository.Actions = []string{"security-audit"}
	err := e.Execute(testActionContext(t), request)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "validate-findings.cjs failed: exit status 17") || writes.Load() != 0 {
		t.Fatalf("hostile audit crossed the parent validator: error=%v writes=%d", err, writes.Load())
	}
	if complete, err := filepath.Glob(filepath.Join(workRoot, "audits", "complete-*")); err != nil || len(complete) != 0 {
		t.Fatalf("invalid audit became complete: paths=%v error=%v", complete, err)
	}
	if incomplete, err := filepath.Glob(filepath.Join(workRoot, "audits", "incomplete-*")); err != nil || len(incomplete) != 1 {
		t.Fatalf("invalid audit was not retained as incomplete: paths=%v error=%v", incomplete, err)
	}
	if scratch, err := filepath.Glob(filepath.Join(workRoot, ".minion-audit-validation-*")); err != nil || len(scratch) != 0 {
		t.Fatalf("confirmed validator process left scratch files: paths=%v error=%v", scratch, err)
	}
}
