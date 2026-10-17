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
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestPortableJobUsesCriticDefaultAndRejectsInvalidFixSource(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint(collision), func(t *testing.T) {
			var files []workspace.File
			if collision {
				files = []workspace.File{{Path: "Docs/a", Mode: workspace.Regular}, {Path: "docs/b", Mode: workspace.Regular}}
			}
			gitFixtureRemote(t, files...)
			checkout, err := gitwork.Clone(context.Background(), t.TempDir(), "o/r", "main", func(_ context.Context, sha string) (github.GitTree, error) {
				truncated := false
				entries := []github.GitTreeEntry{}
				for _, file := range files {
					size := int64(len(file.Data))
					entries = append(entries, github.GitTreeEntry{Path: file.Path, Type: "blob", Mode: "100644", Size: &size})
				}
				return github.GitTree{SHA: sha, Truncated: &truncated, Tree: entries}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := checkout.Close(); err != nil {
					t.Error(err)
				}
			}()
			action := command.Comment
			if collision {
				action = command.Fix
			}
			request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7, Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Source: github.Item{Author: github.User{ID: 3, Login: "writer"}}, Command: command.Command{Name: string(action), Action: action}}
			orchestrator := Orchestrator{Adversarial: "claude", Commands: map[string]config.CommandMapping{string(action): {Action: string(action), Skills: []string{skills.BuiltinAntiPromptInjection}}}}
			job, err := orchestrator.portableJob(testActionContext(t), &preparedAction{checkout: checkout, request: request}, "title", "body")
			if collision {
				if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "directory spelling collision") {
					t.Fatalf("invalid fix source was not terminal: %v", err)
				}
			} else if err != nil || job.CriticMaxRounds != config.DefaultCriticMaxRounds {
				t.Fatalf("critic default=%d err=%v", job.CriticMaxRounds, err)
			}
		})
	}
}

func TestPreparedActionRejectsForeignResultBeforePublication(t *testing.T) {
	base, _, _ := gitFixtureRemote(t)
	installActionTestSkills(t, testReviewSkillURL)
	writeActionTestSkills(t, os.Getenv("HOME"), ".claude", testReviewSkillURL)
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"title":"Task","body":"Inspect source"}`)
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
	client := github.New("github-secret")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
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
	request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7,
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"audit"}},
		Source:     github.Item{Author: github.User{ID: 3, Login: "writer"}},
		Command:    command.Command{Name: "audit", Action: command.Comment},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e.Usage = &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	e, finish, err := e.trackUsage(request)
	if err != nil {
		t.Fatal(err)
	}
	installedHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	prepared, err := e.prepare(ctx, request)
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
	defer func() {
		if err := prepared.close(); err != nil {
			t.Error(err)
		}
	}()
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
	for _, mutate := range []func(*Result){
		func(r *Result) { r.Identity.AttemptID = "foreign" },
		func(r *Result) { r.SourceRef = strings.Repeat("f", 40) },
		func(r *Result) { r.Version++ },
	} {
		bad := result
		mutate(&bad)
		if err := e.publish(ctx, prepared, bad); err == nil {
			t.Fatal("invalid result published")
		}
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
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	records, err := usage.ReadAll(e.Usage.Dir)
	if err != nil || len(records) != 1 || records[0].ID != prepared.job.Identity.AttemptID || records[0].ClaudeCalls != 1 {
		t.Fatalf("attempt/usage correlation lost: records=%+v job=%+v err=%v", records, prepared.job.Identity, err)
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
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
		case r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main"}`)
		case r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"title":"Audit","body":"Inspect source"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	var journal bytes.Buffer
	failure := errors.New("private provider failure detail")
	const report = "Private incomplete findings requiring validation."
	var sourceDir, reportDir string
	e := &Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0), GitHub: client, Commands: map[string]config.CommandMapping{"security-audit": {Action: "security-audit", Skills: []string{testAuditSkillURL}}},
		Runner: Runner{
			WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0), runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
				dir, output := modelRequest.dir, modelRequest.outputDir
				sourceDir, reportDir = dir, output
				if err := os.WriteFile(filepath.Join(output, "REPORT.md"), []byte(report), 0600); err != nil {
					t.Fatal(err)
				}
				return aicli.Result{}, failure
			},
		}}
	request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7,
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"security-audit"}},
		Source:     github.Item{Author: github.User{ID: 3, Login: "writer"}}, Command: command.Command{Name: "security-audit", Action: command.SecurityAudit}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := e.perform(ctx, request); !errors.Is(err, failure) {
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

func TestIncompleteAuditIdentityAndPrivateDiagnosticLogging(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Artifacts:   []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: []byte("partial")}},
		Diagnostics: []Diagnostic{{Code: "limit_hit", Message: "private provider diagnostic must remain private"}}}
	if err := result.validate(job, false); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Result){
		func(r *Result) { r.Identity.AttemptID = "foreign" }, func(r *Result) { r.SourceRef = strings.Repeat("b", 40) }, func(r *Result) { r.Version++ },
	} {
		bad := result
		mutate(&bad)
		if bad.validate(job, false) == nil {
			t.Fatal("incomplete result bypassed source or attempt identity validation")
		}
	}
	var journal bytes.Buffer
	e := Orchestrator{Logger: log.New(&journal, "", 0)}
	e.logResult(job, result)
	for _, want := range []string{"job=" + job.Identity.JobID, "attempt=" + job.Identity.AttemptID, "executor=" + job.Identity.ExecutorID, "code=limit_hit"} {
		if !strings.Contains(journal.String(), want) {
			t.Fatalf("diagnostic lacks %q: %s", want, &journal)
		}
	}
	if strings.Contains(journal.String(), result.Diagnostics[0].Message) {
		t.Fatalf("private diagnostic journaled: %s", &journal)
	}
}

