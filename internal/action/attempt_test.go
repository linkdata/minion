package action

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

func TestExecuteScopesAttemptIdentity(t *testing.T) {
	for _, withUsage := range []bool{false, true} {
		t.Run(fmt.Sprint(withUsage), func(t *testing.T) {
			gitFixtureRemote(t)
			writes, calls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
					writeTreeFixture(t, w, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, publicRepositoryJSON)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					_, _ = fmt.Fprint(w, `{"title":"Task","body":"Inspect source"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
					_ = json.NewEncoder(w).Encode([]github.Comment{})
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
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			var journal bytes.Buffer
			orchestrator := Orchestrator{GitHub: client, BotLogin: "bot", WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0), Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}, Runner: Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				calls++
				return aicli.Result{Answer: "No findings.", Tokens: usage.Tokens{Input: 1}, UsageComplete: true}, nil
			}}}
			if withUsage {
				orchestrator.Usage = &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			}
			request := usageJob()
			request.Command.Name = "audit"
			request.Command.Action = command.Comment
			request.Repository.Actions = []string{"audit"}
			for range 2 {
				if err := orchestrator.Execute(context.Background(), request); err != nil {
					t.Fatal(err)
				}
			}
			if writes != 2 || calls != 2 {
				t.Fatalf("accepted executions: writes=%d calls=%d", writes, calls)
			}
			if orchestrator.attemptID != "" || orchestrator.Runner.aroundModel != nil {
				t.Fatal("Execute mutated shared attempt/Runner state")
			}
			identities := regexp.MustCompile(`action call job=[^ ]+ attempt=([0-9a-f]{32}) executor=local`).FindAllStringSubmatch(journal.String(), -1)
			if len(identities) != 2 || identities[0][1] == identities[1][1] {
				t.Fatalf("per-execution attempt identities missing or reused: %v", identities)
			}
			if withUsage {
				records, err := usage.ReadAll(orchestrator.Usage.Dir)
				if err != nil || len(records) != 2 {
					t.Fatalf("records=%d err=%v", len(records), err)
				}
				expected := map[string]bool{identities[0][1]: true, identities[1][1]: true}
				for _, record := range records {
					if !expected[record.ID] || record.CodexCalls != 1 || record.Status != "succeeded" {
						t.Fatalf("usage/Job attempt correlation lost: %+v", record)
					}
					delete(expected, record.ID)
				}
				if len(expected) != 0 {
					t.Fatal("usage record missing execution attempt")
				}
			}
		})
	}
}
