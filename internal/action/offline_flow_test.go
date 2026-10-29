package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

// Exercise the real API client, monitor, orchestrator, and Git checkout together.
// All GitHub responses come from this local server; Git uses a local bare repo.
func TestOfflineReviewPollRecoversAfterPublication(t *testing.T) {
	_, head, _ := gitFixtureRemote(t)

	now := time.Date(2026, 10, 9, 8, 1, 0, 0, time.UTC)
	requestAt := now.Add(-time.Second) // Remains inside the next poll's cursor overlap.
	requestBody := "@linkdataminion review"
	parsed, err := command.ParseForLoginCommands(requestBody, "linkdataminion", defaultActionTestCommands)
	if err != nil {
		t.Fatal(err)
	}
	jobID := sha256.Sum256([]byte("o/r#issue-comment:42\x00" + parsed.Name))
	marker := fmt.Sprintf("<!-- linkdataminion:%x -->", jobID)
	request := github.Comment{
		ID: 42, NodeID: "request-node", Body: requestBody,
		User: github.User{ID: 7, Login: "writer"}, CreatedAt: requestAt, UpdatedAt: requestAt,
		HTMLURL: "https://github.com/o/r/pull/7#issuecomment-42",
	}

	var mu sync.Mutex
	var events []string
	var reviews []github.Review
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	snapshot := func() ([]string, []github.Review) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events), slices.Clone(reviews)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer offline-token" {
			t.Errorf("unexpected authorization header for %s %s", r.Method, r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/notifications":
			since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
			if err != nil {
				t.Errorf("notification since: %v", err)
				http.Error(w, "invalid since", http.StatusBadRequest)
				return
			}
			if since.After(requestAt) {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			_ = json.NewEncoder(w).Encode([]github.Notification{{
				ID: "notification-7", Repository: github.Repository{ID: 1, FullName: "o/r", Owner: github.User{ID: 2, Login: "o"}, Private: true},
				Subject:   github.Subject{Type: "PullRequest", URL: "https://api.github.com/repos/o/r/pulls/7"},
				UpdatedAt: requestAt,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = fmt.Fprintf(w, `{"title":"PR","created_at":"2026-10-08T00:00:00Z","base":{"ref":"main"},"head":{"sha":%q}}`, head)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]github.Comment{request})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var input struct {
				Query     string `json:"query"`
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode GraphQL request: %v", err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			switch {
			case strings.Contains(input.Query, "timelineItems"):
				_, current := snapshot()
				nodes := make([]map[string]any, 0, len(current))
				for _, review := range current {
					nodes = append(nodes, map[string]any{"id": review.NodeID, "fullDatabaseId": strconv.FormatInt(review.ID, 10),
						"body": review.Body, "author": map[string]string{"login": review.User.Login}, "state": review.State,
						"submittedAt": review.SubmittedAt})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
			case strings.Contains(input.Query, "addReaction") && input.Variables.ID == request.NodeID:
				record("reaction")
				_, _ = fmt.Fprint(w, `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":true}}}}}`)
			case strings.Contains(input.Query, "... on Comment") && input.Variables.ID == request.NodeID:
				record("verify request")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"body": requestBody, "author": map[string]any{"login": "writer", "id": 7}}}})
			case strings.Contains(input.Query, "... on Comment") && input.Variables.ID == "review-node":
				_, current := snapshot()
				if len(current) != 1 {
					t.Errorf("review provenance requested with %d reviews", len(current))
					http.Error(w, "missing review", http.StatusNotFound)
					return
				}
				record("verify review")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"body": current[0].Body, "author": map[string]string{"login": "linkdataminion"}}}})
			default:
				t.Errorf("unexpected GraphQL query %q for node %q", input.Query, input.Variables.ID)
				http.Error(w, "unexpected GraphQL request", http.StatusBadRequest)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/collaborators/writer/permission":
			record("permission")
			_, _ = fmt.Fprint(w, `{"permission":"write","user":{"login":"writer","id":7}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main","private":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			var input struct {
				Body     string `json:"body"`
				CommitID string `json:"commit_id"`
				Event    string `json:"event"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode review request: %v", err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if input.CommitID != head || input.Event != "COMMENT" || !strings.HasSuffix(input.Body, "\n\n"+marker) {
				t.Errorf("unexpected review: commit=%q event=%q body=%q", input.CommitID, input.Event, input.Body)
			}
			mu.Lock()
			reviews = append(reviews, github.Review{ID: 99, NodeID: "review-node", Body: input.Body,
				User: github.User{Login: "linkdataminion"}, State: "COMMENTED", SubmittedAt: requestAt})
			events = append(events, "publish")
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{"id":99}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	statePath := t.TempDir() + "/state.json"
	store, err := state.Open(statePath, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	installActionTestSkills(t, testReviewSkillURL)
	commands := map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{testReviewSkillURL}}}
	var acceptedBeforePublication state.AcceptedAction
	var failureBeforePublication state.JobFailure
	var resultBeforePublication []byte
	run := func(at time.Time, store *state.Store) {
		t.Helper()
		client := testGitHub(server, "offline-token")
		orchestrator := &Orchestrator{WorkRoot: t.TempDir(), GitHub: client,
			BotLogin: "linkdataminion", BotID: 8,
			Store:    store,
			Commands: commands,
			Runner: Runner{
				runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
					dir, prompt, writable, outputDir := modelRequest.dir, modelRequest.prompt, modelRequest.writable, modelRequest.outputDir
					if _, err := os.Stat(dir); err != nil || writable || outputDir != "" || !strings.Contains(prompt, head) {
						return aicli.Result{}, fmt.Errorf("unexpected model invocation: dir=%q writable=%t output=%q stat=%v", dir, writable, outputDir, err)
					}
					record("model")
					return aicli.Result{Answer: "No findings."}, nil
				},
			},
		}
		m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60, Commands: commands,
			Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"review"}, AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}},
			GitHub: client, Handler: orchestrator, Now: func() time.Time { return at }}
		bindSelectedRunner(&m, store, orchestrator)
		validatePublication := orchestrator.ValidatePublication
		orchestrator.ValidatePublication = func(ctx context.Context, job monitor.Job, requireAssigned bool) error {
			if err := validatePublication(ctx, job, requireAssigned); err != nil {
				return err
			}
			if !requireAssigned || acceptedBeforePublication.JobID != "" {
				return nil
			}
			return store.WithLock(func() error {
				acceptedBeforePublication = *store.Data.Accepted
				failureBeforePublication = store.Data.JobFailures[job.ID]
				var err error
				resultBeforePublication, err = store.ReadAcceptedResult(AcceptedResultMaxBytes)
				return err
			})
		}
		if _, err := m.RunOnce(t.Context(), store); err != nil {
			t.Fatal(err)
		}
	}
	run(now, store)
	firstEvents, firstReviews := snapshot()
	if !slices.Equal(firstEvents, []string{"verify request", "permission", "reaction", "verify request", "permission",
		"model", "verify request", "permission", "publish"}) ||
		len(firstReviews) != 1 || !strings.Contains(firstReviews[0].Body, "No findings.") || len(store.Data.Processed) != 1 {
		t.Fatalf("first poll: events=%v reviews=%v processed=%v", firstEvents, firstReviews, store.Data.Processed)
	}
	// Restore the durable reservation and result as they stood immediately before the GitHub write.
	// Keep the observed publication receipt to simulate a crash before terminal completion.
	if !acceptedBeforePublication.Assigned || !acceptedBeforePublication.ResultReady || len(resultBeforePublication) == 0 {
		t.Fatalf("selected publication snapshot is incomplete: accepted=%+v result bytes=%d", acceptedBeforePublication, len(resultBeforePublication))
	}
	if err := store.WithLock(func() error {
		store.Data.Accepted = &acceptedBeforePublication
		store.Data.JobFailures[fmt.Sprintf("%x", jobID)] = failureBeforePublication
		delete(store.Data.Processed, fmt.Sprintf("%x", jobID))
		delete(store.Data.TerminalDecisions, fmt.Sprintf("%x", jobID))
		if err := store.WriteAcceptedResult(resultBeforePublication); err != nil {
			return err
		}
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	store = reopenedStore
	if len(store.Data.Processed) != 0 {
		t.Fatalf("crash simulation retained completion checkpoint: %v", store.Data.Processed)
	}
	run(now.Add(time.Minute), store)
	allEvents, allReviews := snapshot()
	if !slices.Equal(allEvents, firstEvents) || len(allReviews) != 1 || len(store.Data.Processed) != 1 ||
		store.Data.PublicationWrites[fmt.Sprintf("%x", jobID)].Status != state.PublicationWriteObserved {
		t.Fatalf("replayed poll: events=%v reviews=%v processed=%v", allEvents, allReviews, store.Data.Processed)
	}
}
