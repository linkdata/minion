package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
)

func TestPreparedActionRejectsForeignResultBeforePublication(t *testing.T) {
	base, _, _ := gitFixtureRemote(t)
	installActionTestSkills(t, testReviewSkillURL)
	writeActionTestSkills(t, os.Getenv("HOME"), ".claude", testReviewSkillURL)
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case serveIssueFixture(t, w, r):
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			writes++
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testGitHub(server, "github-secret")
	e := &Orchestrator{WorkRoot: t.TempDir(), GitHub: client, Main: "claude", ClaudeModel: "portable-claude-model", ClaudeEffort: "high", Timeout: 2 * time.Minute,
		BotLogin: "linkdataminion",
		Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{testReviewSkillURL}}},
		Runner: Runner{
			WorkRoot: t.TempDir(),
			Claude:   config.ClaudeConfig{APIKey: "model-secret"}, runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
				dir, prompt := modelRequest.dir, modelRequest.prompt
				if modelRequest.provider != "claude" || modelRequest.model != "portable-claude-model" || modelRequest.effort != "high" {
					t.Fatalf("lost actual Claude role settings: %+v", modelRequest)
				}
				assertPortableSource(t, dir)
				if !strings.Contains(prompt, base) {
					t.Fatal("prompt lost source commit")
				}
				return aicli.Result{Answer: "No findings."}, nil
			},
		},
	}
	request := usageJob()
	request.Repository.Actions = []string{"audit"}
	request.Source.Author.ID = 3
	request.Command = command.Command{Name: "audit", Action: command.Comment}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e = attemptFixture(e)
	installedHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	prepared, err := attemptFixture(e).prepare(ctx, request)
	t.Setenv("HOME", installedHome)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.checkout != nil || !prepared.needsExecution() {
		t.Fatal("read-only preparation retained its clone or lost the computation job")
	}
	entries, err := os.ReadDir(e.WorkRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only preparation clone was not removed: %v, %v", entries, err)
	}
	if prepared.job.RuntimeBudget <= 0 || prepared.job.RuntimeBudget > time.Minute {
		t.Fatalf("accepted budget %s exceeds context deadline despite 2m configured timeout", prepared.job.RuntimeBudget)
	}
	if prepared.job.Main.Provider != "claude" || prepared.job.Main.Model != "portable-claude-model" || prepared.job.Main.Effort != "high" {
		t.Fatalf("portableJob lost Claude model/effort: %+v", prepared.job.Main)
	}
	data, err := json.Marshal(prepared.job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), e.Runner.WorkRoot) || strings.Contains(string(data), e.WorkRoot) {
		t.Fatal("portable job contains credentials or host paths")
	}
	var portable Job
	if err := json.Unmarshal(data, &portable); err != nil {
		t.Fatal(err)
	}
	result, err := e.Runner.Run(ctx, portable)
	if err != nil {
		t.Fatal(err)
	}
	bad := result
	bad.Version++
	if err := e.publish(ctx, prepared, bad); err == nil {
		t.Fatal("invalid result published")
	}
	if writes != 0 {
		t.Fatal("invalid result issued a GitHub write")
	}
	if err := e.publish(ctx, prepared, result); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("publication writes = %d", writes)
	}
}

