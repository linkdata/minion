package action

import (
	"context"
	"errors"
	"fmt"
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

func amplifiedAuditLedger() string {
	paths := strings.TrimSuffix(strings.Repeat(`"source.txt",`, 600), ",")
	fingerprints := strings.TrimSuffix(strings.Repeat(`"F",`, 600), ",")
	ledger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
	ledger = strings.Replace(ledger, `"reviewed_paths":["source.txt"],"invariant"`, `"reviewed_paths":[`+paths+`],"invariant"`, 1)
	return strings.Replace(ledger, `"result_fingerprints":[]`, `"result_fingerprints":[`+fingerprints+`]`, 1)
}

func amplifiedAuditAttemptsLedger() string {
	paths := make([]string, 100)
	for i := range paths {
		paths[i] = fmt.Sprintf(`"area/%03d"`, i)
	}
	const attempt = `{"status":"covered","reviewed_paths":["source.txt"]}`
	attempts := strings.TrimSuffix(strings.Repeat(attempt+",", 2600), ",")
	ledger := strings.Replace(auditCoveredLedgerFixture, `"starting_paths":["source.txt"]`, `"starting_paths":[`+strings.Join(paths, ",")+`]`, 1)
	return strings.Replace(ledger, `"attempts":[]`, `"attempts":[`+attempts+`]`, 1)
}

func TestAuditCoverageBoundsAndDuplicateSeeds(t *testing.T) {
	job := testJob(command.SecurityAudit)
	for _, test := range []struct{ name, ledger, findings, want string }{
		{name: "excessive JSON nesting", ledger: strings.Repeat("[", 10_001) + "0" + strings.Repeat("]", 10_001), want: "audit JSON is invalid"},
		{name: "duplicate seed", ledger: strings.Replace(auditCoveredLedgerFixture, `"starting_paths":["source.txt"]`, `"starting_paths":["source.txt","source.txt"]`, 1), want: "duplicate starting path"},
		{name: "excess evidence items", ledger: strings.Replace(auditCoveredLedgerFixture, `"starting_paths":["source.txt"]`, `"starting_paths":[`+strings.TrimSuffix(strings.Repeat(`"source.txt",`, 100_001), ",")+`]`, 1), want: "validation work limit"},
		{name: "candidate cross product", ledger: amplifiedAuditLedger(), findings: auditCandidateFindingFixture, want: "repeated validation work limit"},
		{name: "seed index repeated for attempts", ledger: amplifiedAuditAttemptsLedger(), want: "repeated validation work limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if len(test.ledger) > maxAuditArtifactBytes {
				t.Fatalf("hostile ledger exceeds wire limit: %d", len(test.ledger))
			}
			result := completeAuditResult(job)
			for i := range result.Artifacts {
				switch result.Artifacts[i].Path {
				case "coverage-ledger.json":
					result.Artifacts[i].Data = []byte(test.ledger)
				case "findings.json":
					if test.findings != "" {
						result.Artifacts[i].Data = []byte(test.findings)
					}
				case "NEEDS-VALIDATION.md":
					if test.findings != "" {
						result.Artifacts[i].Data = []byte(auditNeedsDetailFixture)
					}
				}
			}
			if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("hostile Result.Validate = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAuditSeedsCheckedUsesReviewedFiles(t *testing.T) {
	const count = 10_000
	seeds, reviewed := make([]string, count), make([]string, count)
	lines := make(map[string]int, count)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("area/%05d", i)
		path := seeds[i] + "/entry.go"
		reviewed[count-1-i] = path
		lines[path] = 1
	}
	if !auditSeedsChecked(seeds, reviewed, lines) {
		t.Fatal("reviewed descendants did not cover starting paths")
	}
	if auditSeedsChecked([]string{"area/00000"}, []string{"area/000001/entry.go"}, nil) {
		t.Fatal("partial path component counted as coverage")
	}
	if auditSeedsChecked([]string{"area/00000"}, []string{"area/00000"}, lines) {
		t.Fatal("directory-only reviewed path counted as current source file")
	}
	if !auditSeedsChecked([]string{"area/00000"}, []string{"area/00000"}, nil) {
		t.Fatal("offline ledger-only validation rejected declared path")
	}
}

func TestHostileHTTPAuditWorkBudgetCannotPublish(t *testing.T) {
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
	e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
		job.Source.BaseFiles = nil
		f := newHTTPServerTest(t, ctx, job, checkpoints, nil)
		f.pull(t)
		before := f.call(1)
		f.ack(t, "/before", before, http.StatusOK, 1)
		call := before.Checkpoint.Call
		call.Tokens, call.Complete = usage.Tokens{Input: 9, Output: 2}, true
		f.ack(t, "/after", httpCall{Sequence: 2, Checkpoint: CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, http.StatusOK, 2)
		outcome := f.outcome(3, nil)
		outcome.Result = Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Hostile audit report: validation work exceeded.", Calls: []CallUsage{call},
			Artifacts: auditArtifactsFixture(job, sourceIdentity(job), "Hostile audit report: validation work exceeded.")}
		for i := range outcome.Result.Artifacts {
			switch outcome.Result.Artifacts[i].Path {
			case "coverage-ledger.json":
				outcome.Result.Artifacts[i].Data = []byte(amplifiedAuditLedger())
			case "findings.json":
				outcome.Result.Artifacts[i].Data = []byte(auditCandidateFindingFixture)
			case "NEEDS-VALIDATION.md":
				outcome.Result.Artifacts[i].Data = []byte(auditNeedsDetailFixture)
			}
		}
		if err := httpResultShape(outcome.Result); err != nil {
			t.Fatalf("hostile audit failed HTTP shape: %v", err)
		}
		if err := outcome.Result.Validate(job); err == nil || !strings.Contains(err.Error(), "repeated validation work limit") {
			t.Fatalf("hostile audit passed Result.Validate: %v", err)
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
	if data, err := os.ReadFile(filepath.Join(incomplete[0], "coverage-ledger.json")); err != nil || !strings.Contains(string(data), `"result_fingerprints"`) {
		t.Fatalf("private hostile evidence missing: bytes=%d error=%v", len(data), err)
	}
}
