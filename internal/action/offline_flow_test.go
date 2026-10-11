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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

// Exercise the real API client, monitor, executor, and Git checkout together.
// All GitHub responses come from this local server; Git uses a local bare repo.
func TestOfflineReviewPollRecoversAfterPublication(t *testing.T) {
	_, head := auditGitFixture(t)

	now := time.Date(2026, 10, 9, 8, 1, 0, 0, time.UTC)
	requestAt := now.Add(-time.Second) // Remains inside the next poll's cursor overlap.
	requestBody := "@linkdataminion review"
	parsed, err := command.ParseForLogin(requestBody, "linkdataminion")
	if err != nil {
		t.Fatal(err)
	}
	jobID := sha256.Sum256([]byte("o/r#issue-comment:42\x00" + parsed.Key))
	marker := fmt.Sprintf("<!-- linkdataminion:%x -->", jobID)
	request := github.Comment{
		ID: 42, NodeID: "request-node", Body: requestBody,
		User: github.User{Login: "writer"}, CreatedAt: requestAt, UpdatedAt: requestAt,
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
		case r.Method == http.MethodGet && r.URL.Path == "/notifications":
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
				ID: "notification-7", Repository: github.Repository{FullName: "o/r", Private: true},
				Subject:   github.Subject{Type: "PullRequest", URL: "https://api.github.com/repos/o/r/pulls/7"},
				UpdatedAt: requestAt,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = fmt.Fprintf(w, `{"title":"PR","created_at":"2026-10-08T00:00:00Z","base":{"ref":"main"},"head":{"sha":%q}}`, head)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]github.Comment{request})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			_, current := snapshot()
			_ = json.NewEncoder(w).Encode(current)
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
			case strings.Contains(input.Query, "addReaction") && input.Variables.ID == request.NodeID:
				record("reaction")
				_, _ = fmt.Fprint(w, `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":true}}}}}`)
			case strings.Contains(input.Query, "... on Comment") && input.Variables.ID == request.NodeID:
				record("verify request")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"body": requestBody, "author": map[string]string{"login": "writer"}}}})
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
			_, _ = fmt.Fprint(w, `{"permission":"write"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main","private":true}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/")
			_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, sha)
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
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	run := func(at time.Time, store *state.Store) {
		t.Helper()
		client := github.New("offline-token")
		client.BaseURL, client.HTTPClient = server.URL, server.Client()
		executor := &Executor{GitHub: client, BotLogin: "linkdataminion", Store: store, WorkRoot: t.TempDir(),
			RunCodex: func(_ context.Context, dir, prompt string, writable bool, outputDir string) (codexcli.Result, error) {
				if _, err := os.Stat(dir); err != nil || writable || outputDir != "" || !strings.Contains(prompt, head) {
					return codexcli.Result{}, fmt.Errorf("unexpected Codex invocation: dir=%q writable=%t output=%q stat=%v", dir, writable, outputDir, err)
				}
				record("codex")
				return codexcli.Result{Answer: "No findings."}, nil
			},
		}
		m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60,
			Repositories: []config.Repository{{Name: "o/r", Actions: []string{"review"}, AllowedActors: []string{"writer"}}}},
			GitHub: client, Executor: executor, Now: func() time.Time { return at }}
		if _, err := m.RunOnce(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	run(now, store)
	firstEvents, firstReviews := snapshot()
	if !slices.Equal(firstEvents, []string{"verify request", "permission", "reaction", "codex", "publish"}) ||
		len(firstReviews) != 1 || !strings.Contains(firstReviews[0].Body, "No findings.") || len(store.Data.Processed) != 1 {
		t.Fatalf("first poll: events=%v reviews=%v processed=%v", firstEvents, firstReviews, store.Data.Processed)
	}
	// Simulate a crash after GitHub accepted the review but before the final state save.
	delete(store.Data.Processed, fmt.Sprintf("%x", jobID))
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.Data.Processed) != 0 {
		t.Fatalf("crash simulation retained completion checkpoint: %v", store.Data.Processed)
	}
	run(now.Add(time.Minute), store)
	allEvents, allReviews := snapshot()
	if !slices.Equal(allEvents, []string{"verify request", "permission", "reaction", "codex", "publish",
		"verify request", "permission", "reaction", "verify review"}) || len(allReviews) != 1 || len(store.Data.Processed) != 1 {
		t.Fatalf("replayed poll: events=%v reviews=%v processed=%v", allEvents, allReviews, store.Data.Processed)
	}
}