func TestFailedAuditRetainsLabelledPartialArtifactsWithoutPublication(t *testing.T) {
	gitFixtureRemote(t)
	installActionTestSkills(t, testAuditSkillURL)
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			t.Errorf("failed audit attempted publication: %s %s", r.Method, r.URL.Path)
			http.Error(w, "publication forbidden", http.StatusForbidden)
			return
		}
		switch {
		case serveIssueFixture(t, w, r):
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testGitHub(server, "token")
	var journal bytes.Buffer
	failure := errors.New("private provider failure detail")
	const report = "Private incomplete findings requiring validation."
	var sourceDir, reportDir string
	e := &Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0), GitHub: client, Commands: map[string]config.CommandMapping{"security-audit": {Action: "security-audit", Skills: []string{testAuditSkillURL}}},
		Runner: Runner{
			Logger: log.New(&journal, "", 0), runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
				dir, output := modelRequest.dir, modelRequest.outputDir
				sourceDir, reportDir = dir, output
				if err := os.WriteFile(filepath.Join(output, "REPORT.md"), []byte(report), 0600); err != nil {
					t.Fatal(err)
				}
				return aicli.Result{}, failure
			},
		}}
	request := usageJob()
	request.Repository.Actions = []string{"security-audit"}
	request.Source.Author.ID = 3
	request.Command = command.Command{Name: "security-audit", Action: command.SecurityAudit}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := attemptFixture(e).perform(ctx, request); !errors.Is(err, failure) {
		t.Fatalf("audit failure=%v", err)
	}
	if writes != 0 {
		t.Fatalf("publication writes=%d", writes)
	}
	dirs, err := filepath.Glob(filepath.Join(e.WorkRoot, "audits", "incomplete-*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("incomplete audit directories=%v,%v", dirs, err)
	}
	data, err := os.ReadFile(filepath.Join(dirs[0], "REPORT.md"))
	if err != nil || string(data) != report {
		t.Fatalf("partial report=%q,%v", data, err)
	}
	marker, err := os.ReadFile(filepath.Join(dirs[0], "INCOMPLETE.txt"))
	if err != nil || !strings.Contains(string(marker), "must not be published") {
		t.Fatalf("incomplete marker=%q,%v", marker, err)
	}
	for _, dir := range []string{sourceDir, reportDir} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worker directory retained: %s,%v", dir, err)
		}
	}
	if !strings.Contains(journal.String(), "job="+request.ID) || !regexp.MustCompile(`attempt=[0-9a-f]{32} executor=local code=audit_incomplete`).MatchString(journal.String()) {
		t.Fatalf("missing identified failure diagnostic: %s", &journal)
	}
	if strings.Contains(journal.String(), report) || strings.Contains(journal.String(), failure.Error()) || strings.Contains(journal.String(), "collected artifacts are incomplete") {
		t.Fatalf("private discussion journaled: %s", &journal)
	}
}

func TestPublishFixWithoutChangesPostsNormalReply(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			captureComment(t, w, r, &body)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testGitHub(server, "token")
	e := Orchestrator{GitHub: client}
	request := usageJob()
	request.Source = github.Item{}
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	if err := e.publishFix(context.Background(), &preparedAction{request: request}, Result{Answer: "Tests passed."}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "without changing files") || !strings.Contains(body, "Tests passed.") || !strings.Contains(body, request.Marker()) {
		t.Fatalf("no-change reply=%q", body)
	}
}

func testActionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestPortableJobRequiresExistingActionDeadline(t *testing.T) {
	if _, err := (&Orchestrator{}).portableJob(context.Background(), nil, "", ""); err == nil || !strings.Contains(err.Error(), "requires a deadline") {
		t.Fatalf("preparation created a fresh budget: %v", err)
	}
}

type expiredPreparationContext struct {
	context.Context
	deadline time.Time
}

func (c expiredPreparationContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestPortableJobPreservesElapsedDeadlineCause(t *testing.T) {
	gitFixtureRemote(t)
	checkout := cloneFixture(t, t.TempDir())
	request := usageJob()
	request.Command.Name = "audit"
	request.Command.Action = command.Comment
	orchestrator := attemptFixture(&Orchestrator{Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}})
	// Deadline expiration may precede propagation of the context's Err value.
	ctx := expiredPreparationContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	if ctx.Err() != nil {
		t.Fatal("fixture must model the deadline/Err propagation race")
	}
	_, err := orchestrator.portableJob(ctx, &preparedAction{checkout: checkout, request: request}, "title", "description")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), "invalid action runtime budget") {
		t.Fatalf("elapsed preparation deadline lost its cause: %v", err)
	}
}

func TestPreparedHistoryModifierReachesPortableSource(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprintf("history_%t", history), func(t *testing.T) {
			base, _, _ := gitFixtureRemote(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !serveIssueFixture(t, w, r) {
					t.Errorf("unexpected preparation request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			request := usageJob()
			request.Command = command.Command{Name: "inspect", Action: command.Comment, History: history}
			orchestrator := attemptFixture(&Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(server, "token"), Commands: map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}})
			prepared, err := orchestrator.prepare(testActionContext(t), request)
			if err != nil {
				t.Fatal(err)
			}
			job := prepared.job
			if job.Source.HistoryRequested != history || (len(job.Source.History) != 0) != history || (history && !strings.Contains(string(job.Source.History), base)) {
				t.Fatalf("modifier lost during source preparation: history=%t snapshot=%+v", history, job.Source)
			}
		})
	}
}
