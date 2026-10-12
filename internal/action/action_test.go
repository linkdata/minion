package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func TestCloseCheckoutRemovesRestrictedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	auditGitFixture(t)
	// Go 1.23 predates testing.T.Context.
	checkout, err := gitwork.Clone(context.Background(), t.TempDir(), "o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	path := checkout.Path()
	locked := filepath.Join(checkout.Path(), "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0700); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	executor := Executor{Logger: log.New(&output, "", 0)}
	executor.closeCheckout(checkout)
	if output.Len() != 0 {
		t.Fatalf("cleanup failed: %s", &output)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout still exists: %v", err)
	}
}

func TestAlreadyAcknowledgedRequiresOwnTrailingMarker(t *testing.T) {
	job := monitor.Job{Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "PullRequest", Number: 7, ID: strings.Repeat("a", 64), Command: command.Command{Action: command.Review}}
	reviewMarker := "<!-- linkdataminion:" + strings.Repeat("b", 64) + " -->"
	for _, tt := range []struct {
		name, body, author, liveBody string
		edited, want                 bool
	}{
		{name: "copied audit marker in review", body: "Review result\n\n" + job.Marker() + "\n\n" + reviewMarker, author: "linkdataminion"},
		{name: "own completion marker", body: "Security audit complete\n\n" + job.Marker(), author: "linkdataminion", want: true},
		{name: "edited bot completion", body: "Security audit complete\n\n" + job.Marker(), author: "linkdataminion", edited: true},
		{name: "stale completion body", body: "Security audit complete\n\n" + job.Marker(), author: "linkdataminion", liveBody: "Edited body without marker"},
		{name: "other author", body: "Security audit complete\n\n" + job.Marker(), author: "writer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var provenanceCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/o/r/pulls/7":
					_, _ = fmt.Fprint(w, `{}`)
				case "/repos/o/r/issues/7/comments", "/repos/o/r/pulls/7/comments":
					_, _ = fmt.Fprint(w, `[]`)
				case "/graphql":
					var input struct {
						Query string `json:"query"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						return
					}
					if strings.Contains(input.Query, "timelineItems") {
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
							"pullRequest": map[string]any{"timelineItems": map[string]any{
								"nodes": []map[string]any{{"id": "review-node-1", "fullDatabaseId": "1", "body": tt.body,
									"author": map[string]string{"login": tt.author}, "state": "COMMENTED", "submittedAt": "2026-10-09T08:00:00Z"}},
								"pageInfo": map[string]any{"hasNextPage": false}}}}}})
						return
					}
					provenanceCalls++
					liveBody := tt.body
					if tt.liveBody != "" {
						liveBody = tt.liveBody
					}
					var editor, editedAt any
					if tt.edited {
						editor = map[string]string{"login": "writer"}
						editedAt = "2026-10-09T08:00:00Z"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"body": liveBody, "author": map[string]string{"login": tt.author}, "editor": editor, "lastEditedAt": editedAt}}})
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			executor := Executor{GitHub: client, BotLogin: "linkdataminion"}
			got, err := executor.alreadyAcknowledged(context.Background(), job)
			if err != nil || got != tt.want {
				t.Fatalf("alreadyAcknowledged() = %t, %v; want %t", got, err, tt.want)
			}
			if (provenanceCalls == 1) != (tt.author == "linkdataminion" && strings.HasSuffix(tt.body, job.Marker())) {
				t.Fatalf("provenance checks = %d for %q", provenanceCalls, tt.name)
			}
		})
	}
}

func TestPrecheckDeadlineStopsJobBeforeCodex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues/7/comments" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client := github.New("test-token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	executor := &Executor{GitHub: client, BotLogin: "linkdataminion", Timeout: 20 * time.Millisecond,
		RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
			t.Fatal("Codex ran after precheck deadline")
			return codexcli.Result{}, nil
		}}
	job := monitor.Job{Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "Issue", Number: 7, ID: strings.Repeat("a", 64)}
	if err := executor.Execute(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("precheck deadline was refundable: %v", err)
	}
}

func TestVerifiedRepositoryRejectsReclaimedName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":9,"full_name":"o/r","owner":{"id":2,"login":"o"}}`)
	}))
	defer server.Close()
	client := github.New("token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	executor := &Executor{GitHub: client}
	job := monitor.Job{Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}}
	if _, err := executor.verifiedRepository(context.Background(), job.Repository); err == nil {
		t.Fatal("accepted a replacement repository under the same name")
	}
	job.Repository.ID = 9
	job.Repository.OwnerID = 3
	if _, err := executor.verifiedRepository(context.Background(), job.Repository); err == nil {
		t.Fatal("accepted a replacement owner under the same name")
	}
	job.Repository.OwnerID = 2
	if _, err := executor.verifiedRepository(context.Background(), job.Repository); err != nil {
		t.Fatalf("rejected pinned repository: %v", err)
	}
}