func TestCommandValidationDoesNotResolveExecutorSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	request := usageJob()
	request.Command.Name = "review"
	request.Repository.Actions = []string{"review"}
	e := Orchestrator{Commands: map[string]config.CommandMapping{
		"review": {Action: "review", Skills: []string{"https://github.com/owner/repo/tree/pinned/skills/not-installed"}},
	}}
	if err := e.validateCommand(request); err != nil {
		t.Fatalf("logical command validation requires executor-host installation: %v", err)
	}
}

func TestPreparedReplyNeedsNoExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2}}`)
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
	client := github.New("token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	e := &Orchestrator{
		GitHub:   client,
		Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{testReviewSkillURL}}},
	}
	request := monitor.Job{SubjectType: "Issue", Number: 7, ID: strings.Repeat("a", 64),
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"review"}},
		Command:    command.Command{Name: "review", Action: command.Review},
	}
	prepared, err := e.prepare(testActionContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.needsExecution() {
		t.Fatal("fixed reply dispatched execution")
	}
	if err := e.publish(context.Background(), prepared, Result{}); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("reply writes = %d", writes)
	}
	if err := prepared.close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishFixWithoutChangesPostsNormalReply(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var payload struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			body = payload.Body
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	e := Orchestrator{GitHub: client}
	request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7,
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Command: command.Command{Name: "fix", Action: command.Fix}}
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

func TestPublishFixForkCheckFailureConsumesPublicationAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary GitHub failure", http.StatusBadGateway)
	}))
	defer server.Close()
	client := github.New("token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	orchestrator := Orchestrator{GitHub: client}
	prepared := &preparedAction{request: monitor.Job{Repository: config.Repository{Name: "owner/repo", ID: 1, OwnerID: 2}}}
	err := orchestrator.publishFix(context.Background(), prepared, Result{Changes: []workspace.Change{{Path: "a", Mode: workspace.Regular}}})
	var apiError *github.StatusError
	if !errors.Is(err, monitor.ErrPostModelPublication) || errors.Is(err, monitor.ErrTerminalJob) || !errors.As(err, &apiError) || apiError.StatusCode != http.StatusBadGateway {
		t.Fatalf("post-model fork check lost classification or cause: %v", err)
	}
}
