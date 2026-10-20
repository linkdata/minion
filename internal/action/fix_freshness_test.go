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
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
)

func TestFixRefusesIssueEditedDuringModel(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		queryFailure bool
	}{
		{name: "no changes"},
		{name: "changed files", change: "fixed\n"},
		{name: "final query fails", change: "fixed\n", queryFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitFixtureRemote(t)
			requestAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			request := monitor.Job{
				Repository:  config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"fix"}},
				SubjectType: "Issue", Number: 7, ID: strings.Repeat("a", 64),
				Source:  github.Item{Author: github.User{ID: 7, Login: "writer"}, Body: "@linkdataminion fix", UpdatedAt: requestAt},
				Command: command.Command{Name: "fix", Action: command.Fix},
			}
			store := openState(t, requestAt)
			var modelRan atomic.Bool
			var queries atomic.Int32
			var reply string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, privateRepositoryJSON)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r":
					_, _ = fmt.Fprint(w, privateForkRepositoryJSON)
				case serveSafeForkChecks(w, r, "linkdataminion/r", true):
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
					_, _ = fmt.Fprint(w, `[]`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r/branches/minion/fix-7-aaaaaaaaaaaa":
					http.NotFound(w, r)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					_, _ = fmt.Fprint(w, `{"node_id":"issue-node","title":"Bug","body":"Original task"}`)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					queries.Add(1)
					if modelRan.Load() && tc.queryFailure {
						http.Error(w, "temporary query failure", http.StatusBadGateway)
						return
					}
					edited := "null"
					if modelRan.Load() {
						edited = `"2026-10-09T08:00:01Z"`
					}
					_, _ = fmt.Fprintf(w, `{"data":{"node":{"title":"Bug","bodyText":"Original task","lastEditedAt":%s}}}`, edited)
				case captureComment(t, w, r, &reply):
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			orchestrator := &Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(server, "token"),
				BotLogin: "linkdataminion", Store: store,
				Commands: map[string]config.CommandMapping{"fix": {Action: "fix", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Runner: Runner{runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
					if tc.change != "" {
						if err := os.WriteFile(filepath.Join(modelRequest.dir, "fixed.txt"), []byte(tc.change), 0600); err != nil {
							return aicli.Result{}, err
						}
					}
					modelRan.Store(true)
					return aicli.Result{Answer: "stale model answer"}, nil
				}},
			}
			err := attemptFixture(orchestrator).perform(testActionContext(t), request)
			if tc.queryFailure {
				if !errors.Is(err, monitor.ErrPostModelPublication) || reply != "" {
					t.Fatalf("query failure published or lost retry classification: reply=%q err=%v", reply, err)
				}
			} else if err != nil || !strings.Contains(reply, "issue changed after this `fix` request") || strings.Contains(reply, "stale model answer") {
				t.Fatalf("stale issue was published: reply=%q err=%v", reply, err)
			}
			if !modelRan.Load() || queries.Load() != 2 || len(store.Data.PendingFixes) != 0 {
				t.Fatalf("freshness checks or checkpoint wrong: model=%t queries=%d pending=%v", modelRan.Load(), queries.Load(), store.Data.PendingFixes)
			}
		})
	}
}
