package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func TestExpiredIssuedCommentNeverRepostsOrRerunsModel(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8, PublicationGrace: time.Minute}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[request.ID]
	write.StartedAt = time.Now().UTC().Add(-2 * time.Minute)
	write.Deadline = write.StartedAt.Add(time.Minute)
	store.Data.PublicationWrites[request.ID] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var lookups, taskPosts, unexpected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			lookups.Add(1)
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			taskPosts.Add(1)
			http.Error(w, "second task publication", http.StatusInternalServerError)
		default:
			unexpected.Add(1)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.GitHub = testGitHub(server, "token")
	reconciler := &monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion"}, GitHub: orchestrator.GitHub}
	if err := reconciler.ReconcilePublicationWrites(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.Data.PublicationWrites[request.ID]; got.Status != state.PublicationWriteUncertain || got.ResolvedAt.IsZero() {
		t.Fatalf("expired issued write was not closed as uncertain: %+v", got)
	}
	var workChecks, modelCalls int
	orchestrator.CheckWorkRoot = func() error { workChecks++; return nil }
	orchestrator.Runner = Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
		modelCalls++
		return aicli.Result{}, errors.New("model should not run")
	}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("recovered uncertain publication = %v", err)
	}
	if err := orchestrator.Execute(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("executed uncertain publication = %v", err)
	}
	if lookups.Load() != 1 || taskPosts.Load() != 0 || unexpected.Load() != 0 || workChecks != 0 || modelCalls != 0 {
		t.Fatalf("uncertain write started work or issued another publication: lookups=%d posts=%d unexpected=%d work=%d models=%d",
			lookups.Load(), taskPosts.Load(), unexpected.Load(), workChecks, modelCalls)
	}
}

func TestTaskPublicationUsesConfiguredDefaultGrace(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := e.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[request.ID]
	if got, want := write.Deadline.Sub(write.StartedAt), time.Duration(config.DefaultPublicationGraceSeconds)*time.Second; got != want {
		t.Fatalf("publication grace = %s, want %s", got, want)
	}
}

func TestUnissuedPublicationIsClearedWhenDeadlineExpiresAfterSave(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	before := request.Deadline.Add(-time.Second)
	checks := 0
	err := e.beginTaskPublicationAt(t.Context(), request, "comment", func() time.Time {
		checks++
		if checks == 1 {
			return before
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var saved state.Data
		if unmarshalErr := json.Unmarshal(data, &saved); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		if write := saved.PublicationWrites[request.ID]; write.Status != state.PublicationWritePending {
			t.Fatalf("issued write was not saved before the deadline check: %+v", write)
		}
		return request.Deadline.Add(time.Nanosecond)
	})
	if !errors.Is(err, context.DeadlineExceeded) || checks != 2 || len(store.Data.PublicationWrites) != 0 {
		t.Fatalf("known unissued write remained pending: err=%v checks=%d writes=%v", err, checks, store.Data.PublicationWrites)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(reopened.Data.PublicationWrites) != 0 {
		t.Fatalf("unissued write survived restart: %+v", reopened.Data.PublicationWrites)
	}
}

func TestTaskPublicationRequiresValidBotIdentityBeforeWrite(t *testing.T) {
	for _, login := range []string{"", "owner/bot"} {
		t.Run(login, func(t *testing.T) {
			store := openState(t, time.Now())
			request := usageJob()
			e := Orchestrator{Store: store, BotLogin: login, BotID: 8}
			if err := e.beginTaskPublication(t.Context(), request, "comment"); !errors.Is(err, monitor.ErrAcceptanceState) || len(store.Data.PublicationWrites) != 0 {
				t.Fatalf("invalid bot login issued a write: err=%v writes=%v", err, store.Data.PublicationWrites)
			}
		})
	}
	t.Run("missing ID", func(t *testing.T) {
		store := openState(t, time.Now())
		e := Orchestrator{Store: store, BotLogin: "linkdataminion"}
		if err := e.beginTaskPublication(t.Context(), usageJob(), "comment"); !errors.Is(err, monitor.ErrAcceptanceState) || len(store.Data.PublicationWrites) != 0 {
			t.Fatalf("missing bot ID issued a write: err=%v writes=%v", err, store.Data.PublicationWrites)
		}
	})
}

func TestObservedTaskPublicationUpdatesEarlierUncertainResolution(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := e.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[request.ID]
	write.StartedAt = time.Now().UTC().Add(-2 * time.Minute)
	write.Deadline = write.StartedAt.Add(time.Minute)
	write.Status = state.PublicationWriteUncertain
	write.ResolvedAt = write.Deadline
	store.Data.PublicationWrites[request.ID] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if err := e.observeTaskPublication(request); err != nil {
		t.Fatal(err)
	}
	observed := store.Data.PublicationWrites[request.ID]
	if observed.Status != state.PublicationWriteObserved || observed.ResolvedAt.Before(started) || observed.ResolvedAt.After(time.Now().UTC()) {
		t.Fatalf("observed write retained stale resolution: %+v", observed)
	}
}

func TestIssuedCommentReconcilesLostResponseAfterRestartWithoutSecondWrite(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8, PublicationGrace: time.Minute}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if write := reopened.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWritePending || write.BotLogin != "linkdataminion" || write.BotID != 8 {
		t.Fatalf("issued write did not survive restart: %+v", write)
	}
	var lookups, posts atomic.Int32
	markerBody := "Published before the response was lost\n\n" + request.Marker()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			if lookups.Add(1) == 1 {
				_, _ = fmt.Fprint(w, `[]`)
			} else {
				_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"id":8,"login":"renamed-bot"},"updated_at":%q}]`,
					markerBody, request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"id":8,"login":"renamed-bot"}}}}`, markerBody)
		default:
			posts.Add(1)
			http.Error(w, "unexpected second write", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.Store, orchestrator.GitHub, orchestrator.BotLogin, orchestrator.BotID = reopened, testGitHub(server, "token"), "replacement-bot", 9
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) || posts.Load() != 0 {
		t.Fatalf("unobserved issued write was retried: err=%v posts=%d", err, posts.Load())
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 0 || lookups.Load() != 2 {
		t.Fatalf("published marker was not reconciled: err=%v posts=%d lookups=%d", err, posts.Load(), lookups.Load())
	}
	if write := reopened.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWriteObserved || write.ResolvedAt.IsZero() {
		t.Fatalf("observed remote effect was not saved: %+v", write)
	}
	if got := reopened.Data.TerminalDecisions[request.ID].Outcome; got != "succeeded" {
		t.Fatalf("reconciled effect did not record successful local outcome: %q", got)
	}
}

