package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
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
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

func TestExecutePublicationBookkeepingBoundaries(t *testing.T) {
	for _, scenario := range []string{"final usage save", "hard deadline"} {
		t.Run(scenario, func(t *testing.T) {
			gitFixtureRemote(t)
			root := t.TempDir()
			usageDir := filepath.Join(root, "usage")
			var posted atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case serveIssueFixture(t, w, r):
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					posted.Store(true)
					if scenario == "hard deadline" {
						// Also bound a broken implementation that renews the publication context.
						select {
						case <-r.Context().Done():
						case <-time.After(20 * time.Second):
							http.Error(w, "publication did not stop", http.StatusGatewayTimeout)
						}
						return
					}
					if err := os.Rename(usageDir, usageDir+"-durable"); err != nil {
						t.Error(err)
						http.Error(w, "fixture", http.StatusInternalServerError)
						return
					}
					if err := os.WriteFile(usageDir, []byte("blocked"), 0600); err != nil {
						t.Error(err)
					}
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"id":1}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			budget := 5 * time.Second
			if scenario == "final usage save" {
				budget = 30 * time.Second
			}
			started := time.Now()
			work := t.TempDir()
			var journal bytes.Buffer
			e := Orchestrator{GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: work,
				Timeout: budget, Logger: log.New(&journal, "", 0), Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Runner: Runner{runModel: func(modelContext context.Context, _ modelRun) (aicli.Result, error) {
					got, ok := modelContext.Deadline()
					if !ok || got.Before(started) || got.Sub(started) > budget+500*time.Millisecond {
						t.Fatal("model lost the configured action deadline")
					}
					return aicli.Result{Answer: "No findings.", Tokens: usage.Tokens{Input: 10, Output: 2}, UsageComplete: true}, nil
				}},
			}
			if scenario == "final usage save" {
				e.Usage = &usage.Store{Dir: usageDir}
			}
			request := usageJob()
			request.Command.Name, request.Command.Action = "audit", command.Comment
			request.Repository.Actions = []string{"audit"}
			err := e.Execute(t.Context(), request)
			if !posted.Load() {
				t.Fatalf("test never reached publication: %v", err)
			}
			if scenario == "hard deadline" {
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, monitor.ErrPostModelPublication) {
					t.Fatalf("publication did not retain the terminal hard deadline: %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(journal.String(), "final action usage checkpoint failed:") {
				t.Fatalf("published work failed or its accounting error was hidden: %v; %s", err, journal.String())
			}
			records, err := usage.ReadAll(usageDir + "-durable")
			if err != nil || len(records) != 1 || records[0].Status != "running" || records[0].Tokens != (usage.Tokens{Input: 10, Output: 2}) || !records[0].UsageComplete {
				t.Fatalf("last durable paid usage was lost: %+v, %v", records, err)
			}
		})
	}
}
