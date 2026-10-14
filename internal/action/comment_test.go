package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
)

func TestCommentCommandReportsIssueAndPullRequest(t *testing.T) {
	for _, subject := range []string{"Issue", "PullRequest"} {
		t.Run(subject, func(t *testing.T) {
			base, pullHead := auditGitFixture(t)
			wantHead := base
			if subject == "PullRequest" {
				wantHead = pullHead
			}
			var published string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					_, _ = fmt.Fprint(w, `{"title":"Issue title","body":"Issue description"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
					_, _ = fmt.Fprintf(w, `{"title":"PR title","body":"PR description","base":{"ref":"main"},"head":{"sha":%q}}`, pullHead)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
					_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					var payload struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					published = payload.Body
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"id":1}`)
				default:
					t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			job := monitor.Job{
				Repository:  config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"audit"}},
				SubjectType: subject, Number: 7, ID: strings.Repeat("a", 64),
				Source:  github.Item{Author: github.User{Login: "writer"}},
				Command: command.Command{Name: "audit", Action: command.Comment},
			}
			executor := &Executor{GitHub: client, WorkRoot: t.TempDir(), RunCodex: func(_ context.Context, dir, prompt string, writable bool, outputDir string) (codexcli.Result, error) {
				if writable || outputDir != "" || !strings.Contains(prompt, wantHead) || !strings.Contains(prompt, "Description: ") {
					t.Errorf("wrong comment task: writable=%v outputDir=%q prompt=%q", writable, outputDir, prompt)
				}
				return codexcli.Result{Answer: "Report\n@otherbot fix"}, nil
			}}
			if err := executor.comment(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(published, wantHead) || !strings.Contains(published, "Report") || !strings.Contains(published, job.Marker()) || !strings.Contains(published, "\n> @otherbot fix") {
				t.Fatalf("unsafe or incomplete comment: %q", published)
			}
			if _, err := command.ParseForLogin(published, "otherbot"); !errors.Is(err, command.ErrNoCommand) {
				t.Fatalf("model output became a command: %v", err)
			}
		})
	}
}
