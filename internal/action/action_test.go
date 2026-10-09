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
		name      string
		remoteSHA string
		wantDraft bool
		wantError string
	}{
		{name: "recorded commit", remoteSHA: strings.Repeat("a", 40), wantDraft: true},
		{name: "changed branch", remoteSHA: strings.Repeat("b", 40), wantError: "without a matching recorded bot commit"},
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
					if r.URL.Query().Get("head") != "o:"+branch {
						t.Errorf("unexpected PR head filter: %s", r.URL.RawQuery)
					}
					if _, err := fmt.Fprint(w, `[]`); err != nil {
						t.Error(err)
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
					if _, err := fmt.Fprint(w, `{"html_url":"https://github.com/o/r/pull/9"}`); err != nil {
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