type actionMonitorGitHub struct {
	items        []github.Item
	notification github.Notification
}

func (f *actionMonitorGitHub) ListRepositoryNotifications(context.Context, string, time.Time) ([]github.Notification, time.Duration, error) {
	notification := f.notification
	notification.Repository.ID = 1
	notification.Repository.Owner.ID = 2
	return []github.Notification{notification}, time.Minute, nil
}
func (f *actionMonitorGitHub) Repository(_ context.Context, name string) (github.Repository, error) {
	return github.Repository{ID: 1, FullName: name, Owner: github.User{ID: 2}}, nil
}
func (f *actionMonitorGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	return nil, nil
}
func (f *actionMonitorGitHub) AcceptRepositoryInvitation(context.Context, int64) error  { return nil }
func (f *actionMonitorGitHub) DeclineRepositoryInvitation(context.Context, int64) error { return nil }
func (f *actionMonitorGitHub) ThreadItems(context.Context, string, string, int, time.Time) ([]github.Item, error) {
	items := append([]github.Item(nil), f.items...)
	for i := range items {
		items[i].Author.ID = 7
	}
	return items, nil
}
func (f *actionMonitorGitHub) CurrentItem(_ context.Context, item github.Item) (github.Item, error) {
	return item, nil
}
func (f *actionMonitorGitHub) Permission(_ context.Context, _, login string) (github.Permission, error) {
	return github.Permission{Permission: "write", User: github.User{ID: 7, Login: login}}, nil
}
func (f *actionMonitorGitHub) ReactEyes(context.Context, string) error { return nil }
func (f *actionMonitorGitHub) CreateIssueComment(context.Context, string, int, string) (github.Comment, error) {
	return github.Comment{}, nil
}

