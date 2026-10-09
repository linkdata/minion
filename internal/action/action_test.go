package action

import (
	"bytes"
	"context"
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
						_, _ = fmt.Fprint(w, `[{"id":42}]`)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/rulesets/42":
					_, _ = fmt.Fprint(w, `{"id":42,"target":"tag","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["~ALL"],"exclude":[]}},"rules":[{"type":"update"},{"type":"deletion"}]}`)
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
