package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

func TestModeledFixRepliesRecoverLostPostWithoutDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name, wantReply string
		branchReady     bool
	}{
		{name: "no changes", wantReply: "AI returned a response without changing files."},
		{name: "changed result branch reply", branchReady: true, wantReply: "Fix branch `minion/fix-7-reviewed` is ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := testJob(command.Fix)
			job.Identity.Repository = "o/r"
			result := completedRecoveryResult(job)
			if tc.branchReady {
				result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}
			}
			store, path, request := savedRecoveryFixture(t, job, result)
			if tc.branchReady {
				store.Data.PendingFixes[request.ID] = state.PendingFix{SHA: portableHead}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
			}

			var posted atomic.Pointer[string]
			var posts, provenance atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, publicRepositoryJSON)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
					var replies []map[string]any
					if body := posted.Load(); body != nil {
						replies = []map[string]any{{
							"id": 1, "node_id": "reply-node", "body": *body,
							"user":       map[string]any{"id": 8, "login": "linkdataminion"},
							"updated_at": request.Source.UpdatedAt.Add(time.Second),
						}}
					}
					_ = json.NewEncoder(w).Encode(replies)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					var input struct{ Query string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode GraphQL read: %v", err)
						return
					}
					if strings.Contains(input.Query, "bodyText") {
						_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Task","bodyText":"Inspect source","lastEditedAt":null}}}`)
					} else if body := posted.Load(); body != nil && strings.Contains(input.Query, "node(id:") {
						provenance.Add(1)
						_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"id":8,"login":"linkdataminion"}}}}`, *body)
					} else {
						t.Errorf("unexpected GraphQL read: %q", input.Query)
						http.Error(w, "unexpected GraphQL read", http.StatusBadRequest)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					if posts.Add(1) != 1 {
						http.Error(w, "duplicate reply", http.StatusInternalServerError)
						return
					}
					var input struct{ Body string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode reply: %v", err)
						return
					}
					if !strings.Contains(input.Body, tc.wantReply) || !strings.HasSuffix(input.Body, "\n\n"+request.Marker()) {
						t.Errorf("unexpected reply: %q", input.Body)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Errorf("read publication ledger before POST: %v", err)
					} else {
						var durable state.Data
						if err := json.Unmarshal(data, &durable); err != nil || durable.PublicationWrites[request.ID].Status != state.PublicationWritePending {
							t.Errorf("publication intent was not durable before POST: err=%v write=%+v", err, durable.PublicationWrites[request.ID])
						}
					}
					posted.Store(&input.Body)
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"id":1}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client := testGitHub(server, "token")
			underlying := client.HTTPClient.Transport
			client.HTTPClient.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := underlying.RoundTrip(r)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/issues/7/comments" {
					return response, err
				}
				_ = response.Body.Close()
				return nil, io.ErrUnexpectedEOF
			})
			e := Orchestrator{Store: store, GitHub: client, BotLogin: "linkdataminion", BotID: 8,
				ValidatePublication: permitSelectedPublication, Runner: Runner{runModel: forbidModel(t)}}
			prepared := &preparedAction{job: job, request: request, issueNodeID: "issue-node",
				repository: github.Repository{FullName: "o/r", DefaultBranch: "main"},
				fork:       github.Repository{FullName: "bot/r"}, branch: "minion/fix-7-reviewed"}
			var err error
			if tc.branchReady {
				err = e.publishFixReply(t.Context(), prepared)
			} else {
				err = e.publishFix(t.Context(), prepared, result, publication{answer: result.Answer})
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) || posts.Load() != 1 {
				t.Fatalf("accepted POST did not retain lost response: err=%v posts=%d", err, posts.Load())
			}
			if write := store.Data.PublicationWrites[request.ID]; write.Kind != "comment" || write.Status != state.PublicationWritePending {
				t.Fatalf("lost response did not retain pending write: %+v", write)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			e.Store, e.GitHub = reopened, testGitHub(server, "token")
			if err := e.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 || provenance.Load() != 1 {
				t.Fatalf("recovery reposted reply or failed provenance: err=%v posts=%d provenance=%d", err, posts.Load(), provenance.Load())
			}
			if write := reopened.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWriteObserved || write.BotID != 8 {
				t.Fatalf("recovered reply was not observed: %+v", write)
			}
			if decision := reopened.Data.TerminalDecisions[request.ID]; decision.Outcome != "succeeded" {
				t.Fatalf("recovered reply did not complete action: %+v", decision)
			}
		})
	}
}

func TestPreWorkerFixReplyKeepsAcknowledgementPath(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	reserveActionRequest(t, store, &request, true)
	store.Data.PendingFixes[request.ID] = state.PendingFix{SHA: portableHead}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	e := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot",
		ValidatePublication: permitSelectedPublication}
	p := &preparedAction{request: request,
		repository: github.Repository{FullName: "o/r", DefaultBranch: "main"},
		fork:       github.Repository{FullName: "bot/r"}, branch: "minion/fix-7-reviewed"}
	if err := e.publishFixReply(t.Context(), p); err != nil || posts.Load() != 1 || len(store.Data.PublicationWrites) != 0 {
		t.Fatalf("pre-worker reply changed path: err=%v posts=%d ledger=%v", err, posts.Load(), store.Data.PublicationWrites)
	}
}