func TestTaskPublicationRecoversAcceptedPostWithLostResponseAfterBotSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		action     command.Action
		status     int
	}{
		{"comment", "/repos/owner/repo/issues/7/comments", command.Comment, http.StatusCreated},
		{"review", "/repos/owner/repo/pulls/7/reviews", command.Review, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := testJob(tc.action)
			store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
			nodeID := "reply-node"
			if tc.action == command.Review {
				nodeID = "review-node"
			}
			var posts, provenance atomic.Int32
			var mu sync.Mutex
			var postedBody string
			body := func() string {
				mu.Lock()
				defer mu.Unlock()
				return postedBody
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
					if current := body(); current != "" && tc.action == command.Comment {
						_ = json.NewEncoder(w).Encode([]map[string]any{{
							"id": 1, "node_id": nodeID, "body": current,
							"user":       map[string]any{"id": 8, "login": "linkdataminion"},
							"updated_at": request.Source.UpdatedAt.Add(time.Second),
						}})
					} else {
						_, _ = fmt.Fprint(w, `[]`)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"owner/repo","owner":{"id":2,"login":"owner"},"default_branch":"main"}`)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					var input struct {
						Query     string `json:"query"`
						Variables struct {
							ID string `json:"id"`
						} `json:"variables"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode GraphQL request: %v", err)
						return
					}
					current := body()
					if strings.Contains(input.Query, "timelineItems") && tc.action == command.Review {
						var nodes []map[string]any
						if current != "" {
							nodes = []map[string]any{{
								"id": nodeID, "fullDatabaseId": "1", "body": current,
								"author": map[string]any{"id": 8, "login": "linkdataminion"}, "state": "COMMENTED",
								"submittedAt": request.Source.UpdatedAt.Add(time.Second),
							}}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
							"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
					} else if current != "" && input.Variables.ID == nodeID {
						provenance.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
							"body": current, "author": map[string]any{"id": 8, "login": "linkdataminion"}}}})
					} else {
						t.Errorf("unexpected GraphQL query or node: %q %q", input.Query, input.Variables.ID)
						http.Error(w, "unexpected GraphQL request", http.StatusBadRequest)
					}
				case r.Method == http.MethodPost && r.URL.Path == tc.path:
					var input struct {
						Body     string `json:"body"`
						CommitID string `json:"commit_id"`
						Event    string `json:"event"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode publication: %v", err)
						return
					}
					if !strings.Contains(input.Body, "Saved complete answer") || !strings.HasSuffix(input.Body, "\n\n"+request.Marker()) ||
						tc.action == command.Review && (input.CommitID != job.Source.Head || input.Event != "COMMENT") {
						t.Errorf("unexpected publication: %+v", input)
					}
					if posts.Add(1) != 1 {
						http.Error(w, "unexpected second publication", http.StatusInternalServerError)
						return
					}
					mu.Lock()
					postedBody = input.Body
					mu.Unlock()
					w.WriteHeader(tc.status)
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
				if err != nil || r.Method != http.MethodPost || r.URL.Path != tc.path {
					return response, err
				}
				if response.StatusCode != tc.status {
					t.Errorf("publication was not accepted: status=%d", response.StatusCode)
				}
				_ = response.Body.Close() // The server accepted the POST; its response is lost to the client.
				return nil, io.ErrUnexpectedEOF
			})
			orchestrator := Orchestrator{Store: store, GitHub: client, BotLogin: "linkdataminion", BotID: 8, WorkRoot: t.TempDir()}
			if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) || !errors.Is(err, io.ErrUnexpectedEOF) || posts.Load() != 1 {
				t.Fatalf("accepted POST did not leave an issued write: err=%v posts=%d", err, posts.Load())
			}
			if write := store.Data.PublicationWrites[request.ID]; write.Kind != string(tc.action) || write.BotLogin != "linkdataminion" || write.BotID != 8 || write.Status != state.PublicationWritePending {
				t.Fatalf("lost response did not preserve issued write: %+v", write)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			orchestrator.Store, orchestrator.GitHub, orchestrator.BotLogin, orchestrator.BotID = reopened, testGitHub(server, "token"), "replacement-bot", 9
			if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 || provenance.Load() != 1 {
				t.Fatalf("restart did not reconcile accepted POST: err=%v posts=%d provenance=%d", err, posts.Load(), provenance.Load())
			}
			if write := reopened.Data.PublicationWrites[request.ID]; write.Kind != string(tc.action) || write.BotLogin != "linkdataminion" || write.BotID != 8 || write.Marker != request.Marker() || write.Status != state.PublicationWriteObserved {
				t.Fatalf("marker publication was not observed: %+v", write)
			}
			if got := reopened.Data.TerminalDecisions[request.ID].Outcome; got != "succeeded" {
				t.Fatalf("reconciled publication did not finish the action: %q", got)
			}
		})
	}
}

func TestIssuedTaskCommentResponseIsObservedBeforeTerminalDecision(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command.Action = command.Comment
	request.Deadline = time.Now().Add(time.Minute)
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
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "linkdataminion", BotID: 8}
	if err := e.publishTaskComment(t.Context(), request, "answer"); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || store.Data.PublicationWrites[request.ID].Status != state.PublicationWriteObserved ||
		len(store.Data.TerminalDecisions) != 0 {
		t.Fatalf("remote effect was not recorded before the terminal decision: posts=%d state=%+v", posts.Load(), store.Data)
	}
	if err := e.publishTaskComment(t.Context(), request, "answer"); !errors.Is(err, monitor.ErrPostModelPublication) || posts.Load() != 1 {
		t.Fatalf("issued comment was posted again: err=%v posts=%d", err, posts.Load())
	}
}

func TestAcceptedTaskCommentAfterDeadlineKeepsRemoteEffectAndFirstDecision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
		posts := 0
		client := github.New("token")
		client.BaseURL = "https://api.example.test"
		client.HTTPClient = &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusOK, `{"id":1,"full_name":"owner/repo","owner":{"id":2},"default_branch":"main","visibility":"public"}`
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/7/comments":
				posts++
				if write := store.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWritePending {
					t.Fatalf("POST preceded durable issued-write record: %+v", write)
				}
				if !time.Now().Before(request.Deadline) {
					t.Fatal("action deadline expired before GitHub accepted the POST")
				}
				time.Sleep(time.Until(request.Deadline) + time.Nanosecond)
				if err := store.WithLock(func() error {
					_, err := store.DecideTerminalLocked(request.ID, request.AttemptID, "expired", time.Now())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				status, body = http.StatusCreated, `{"id":1}`
			default:
				t.Fatalf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})}
		e := &Orchestrator{Store: store, GitHub: client, BotLogin: "linkdataminion", BotID: 8}
		if err := e.publishTaskComment(t.Context(), request, "answer"); err != nil {
			t.Fatal(err)
		}
		if posts != 1 || store.Data.PublicationWrites[request.ID].Status != state.PublicationWriteObserved {
			t.Fatalf("accepted POST was not observed after expiry: posts=%d write=%+v", posts, store.Data.PublicationWrites[request.ID])
		}
		if err := e.recordPublishedOutcome(request); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late publication changed terminal outcome: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := state.Open(path, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		e.Store = reopened
		if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
			t.Fatalf("recovery of late publication = %v", err)
		}
		if posts != 1 || reopened.Data.TerminalDecisions[request.ID].Outcome != "expired" ||
			reopened.Data.PublicationWrites[request.ID].Status != state.PublicationWriteObserved ||
			!reopened.Data.PublicationWrites[request.ID].ResolvedAt.After(request.Deadline) {
			t.Fatalf("recovery changed first decision or reposted: posts=%d decision=%+v write=%+v",
				posts, reopened.Data.TerminalDecisions[request.ID], reopened.Data.PublicationWrites[request.ID])
		}
	})
}

func TestIssuedReviewReconcilesUsingRecordedKindAfterCommandRemap(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.SubjectType = "PullRequest"
	request.Source.UpdatedAt = time.Now().UTC()
	request.AcceptedAt = time.Now().UTC()
	request.Deadline = request.AcceptedAt.Add(time.Minute)
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "review"); err != nil {
		t.Fatal(err)
	}
	request.Command.Action = command.Comment // The configured command changed after the review was issued.
	var timelines, provenance, writes atomic.Int32
	markerBody := "Review already published\n\n" + request.Marker()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var input struct{ Query string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(input.Query, "timelineItems") {
				timelines.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": []map[string]any{{
						"id": "review-node", "fullDatabaseId": "1", "body": markerBody,
						"author": map[string]any{"id": 8, "login": "linkdataminion"}, "state": "COMMENTED",
						"submittedAt": request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339),
					}}, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
			} else {
				provenance.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
					"body": markerBody, "author": map[string]any{"id": 8, "login": "linkdataminion"}}}})
			}
		default:
			writes.Add(1)
			http.Error(w, "unexpected write", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.GitHub = testGitHub(server, "token")
	if err := orchestrator.Execute(t.Context(), request); err != nil ||
		timelines.Load() != 1 || provenance.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("remapped command did not reconcile the issued review: err=%v timelines=%d provenance=%d writes=%d",
			err, timelines.Load(), provenance.Load(), writes.Load())
	}
	if write := store.Data.PublicationWrites[request.ID]; write.Kind != "review" || write.Status != state.PublicationWriteObserved {
		t.Fatalf("recorded review effect changed kind or was not observed: %+v", write)
	}
}

func TestIssuedCommentReconcilesUsingRecordedSinceAfterSourceEdit(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command.Action = command.Comment
	request.Source.UpdatedAt = time.Now().UTC()
	request.AcceptedAt = time.Now().UTC()
	request.Deadline = request.AcceptedAt.Add(time.Minute)
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	issuedSince := request.Source.UpdatedAt
	request.Source.UpdatedAt = issuedSince.Add(10 * time.Second) // The command was edited after its write.
	markerBody := "Comment already published\n\n" + request.Marker()
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			reads.Add(1)
			_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"id":8,"login":"linkdataminion"},"updated_at":%q}]`,
				markerBody, issuedSince.Add(time.Second).UTC().Format(time.RFC3339))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"id":8,"login":"linkdataminion"}}}}`, markerBody)
		default:
			writes.Add(1)
			http.Error(w, "unexpected write", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.GitHub = testGitHub(server, "token")
	if err := orchestrator.Execute(t.Context(), request); err != nil || reads.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("edited source lost the original write observation: err=%v reads=%d writes=%d", err, reads.Load(), writes.Load())
	}
	if write := store.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWriteObserved || !write.Since.Equal(issuedSince) {
		t.Fatalf("recorded lookup bound changed after source edit: %+v", write)
	}
}
