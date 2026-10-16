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

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestPreparedActionRejectsForeignResultBeforePublication(t *testing.T) {
	base, _ := auditGitFixture(t)
	installActionTestSkills(t, testReviewSkillURL)
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
	e := &Orchestrator{
		GitHub:   client,
		BotLogin: "linkdataminion",
		Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{testReviewSkillURL}}},
		Runner: Runner{
			WorkRoot: t.TempDir(),

			Codex: config.CodexConfig{APIKey: "model-secret"},
			RunCodex: func(_ context.Context, dir, prompt string, _ bool, _ string) (codexcli.Result, error) {
				assertPortableSource(t, dir)
				if !strings.Contains(prompt, base) {
					t.Fatal("prompt lost source commit")
				}
				return codexcli.Result{Answer: "No findings."}, nil
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
	installedHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	prepared, err := e.Prepare(ctx, request)
	t.Setenv("HOME", installedHome)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	}()
	data, err := json.Marshal(prepared.Job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), e.WorkRoot) {
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
		if err := e.Publish(ctx, prepared, bad); err == nil {
			t.Fatal("invalid result published")
		}
	}
	if writes != 0 {
		t.Fatal("invalid result issued a GitHub write")
	}
	if err := e.Publish(ctx, prepared, result); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("publication writes = %d", writes)
	}
}

func TestFailedAuditRetainsLabelledPartialArtifactsWithoutPublication(t *testing.T) {
	auditGitFixture(t)
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
	e := &Orchestrator{GitHub: client, Commands: map[string]config.CommandMapping{"security-audit": {Action: "security-audit", Skills: []string{testAuditSkillURL}}}, Runner: Runner{
		WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0),
		RunCodex: func(_ context.Context, dir, _ string, _ bool, output string) (codexcli.Result, error) {
			sourceDir, reportDir = dir, output
			if err := os.WriteFile(filepath.Join(output, "REPORT.md"), []byte(report), 0600); err != nil {
				t.Fatal(err)
			}
			return codexcli.Result{}, failure
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
	e := Orchestrator{Runner: Runner{Logger: log.New(&journal, "", 0)}}
	e.logDiagnostics(result)
	for _, want := range []string{"job=" + job.Identity.JobID, "attempt=" + job.Identity.AttemptID, "executor=" + job.Identity.ExecutorID, "code=limit_hit"} {
		if !strings.Contains(journal.String(), want) {
			t.Fatalf("diagnostic lacks %q: %s", want, &journal)
		}
	}
	if strings.Contains(journal.String(), result.Diagnostics[0].Message) {
		t.Fatalf("private diagnostic journaled: %s", &journal)
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
	prepared, err := e.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.NeedsExecution() {
		t.Fatal("fixed reply dispatched execution")
	}
	if err := e.Publish(context.Background(), prepared, Result{}); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("reply writes = %d", writes)
	}
	if err := prepared.Close(); err != nil {
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
	if err := e.publishFix(context.Background(), &Prepared{request: request}, Result{Answer: "Tests passed."}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "without changing files") || !strings.Contains(body, "Tests passed.") || !strings.Contains(body, request.Marker()) {
		t.Fatalf("no-change reply=%q", body)
	}
}
