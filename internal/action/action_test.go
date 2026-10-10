package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

func TestCloseCheckoutLogsCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	auditGitFixture(t)
	// Go 1.23 predates testing.T.Context.
	checkout, err := gitwork.Clone(context.Background(), t.TempDir(), "o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(checkout.Path(), "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	executor := Executor{Logger: log.New(&output, "", 0)}
	executor.closeCheckout(checkout)
	if !strings.Contains(output.String(), "close checkout: remove checkout:") {
		t.Fatalf("cleanup error not logged: %s", &output)
	}
}

func TestAlreadyAcknowledgedRequiresOwnTrailingMarker(t *testing.T) {
	job := monitor.Job{Repository: config.Repository{Name: "o/r"}, SubjectType: "PullRequest", Number: 7, ID: strings.Repeat("a", 64)}
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
				case "/repos/o/r/pulls/7/reviews":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "node_id": "review-node-1", "body": tt.body, "user": map[string]string{"login": tt.author}, "state": "COMMENTED"}})
				case "/graphql":
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

type actionMonitorGitHub struct {
	items        []github.Item
	notification github.Notification
}

func (f *actionMonitorGitHub) ListNotifications(context.Context, time.Time) ([]github.Notification, time.Duration, error) {
	return []github.Notification{f.notification}, time.Minute, nil
}
func (f *actionMonitorGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	return nil, nil
}
func (f *actionMonitorGitHub) AcceptRepositoryInvitation(context.Context, int64) error { return nil }
func (f *actionMonitorGitHub) ThreadItems(context.Context, string, string, int, time.Time) ([]github.Item, error) {
	return f.items, nil
}
func (f *actionMonitorGitHub) CurrentItem(_ context.Context, item github.Item) (github.Item, error) {
	return item, nil
}
func (f *actionMonitorGitHub) Permission(context.Context, string, string) (github.Permission, error) {
	return github.Permission{Permission: "write"}, nil
}
func (f *actionMonitorGitHub) ReactEyes(context.Context, string) error { return nil }

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
			_, _ = fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = fmt.Fprintf(w, `{"title":"PR","base":{"ref":"main"},"head":{"sha":%q}}`, head)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":"README.md","type":"blob","size":8}]}`, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
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
				return codexcli.Result{Answer: "Review repeats " + auditMarker}, nil
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
	defer store.Close()
	m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60,
		Repositories: []config.Repository{{Name: "o/r", Actions: []string{"review", "audit"}, AllowedActors: []string{"writer"}}}},
		GitHub: monitorGitHub, Executor: executor, Now: func() time.Time { return now.Add(time.Minute) }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if reviewRuns != 1 || auditRuns != 1 || len(reviews) != 1 || len(comments) != 1 || len(store.Data.Processed) != 2 ||
		!strings.Contains(reviews[0].Body, auditMarker) || !strings.Contains(comments[0].Body, "Verified audit report") ||
		!strings.HasSuffix(strings.TrimSpace(comments[0].Body), auditMarker) {
		t.Fatalf("review echo suppressed audit: reviews=%d audits=%d review posts=%d comments=%d processed=%d", reviewRuns, auditRuns, len(reviews), len(comments), len(store.Data.Processed))
	}
}

func TestFixRecoversOnlyRecordedBranch(t *testing.T) {
	for _, tt := range []struct {
		name         string
		remoteSHA    string
		responseSHA  string
		existingUser string
		existingSHA  string
		wantDraft    bool
		wantError    string
	}{
		{name: "recorded commit", remoteSHA: strings.Repeat("a", 40), wantDraft: true},
		{name: "changed branch", remoteSHA: strings.Repeat("b", 40), wantError: "without a matching recorded bot commit"},
		{name: "changed PR response", remoteSHA: strings.Repeat("a", 40), responseSHA: strings.Repeat("b", 40), wantDraft: true, wantError: "does not match recorded bot commit"},
		{name: "forged PR creator", existingUser: "other-writer", existingSHA: strings.Repeat("a", 40), wantError: "without matching bot provenance"},
		{name: "forged PR head", existingUser: "linkdataminion", existingSHA: strings.Repeat("b", 40), wantError: "without matching bot provenance"},
		{name: "bot PR recovery", existingUser: "linkdataminion", existingSHA: strings.Repeat("a", 40)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := monitor.Job{
				Repository:  config.Repository{Name: "o/r", Actions: []string{"fix"}},
				SubjectType: "Issue",
				Number:      7,
				ID:          strings.Repeat("a", 64),
				Source:      github.Item{Author: github.User{Login: "writer"}, HTMLURL: "https://github.com/o/r/issues/7#issuecomment-1"},
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
			var drafts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					if _, err := fmt.Fprint(w, `{"title":"Race fix"}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					if _, err := fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main"}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
					if r.URL.Query().Get("head") != "o:"+branch || r.URL.Query().Get("base") != "main" || r.URL.Query().Get("state") != "open" {
						t.Errorf("unexpected PR head filter: %s", r.URL.RawQuery)
					}
					if tt.existingUser == "" {
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Error(err)
						}
					} else {
						if _, err := fmt.Fprintf(w, `[{"html_url":"https://github.com/o/r/pull/9","body":%q,"state":"open","draft":true,"user":{"login":%q},"head":{"ref":%q,"sha":%q,"repo":{"full_name":"o/r"}},"base":{"ref":"main"}}]`, "body\n\n"+job.Marker(), tt.existingUser, branch, tt.existingSHA); err != nil {
							t.Error(err)
						}
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/branches/"+branch:
					if _, err := fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, tt.remoteSHA); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls":
					var payload struct {
						Draft bool   `json:"draft"`
						Head  string `json:"head"`
						Body  string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode PR: %v", err)
					}
					if !payload.Draft || payload.Head != branch || !strings.Contains(payload.Body, job.Marker()) || !strings.Contains(payload.Body, tt.remoteSHA) {
						t.Errorf("unsafe recovered PR: %+v", payload)
					}
					drafts++
					w.WriteHeader(http.StatusCreated)
					responseSHA := tt.responseSHA
					if responseSHA == "" {
						responseSHA = tt.remoteSHA
					}
					if _, err := fmt.Fprintf(w, `{"html_url":"https://github.com/o/r/pull/9","state":"open","draft":true,"user":{"login":"linkdataminion"},"head":{"ref":%q,"sha":%q,"repo":{"full_name":"o/r"}},"base":{"ref":"main"}}`, branch, responseSHA); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
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
			if (drafts == 1) != tt.wantDraft {
				t.Fatalf("draft PRs = %d, want draft=%t", drafts, tt.wantDraft)
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
				Repository: config.Repository{Name: "o/r", Actions: []string{"release"}}, SubjectType: "Issue", Number: 7,
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
					_, _ = fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main"}`)
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
						body := "notes\n\n" + job.Marker()
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
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.TagName != "v1.0.0" || input.TargetCommitish != target || !input.Draft || !strings.HasSuffix(input.Body, job.Marker()) {
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