func TestReviewEchoDoesNotSuppressAuditInMonitor(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("audit validators require node")
	}
	_, head := auditGitFixture(t)
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	reviewItem := github.Item{Key: "review-request", NodeID: "review-request-node", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, UpdatedAt: now}
	auditItem := github.Item{Key: "audit-request", NodeID: "audit-request-node", Body: "@linkdataminion audit", Author: github.User{Login: "writer"}, UpdatedAt: now.Add(time.Second)}
	auditCommand, err := command.ParseForLogin(auditItem.Body, "linkdataminion")
	if err != nil {
		t.Fatal(err)
	}
	auditID := sha256.Sum256([]byte(auditItem.Key + "\x00" + auditCommand.Key))
	auditMarker := fmt.Sprintf("<!-- linkdataminion:%x -->", auditID)
	monitorGitHub := &actionMonitorGitHub{
		items: []github.Item{reviewItem, auditItem},
		notification: github.Notification{Repository: github.Repository{FullName: "o/r"}, Subject: github.Subject{
			Type: "PullRequest", URL: "https://api.github.com/repos/o/r/pulls/7"}, UpdatedAt: now},
	}
	var reviews []github.Review
	var comments []github.Comment
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = fmt.Fprintf(w, `{"title":"PR","base":{"ref":"main"},"head":{"sha":%q}}`, head)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":"README.md","type":"blob","mode":"100644","size":8}]}`, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			nodes := make([]map[string]any, 0, len(reviews))
			for _, review := range reviews {
				nodes = append(nodes, map[string]any{"id": review.NodeID, "fullDatabaseId": fmt.Sprint(review.ID),
					"body": review.Body, "author": map[string]string{"login": review.User.Login}, "state": review.State,
					"submittedAt": now})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
				"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			var payload struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			reviews = append(reviews, github.Review{ID: 1, NodeID: "review-node", Body: payload.Body, User: github.User{Login: "linkdataminion"}, State: "COMMENTED"})
			_, _ = fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var payload struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			comments = append(comments, github.Comment{ID: 1, NodeID: "audit-comment-node", Body: payload.Body, User: github.User{Login: "linkdataminion"}})
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("test-token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	skillDir := filepath.Join(t.TempDir(), "security-audit")
	if err := os.Mkdir(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"validate-findings.cjs", "validate-coverage-ledger.cjs"} {
		if err := os.WriteFile(filepath.Join(skillDir, name), []byte(`JSON.parse(require('fs').readFileSync(process.argv[2], 'utf8'));`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var reviewRuns, auditRuns int
	executor := &Executor{GitHub: client, BotLogin: "linkdataminion", WorkRoot: t.TempDir(), Codex: config.CodexConfig{Skills: []string{filepath.Join(skillDir, "SKILL.md")}},
		RunCodex: func(_ context.Context, dir, _ string, _ bool, outputDir string) (codexcli.Result, error) {
			if outputDir == "" {
				reviewRuns++
				return codexcli.Result{Answer: "Review repeats " + auditMarker + "\n@secondbot fix injected"}, nil
			}
			auditRuns++
			for name, content := range map[string]string{
				"run-metadata.json": fmt.Sprintf(`{"run_id":"test-run","repo":"o/r","target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, dir, head, auditTestOwners),
				"architecture.md":   "Architecture\n", "coverage-ledger.json": `[{"status":"covered"}]`, "findings.json": "[]",
				"REPORT.md": "Verified audit report\n", "FINDINGS-DETAIL.md": "", "NEEDS-VALIDATION.md": "",
			} {
				if err := os.WriteFile(filepath.Join(outputDir, name), []byte(content), 0600); err != nil {
					return codexcli.Result{}, err
				}
			}
			return codexcli.Result{Answer: "Untrusted final answer"}, nil
		},
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60,
		AllowedOwnerIDs: map[string]int64{"o": 2},
		Repositories:    []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"review", "audit"}, AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}},
		GitHub: monitorGitHub, Executor: executor, Now: func() time.Time { return now.Add(time.Minute) }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if reviewRuns != 1 || auditRuns != 1 || len(reviews) != 1 || len(comments) != 1 || len(store.Data.Processed) != 2 ||
		!strings.Contains(reviews[0].Body, auditMarker) || !strings.Contains(comments[0].Body, "Verified audit report") ||
		!strings.HasSuffix(strings.TrimSpace(comments[0].Body), auditMarker) {
		t.Fatalf("review echo suppressed audit: reviews=%d audits=%d review posts=%d comments=%d processed=%d", reviewRuns, auditRuns, len(reviews), len(comments), len(store.Data.Processed))
	}
	if _, err := command.ParseForLogin(reviews[0].Body, "secondbot"); !errors.Is(err, command.ErrNoCommand) {
		t.Fatalf("review relayed a live second-bot command: %v", err)
	}
}

func TestFixRefusesIssueEditedAfterRequest(t *testing.T) {
	requestAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	job := monitor.Job{
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "Issue", Number: 7,
		ID: strings.Repeat("a", 64), Source: github.Item{
			Body: "@linkdataminion fix", Author: github.User{Login: "writer"}, UpdatedAt: requestAt,
		},
		Command: command.Command{Action: command.Fix},
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), requestAt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var comments int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main","private":true,"visibility":"private"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r":
			_, _ = fmt.Fprint(w, `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r/actions/permissions":
			_, _ = fmt.Fprint(w, `{"enabled":false}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/permissions/fork-pr-workflows-private-repos":
			_, _ = fmt.Fprint(w, `{"send_write_tokens_to_workflows":false,"send_secrets_and_variables":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rulesets"):
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r/branches/minion/fix-7-aaaaaaaaaaaa":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node","title":"Original title","body":"Visible <!--hidden request-->"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Edited title","bodyText":"Edited body","lastEditedAt":"2026-10-09T08:00:01Z"}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			comments++
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("test-token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	executor := &Executor{GitHub: client, BotLogin: "linkdataminion", Store: store, RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
		t.Fatal("Codex ran on an issue edited after the request")
		return codexcli.Result{}, nil
	}}
	if err := executor.fix(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if comments != 1 || len(store.Data.PendingFixes) != 0 {
		t.Fatalf("edited issue created fix state or missed refusal: comments=%d pending=%v", comments, store.Data.PendingFixes)
	}
}

func TestFixRecoversOnlyRecordedBranch(t *testing.T) {
	for _, tt := range []struct {
		name         string
		remoteSHA    string
		existingUser string
		existingID   int64
		existingSHA  string
		wantComment  bool
		wantError    string
	}{
		{name: "legacy base branch", wantError: "legacy base branch"},
		{name: "recorded commit", remoteSHA: strings.Repeat("a", 40), wantComment: true},
		{name: "changed branch", remoteSHA: strings.Repeat("b", 40), wantError: "without a matching recorded bot commit"},
		{name: "other PR creator", remoteSHA: strings.Repeat("a", 40), existingUser: "other-writer", existingID: 8, existingSHA: strings.Repeat("a", 40), wantError: "without matching requester authorship"},
		{name: "changed PR head", remoteSHA: strings.Repeat("a", 40), existingUser: "writer", existingID: 7, existingSHA: strings.Repeat("b", 40), wantError: "without matching requester authorship"},
		{name: "bot PR", remoteSHA: strings.Repeat("a", 40), existingUser: "linkdataminion", existingID: 9, existingSHA: strings.Repeat("a", 40), wantError: "without matching requester authorship"},
		{name: "requester PR recovery", remoteSHA: strings.Repeat("a", 40), existingUser: "writer", existingID: 7, existingSHA: strings.Repeat("a", 40), wantComment: true},
		{name: "renamed requester PR", remoteSHA: strings.Repeat("a", 40), existingUser: "new-login", existingID: 7, existingSHA: strings.Repeat("a", 40), wantComment: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := monitor.Job{
				Repository:  config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"fix"}},
				SubjectType: "Issue",
				Number:      7,
				ID:          strings.Repeat("a", 64),
				Source:      github.Item{Author: github.User{ID: 7, Login: "writer"}, HTMLURL: "https://github.com/o/r/issues/7#issuecomment-1"},
				Command:     command.Command{Action: command.Fix},
			}
			statePath := filepath.Join(t.TempDir(), "state.json")
			store, err := state.Open(statePath, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			store.Data.PendingFixes[job.ID] = strings.Repeat("a", 40)
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = state.Open(statePath, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()

			branch := "minion/fix-7-aaaaaaaaaaaa"
			var commentBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					if _, err := fmt.Fprint(w, `{"title":"Race fix"}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					if _, err := fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main","private":true,"visibility":"private"}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r":
					_, _ = fmt.Fprint(w, `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r/actions/permissions":
					_, _ = fmt.Fprint(w, `{"enabled":false}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/permissions/fork-pr-workflows-private-repos":
					_, _ = fmt.Fprint(w, `{"send_write_tokens_to_workflows":false,"send_secrets_and_variables":false}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rulesets"):
					_, _ = fmt.Fprint(w, `[]`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
					if r.URL.Query().Get("head") != "linkdataminion:"+branch || r.URL.Query().Get("base") != "main" || r.URL.Query().Get("state") != "open" {
						t.Errorf("unexpected PR head filter: %s", r.URL.RawQuery)
					}
					if tt.existingUser == "" {
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Error(err)
						}
					} else {
						if _, err := fmt.Fprintf(w, `[{"html_url":"https://github.com/o/r/pull/9","body":"requester wrote this","state":"open","draft":false,"user":{"login":%q,"id":%d},"head":{"ref":%q,"sha":%q,"repo":{"full_name":"linkdataminion/r"}},"base":{"ref":"main"}}]`, tt.existingUser, tt.existingID, branch, tt.existingSHA); err != nil {
							t.Error(err)
						}
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r/branches/"+branch:
					if tt.remoteSHA == "" {
						http.NotFound(w, r)
					} else if _, err := fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, tt.remoteSHA); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/"+branch:
					_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, strings.Repeat("a", 40))
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					var payload struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					commentBody = payload.Body
					w.WriteHeader(http.StatusCreated)
					if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("token")
			client.BaseURL = server.URL
			client.HTTPClient = server.Client()
			executor := &Executor{
				GitHub: client, BotLogin: "linkdataminion", Store: store,
				RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
					t.Fatal("Codex ran during branch recovery")
					return codexcli.Result{}, nil
				},
			}
			err = executor.fix(context.Background(), job)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("fix() error = %v, want %q", err, tt.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if (commentBody != "") != tt.wantComment {
				t.Fatalf("comment = %q, want comment=%t", commentBody, tt.wantComment)
			}
			if tt.wantComment {
				if !strings.Contains(commentBody, branch) || !strings.Contains(commentBody, strings.Repeat("a", 40)) || !strings.HasSuffix(commentBody, job.Marker()) {
					t.Fatalf("acknowledgement lacks verified branch, head, or marker: %q", commentBody)
				}
				if tt.existingUser == "" && !strings.Contains(commentBody, "https://github.com/o/r/compare/main...linkdataminion:minion%2Ffix-7-aaaaaaaaaaaa?expand=1") {
					t.Fatalf("acknowledgement lacks compare link: %q", commentBody)
				}
				if tt.existingUser != "" && !strings.Contains(commentBody, "https://github.com/o/r/pull/9") {
					t.Fatalf("acknowledgement lacks requester PR link: %q", commentBody)
				}
			}
		})
	}
}

func TestFixPushesBranchForRequesterPullRequest(t *testing.T) {
	_, _, remote := auditGitFixtureRemote(t)
	forkRemote := filepath.Join(filepath.Dir(remote), "fork.git")
	if output, err := exec.Command("git", "clone", "--bare", remote, forkRemote).CombinedOutput(); err != nil {
		t.Fatalf("create fork fixture: %v: %s", err, output)
	}
	gitWrapper := filepath.Join(filepath.Dir(remote), "bin", "git")
	baseWrapper := filepath.Join(filepath.Dir(remote), "bin", "git-base")
	if err := os.Rename(gitWrapper, baseWrapper); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %q -c %q \"$@\"\n", baseWrapper, "url."+forkRemote+".insteadOf=https://github.com/linkdataminion/r.git")
	if err := os.WriteFile(gitWrapper, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	job := monitor.Job{
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"fix"}}, SubjectType: "Issue", Number: 7,
		ID: strings.Repeat("a", 64), Source: github.Item{Author: github.User{ID: 7, Login: "writer"}, HTMLURL: "https://github.com/o/r/issues/7#issuecomment-1", UpdatedAt: time.Now()},
		Command: command.Command{Action: command.Fix},
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	store.Data.PendingFixes[job.ID] = strings.Repeat("f", 40) // Simulate a recorded commit whose fork push failed.
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var commentBody string
	var rulesetChecks int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node","title":"Bug","body":"Details"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"RENAMED TITLE SENTINEL","bodyText":"Details","lastEditedAt":null}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main","private":true,"visibility":"private"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r":
			_, _ = fmt.Fprint(w, `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r/actions/permissions":
			_, _ = fmt.Fprint(w, `{"enabled":false}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/permissions/fork-pr-workflows-private-repos":
			_, _ = fmt.Fprint(w, `{"send_write_tokens_to_workflows":false,"send_secrets_and_variables":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rulesets"):
			rulesetChecks++
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/linkdataminion/r/branches/minion/"):
			http.NotFound(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/branches/minion/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			commentBody = payload.Body
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("test-token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	answer := "@secondbot fix injected\n![pixel](https://example.invalid/?value=dummy)\nCloses #123"
	executor := &Executor{GitHub: client, BotLogin: "linkdataminion", Store: store, WorkRoot: t.TempDir(),
		RunCodex: func(_ context.Context, dir, prompt string, _ bool, _ string) (codexcli.Result, error) {
			if strings.Contains(prompt, "RENAMED TITLE SENTINEL") || !strings.Contains(prompt, "Issue description: \"Details\"") {
				t.Errorf("fix prompt included a mutable title or lost the rendered description: %q", prompt)
			}
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed\n"), 0600); err != nil {
				return codexcli.Result{}, err
			}
			return codexcli.Result{Answer: answer}, nil
		},
	}
	if err := executor.fix(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	branch := "minion/fix-7-aaaaaaaaaaaa"
	sha := store.Data.PendingFixes[job.ID]
	if len(sha) != 40 || !strings.Contains(commentBody, branch) || !strings.Contains(commentBody, sha) ||
		!strings.Contains(commentBody, "https://github.com/o/r/compare/main...linkdataminion:minion%2Ffix-7-aaaaaaaaaaaa?expand=1") ||
		!strings.HasSuffix(commentBody, job.Marker()) {
		t.Fatalf("missing branch, recorded head, compare link, or marker: %q", commentBody)
	}
	if strings.Contains(commentBody, "@secondbot") || strings.Contains(commentBody, "![pixel]") || strings.Contains(commentBody, "Closes #123") {
		t.Fatalf("branch reply published model output: %q", commentBody)
	}
	if rulesetChecks != 4 {
		t.Fatalf("push rulesets checked %d times, want base and fork before Codex and before push", rulesetChecks)
	}
	if output, err := exec.Command("git", "--git-dir", remote, "rev-parse", "refs/heads/"+branch).CombinedOutput(); err == nil {
		t.Fatalf("base repository received a pre-review branch: %s", output)
	}
	remoteSHA, err := exec.Command("git", "--git-dir", forkRemote, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil || strings.TrimSpace(string(remoteSHA)) != sha {
		t.Fatalf("remote branch head = %q, want %s: %v", remoteSHA, sha, err)
	}
}

func TestReviewMarksOnlyPostCodexPublicationFailure(t *testing.T) {
	_, head := auditGitFixture(t)
	job := monitor.Job{Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "PullRequest", Number: 7,
		ID: strings.Repeat("a", 64), Source: github.Item{Author: github.User{Login: "writer"}}}
	repoFail := true
	var codexRuns, postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			if repoFail {
				http.Error(w, "unavailable", http.StatusBadGateway)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = fmt.Fprintf(w, `{"base":{"ref":"main"},"head":{"sha":%q}}`, head)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/")
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, sha)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			postCalls++
			http.Error(w, "unavailable", http.StatusBadGateway)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("test-token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	executor := &Executor{GitHub: client, BotLogin: "linkdataminion", WorkRoot: t.TempDir(),
		RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
			codexRuns++
			return codexcli.Result{Answer: "No findings."}, nil
		}}
	err := executor.review(context.Background(), job)
	if err == nil || errors.Is(err, monitor.ErrPostCodexPublication) || codexRuns != 0 {
		t.Fatalf("pre-Codex outage: err=%v runs=%d", err, codexRuns)
	}
	repoFail = false
	err = executor.review(context.Background(), job)
	if !errors.Is(err, monitor.ErrPostCodexPublication) || codexRuns != 1 || postCalls != 1 {
		t.Fatalf("post-Codex outage: err=%v runs=%d posts=%d", err, codexRuns, postCalls)
	}
}

func TestFixForkRefusesUnsafePublication(t *testing.T) {
	for _, tt := range []struct {
		name, fork, actions, policy, want string
		baseID                            int64
	}{
		{name: "reclaimed base name", baseID: 9, want: "repository ID or owner ID"},
		{name: "missing fork", want: "bot-owned fork"},
		{name: "unrelated repository", fork: `{"id":2,"full_name":"linkdataminion/r","private":true,"visibility":"private"}`, want: "not an active fork"},
		{name: "different parent ID", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":3,"full_name":"o/r"},"private":true,"visibility":"private"}`, want: "not an active fork"},
		{name: "public fork of private base", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"visibility":"public"}`, want: "matching visibility"},
		{name: "fork Actions enabled", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`, actions: `{"enabled":true}`, want: "must be disabled"},
		{name: "fork Actions status absent", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`, actions: `{}`, want: "omitted enabled"},
		{name: "private PR gets secrets", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`, actions: `{"enabled":false}`, policy: `{"send_write_tokens_to_workflows":false,"send_secrets_and_variables":true}`, want: "secrets or write tokens"},
		{name: "private PR gets write token", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`, actions: `{"enabled":false}`, policy: `{"send_write_tokens_to_workflows":true,"send_secrets_and_variables":false}`, want: "secrets or write tokens"},
		{name: "private PR policy incomplete", fork: `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":true,"visibility":"private"}`, actions: `{"enabled":false}`, policy: `{}`, want: "incomplete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/o/r":
					baseID := tt.baseID
					if baseID == 0 {
						baseID = 1
					}
					_, _ = fmt.Fprintf(w, `{"id":%d,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main","private":true,"visibility":"private"}`, baseID)
				case "/repos/linkdataminion/r":
					if tt.fork == "" {
						http.NotFound(w, r)
					} else {
						_, _ = fmt.Fprint(w, tt.fork)
					}
				case "/repos/linkdataminion/r/actions/permissions":
					_, _ = fmt.Fprint(w, tt.actions)
				case "/repos/o/r/actions/permissions/fork-pr-workflows-private-repos":
					_, _ = fmt.Fprint(w, tt.policy)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			executor := &Executor{GitHub: client, BotLogin: "linkdataminion"}
			if _, _, err := executor.fixFork(context.Background(), config.Repository{Name: "o/r", ID: 1, OwnerID: 2}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("fixFork() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestFixForkChecksNetworkRootPushRules(t *testing.T) {
	for _, tt := range []struct {
		name, forkSource, rootRules, want string
		rootStatus                        int
	}{
		{name: "no inherited restrictions", forkSource: `{"id":9,"full_name":"upstream/r"}`},
		{name: "root bypass rule", forkSource: `{"id":9,"full_name":"upstream/r"}`, rootRules: `[{"id":42}]`, want: "can be bypassed"},
		{name: "root ruleset API blocked", forkSource: `{"id":9,"full_name":"upstream/r"}`, rootStatus: http.StatusForbidden, want: "403"},
		{name: "fork source mismatch", forkSource: `{"id":8,"full_name":"upstream/r"}`, want: "not an active fork"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var rootQueries int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/o/r":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"fork":true,"source":{"id":9,"full_name":"upstream/r"},"default_branch":"main","private":true,"visibility":"private"}`)
				case "/repos/upstream/r":
					_, _ = fmt.Fprint(w, `{"id":9,"full_name":"upstream/r","private":true,"visibility":"private"}`)
				case "/repos/linkdataminion/r":
					_, _ = fmt.Fprintf(w, `{"id":2,"full_name":"linkdataminion/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":%s,"private":true,"visibility":"private"}`, tt.forkSource)
				case "/repos/linkdataminion/r/actions/permissions":
					_, _ = fmt.Fprint(w, `{"enabled":false}`)
				case "/repos/o/r/actions/permissions/fork-pr-workflows-private-repos":
					_, _ = fmt.Fprint(w, `{"send_write_tokens_to_workflows":false,"send_secrets_and_variables":false}`)
				case "/repos/upstream/r/rulesets":
					rootQueries++
					if tt.rootStatus != 0 {
						w.WriteHeader(tt.rootStatus)
						_, _ = fmt.Fprint(w, `{"message":"unavailable"}`)
					} else if tt.rootRules == "" {
						_, _ = fmt.Fprint(w, `[]`)
					} else {
						_, _ = fmt.Fprint(w, tt.rootRules)
					}
				case "/repos/upstream/r/rulesets/42":
					_, _ = fmt.Fprint(w, `{"id":42,"target":"push","enforcement":"active","bypass_actors":[{"actor_type":"RepositoryRole","actor_id":5}]}`)
				case "/repos/o/r/rulesets", "/repos/linkdataminion/r/rulesets":
					_, _ = fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			executor := &Executor{GitHub: client, BotLogin: "linkdataminion"}
			_, _, err := executor.fixFork(context.Background(), config.Repository{Name: "o/r", ID: 1, OwnerID: 2})
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("fixFork() = %v, want %q", err, tt.want)
			}
			if tt.name != "fork source mismatch" && rootQueries != 1 {
				t.Fatalf("root push rulesets checked %d times, want 1", rootQueries)
			}
		})
	}
}

func TestValidTagRejectsUnsafeGitRefs(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "release/v2"} {
		if !validTag(tag) {
			t.Errorf("rejected valid tag %q", tag)
		}
	}
	for _, tag := range []string{"", "-bad", "release/.hidden", "release/bad.lock", "v1..2", "release//v2", "v1."} {
		if validTag(tag) {
			t.Errorf("accepted unsafe tag %q", tag)
		}
	}
}

func TestReleaseReservesTagAndRecoversOnlyBotDraft(t *testing.T) {
	for _, tt := range []struct {
		name           string
		competingTag   bool
		firstPostFail  bool
		saveFailure    bool
		existingAuthor string
		tamperedBody   bool
		unprotected    bool
		creationRule   bool
		lostAfterCodex bool
		lostAfterTag   bool
		wantError      string
		wantDrafts     int
		wantComments   int
		wantCodex      int
	}{
		{name: "reserve and draft", wantDrafts: 1, wantComments: 1, wantCodex: 1},
		{name: "competing tag at another commit", competingTag: true, wantError: "reserve tag", wantCodex: 1},
		{name: "recover after failed draft", firstPostFail: true, wantDrafts: 1, wantComments: 1, wantCodex: 1},
		{name: "retry after failed checkpoint", saveFailure: true, wantDrafts: 1, wantComments: 1, wantCodex: 2},
		{name: "forged draft", existingAuthor: "other-writer", wantError: "without matching bot provenance"},
		{name: "bot draft recovery", existingAuthor: "linkdataminion", wantComments: 1},
		{name: "tampered bot draft", existingAuthor: "linkdataminion", tamperedBody: true, wantError: "without matching bot provenance"},
		{name: "unprotected tags", unprotected: true, wantComments: 1},
		{name: "tag creation restricted", creationRule: true, wantComments: 1},
		{name: "protection lost after notes", lostAfterCodex: true, wantError: "tag protection changed", wantCodex: 1},
		{name: "protection lost after tag", lostAfterTag: true, wantError: "tag protection changed", wantCodex: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.saveFailure && os.Geteuid() == 0 {
				t.Skip("root bypasses directory permissions")
			}
			target, alternative, remote := auditGitFixtureRemote(t)
			git := func(args ...string) (string, error) {
				command := exec.Command("git", append([]string{"--git-dir", remote}, args...)...)
				out, err := command.CombinedOutput()
				return strings.TrimSpace(string(out)), err
			}
			job := monitor.Job{
				Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"release"}}, SubjectType: "Issue", Number: 7,
				ID: strings.Repeat("a", 64), Source: github.Item{Author: github.User{Login: "writer"}},
				Command: command.Command{Action: command.Release, Args: "v1.0.0"},
			}
			stateDir := t.TempDir()
			store, err := state.Open(filepath.Join(stateDir, "state.json"), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			t.Cleanup(func() { _ = os.Chmod(stateDir, 0700) })
			if tt.existingAuthor != "" {
				store.Data.PendingReleases[job.ID] = state.PendingRelease{Tag: "v1.0.0", Target: target, Notes: "notes"}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
			}
			if tt.existingAuthor != "" {
				if _, err := git("update-ref", "refs/tags/v1.0.0", target); err != nil {
					t.Fatal(err)
				}
			}
			var codexCalls, draftPosts, draftSuccess, comments, protectionCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/rulesets":
					protectionCalls++
					if tt.unprotected || tt.lostAfterCodex && protectionCalls >= 2 || tt.lostAfterTag && protectionCalls >= 3 {
						_, _ = fmt.Fprint(w, `[]`)
					} else {
						if tt.creationRule {
							_, _ = fmt.Fprint(w, `[{"id":42},{"id":43}]`)
						} else {
							_, _ = fmt.Fprint(w, `[{"id":42}]`)
						}
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/rulesets/42":
					_, _ = fmt.Fprint(w, `{"id":42,"target":"tag","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["~ALL"],"exclude":[]}},"rules":[{"type":"update"},{"type":"deletion"}]}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/rulesets/43":
					_, _ = fmt.Fprint(w, `{"id":43,"target":"tag","enforcement":"active","bypass_actors":[{"actor_type":"RepositoryRole","actor_id":5}],"conditions":{"ref_name":{"include":["refs/tags/v*"]}},"rules":[{"type":"creation"}]}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/releases":
					if tt.existingAuthor != "" {
						body := inertModelOutput("notes") + "\n\n" + job.Marker()
						if tt.tamperedBody {
							body = "changed notes\n\n" + job.Marker()
						}
						_, _ = fmt.Fprintf(w, `[{"tag_name":"v1.0.0","target_commitish":%q,"body":%q,"draft":true,"author":{"login":%q}}]`, target, body, tt.existingAuthor)
					} else {
						_, _ = fmt.Fprint(w, `[]`)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/refs":
					var input struct{ Ref, SHA string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Ref != "refs/tags/v1.0.0" || input.SHA != target {
						t.Errorf("tag reservation: %+v, %v", input, err)
					}
					if _, err := git("rev-parse", "--verify", "refs/tags/v1.0.0"); err == nil {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = fmt.Fprint(w, `{"message":"Reference already exists"}`)
						return
					}
					if _, err := git("update-ref", "refs/tags/v1.0.0", target); err != nil {
						t.Errorf("create remote tag: %v", err)
					}
					w.WriteHeader(http.StatusCreated)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/git/ref/tags/v1.0.0":
					tagSHA, err := git("rev-parse", "--verify", "refs/tags/v1.0.0")
					if err != nil {
						http.NotFound(w, r)
						return
					}
					_, _ = fmt.Fprintf(w, `{"ref":"refs/tags/v1.0.0","object":{"type":"commit","sha":%q}}`, tagSHA)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/releases":
					draftPosts++
					var input struct {
						TagName         string `json:"tag_name"`
						TargetCommitish string `json:"target_commitish"`
						Body            string `json:"body"`
						Draft           bool   `json:"draft"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.TagName != "v1.0.0" || input.TargetCommitish != target || !input.Draft || !strings.HasSuffix(input.Body, job.Marker()) || !strings.Contains(input.Body, inertModelOutput("notes")) {
						t.Errorf("draft input: %+v, %v", input, err)
					}
					if tt.firstPostFail && draftPosts == 1 {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					draftSuccess++
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprintf(w, `{"tag_name":"v1.0.0","target_commitish":%q,"draft":true,"html_url":"https://github.com/o/r/releases/tag/v1.0.0","author":{"login":"linkdataminion"}}`, target)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					comments++
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"id":1}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			executor := &Executor{GitHub: client, BotLogin: "linkdataminion", Store: store, WorkRoot: t.TempDir(), RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
				codexCalls++
				if tt.saveFailure && codexCalls == 1 {
					if err := os.Chmod(stateDir, 0500); err != nil {
						t.Fatal(err)
					}
				}
				if tt.competingTag {
					if _, err := git("update-ref", "refs/tags/v1.0.0", alternative); err != nil {
						t.Fatal(err)
					}
				}
				return codexcli.Result{Answer: "notes"}, nil
			}}
			err = executor.release(context.Background(), job)
			if tt.saveFailure {
				if err == nil {
					t.Fatal("release succeeded without durable checkpoint")
				}
				if _, ok := store.Data.PendingReleases[job.ID]; ok {
					t.Fatal("failed checkpoint remained in memory")
				}
				if _, tagErr := git("rev-parse", "--verify", "refs/tags/v1.0.0"); tagErr == nil {
					t.Fatal("tag was reserved after checkpoint failure")
				}
				if err := os.Chmod(stateDir, 0700); err != nil {
					t.Fatal(err)
				}
				err = executor.release(context.Background(), job)
			}
			if tt.firstPostFail {
				if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
					t.Fatalf("first release error = %v", err)
				}
				err = executor.release(context.Background(), job)
			}
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("release error = %v, want %q", err, tt.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if draftSuccess != tt.wantDrafts || comments != tt.wantComments || codexCalls != tt.wantCodex {
				t.Fatalf("drafts=%d comments=%d codex=%d", draftSuccess, comments, codexCalls)
			}
		})
	}
}
