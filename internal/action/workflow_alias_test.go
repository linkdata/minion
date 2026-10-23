package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestExecutorJobCannotMutatePreparedAssignment(t *testing.T) {
	fixture := func() Job {
		return Job{
			Source: workspace.Snapshot{
				Files:        []workspace.File{{Path: "source.txt", Data: []byte("source")}},
				BaseFiles:    []workspace.File{{Path: "base.txt", Data: []byte("base")}},
				Diff:         []byte("diff"),
				ChangedPaths: []string{"source.txt"},
				Repository:   []byte("bundle"),
				Omitted:      []string{"omitted.txt"},
			},
			Main:   RoleSettings{Skills: []string{"main"}, AlwaysOnSkills: []string{"always"}},
			Critic: &RoleSettings{Skills: []string{"critic"}, AlwaysOnSkills: []string{"critic-always"}},
		}
	}
	job, want := fixture(), fixture()
	worker := cloneJobForExecutor(job)
	worker.Source.Files[0].Path = "forged.txt"
	worker.Source.Files[0].Data[0] = 'X'
	worker.Source.BaseFiles[0].Path = "forged-base.txt"
	worker.Source.BaseFiles[0].Data[0] = 'X'
	worker.Source.Diff[0] = 'X'
	worker.Source.ChangedPaths[0] = "forged.txt"
	worker.Source.Repository[0] = 'X'
	worker.Source.Omitted[0] = "forged-omitted.txt"
	worker.Main.Skills[0] = "forged-main"
	worker.Main.AlwaysOnSkills[0] = "forged-always"
	worker.Critic.Skills[0] = "forged-critic"
	worker.Critic.AlwaysOnSkills[0] = "forged-critic-always"
	if !reflect.DeepEqual(job, want) {
		t.Fatalf("worker changed prepared job: got=%+v want=%+v", job, want)
	}
}

func TestInProcessExecutorCannotPublishAuditForForgedSource(t *testing.T) {
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
	e := &Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(server, "token"), BotLogin: "bot",
		Commands: map[string]config.CommandMapping{"security-audit": {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}}},
	}
	executed := false
	e.Executor = executorFunc(func(_ context.Context, job Job, checkpoints Checkpoints) (Result, error) {
		executed = true
		original := cloneJobForExecutor(job)
		found := false
		for i := range job.Source.Files {
			if job.Source.Files[i].Path == "source.txt" {
				job.Source.Files[i].Path = "forged.txt"
				found = true
				break
			}
		}
		if !found {
			t.Fatal("fixture source file is missing")
		}
		result := completeAuditResult(job)
		for i := range result.Artifacts {
			if result.Artifacts[i].Path == "coverage-ledger.json" {
				result.Artifacts[i].Data = []byte(strings.ReplaceAll(auditCoveredLedgerFixture, "source.txt", "forged.txt"))
			}
		}
		result.Calls[0].Complete = true
		call := result.Calls[0]
		fact := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head, Call: callMetadata(call)}
		if err := checkpoints.BeforeCall(fact); err != nil {
			t.Fatal(err)
		}
		fact.Call = call
		if err := checkpoints.AfterCall(fact); err != nil {
			t.Fatal(err)
		}
		if err := result.Validate(job); err != nil {
			t.Fatalf("forged audit is invalid even against worker's job: %v", err)
		}
		if err := result.Validate(original); err == nil || !strings.Contains(err.Error(), "invalid starting path") {
			t.Fatalf("forged audit did not differ from prepared assignment: %v", err)
		}
		return result, nil
	})
	request := usageJob()
	request.Command = command.Command{Name: "security-audit", Action: command.SecurityAudit}
	request.Repository.Actions = []string{"security-audit"}
	if err := e.Execute(testActionContext(t), request); !errors.Is(err, monitor.ErrTerminalJob) || writes.Load() != 0 {
		t.Fatalf("forged audit crossed publication boundary: error=%v writes=%d", err, writes.Load())
	}
	if !executed {
		t.Fatal("executor was not reached")
	}
}
