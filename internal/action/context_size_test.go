package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestCheckGitHubContext(t *testing.T) {
	for _, tt := range []struct {
		name, notes, title, body string
		limit                    int
		tooLarge                 bool
	}{
		{name: "combined exact", notes: "n", title: "T", body: "éé", limit: 6},
		{name: "combined over", notes: "n", title: "T", body: "ééx", limit: 6, tooLarge: true},
		{name: "rune exact", body: "é", limit: 2},
		{name: "rune crosses limit", body: "é", limit: 1, tooLarge: true},
		{name: "notes over", notes: "é", limit: 1, tooLarge: true},
		{name: "trimmed input", body: "  é  ", limit: 2},
		{name: "title exact", title: strings.Repeat("😀", 256), limit: 2 * maxActionTitleBytes},
		{name: "raw padded title over", title: " " + strings.Repeat("x", maxActionTitleBytes-1) + " ", limit: 2 * maxActionTitleBytes, tooLarge: true},
		{name: "title over", title: strings.Repeat("x", maxActionTitleBytes+1), limit: 2 * maxActionTitleBytes, tooLarge: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := monitor.Job{Source: github.Item{Body: "@linkdataminion review\n" + tt.notes}}
			orchestrator := Orchestrator{
				MaxGitHubContextBytes: tt.limit,
			}
			err := orchestrator.checkGitHubContext(job, tt.title, tt.body)
			if errors.Is(err, errGitHubContextTooLarge) != tt.tooLarge {
				t.Fatalf("checkGitHubContext() = %v, tooLarge=%t", err, tt.tooLarge)
			}
			if err != nil && !tt.tooLarge {
				t.Fatal(err)
			}
		})
	}
	var zero Orchestrator
	if got := zero.githubContextLimit(); got != config.DefaultMaxGitHubContextBytes {
		t.Fatalf("zero-value limit = %d, want %d", got, config.DefaultMaxGitHubContextBytes)
	}
	large := strings.Repeat("é", config.DefaultMaxGitHubContextBytes/2+1)
	quoted := quotedContext(large)
	if text, err := strconv.Unquote(quoted); err != nil || text != large {
		t.Fatalf("quoted context was clipped: bytes=%d, err=%v", len(text), err)
	}
}

func TestGitHubContextLimitAcrossActions(t *testing.T) {
	const title, notes, exactDescription = "T", "N", "éé"
	modelErr := errors.New("stop after observing the model prompt")
	for _, tt := range []struct {
		name, commandName, subject, skill string
		action                            command.Action
	}{
		{name: "review", commandName: "review", subject: "PullRequest", skill: testReviewSkillURL, action: command.Review},
		{name: "comment issue", commandName: "audit", subject: "Issue", skill: testReviewSkillURL, action: command.Comment},
		{name: "comment pull request", commandName: "audit", subject: "PullRequest", skill: testReviewSkillURL, action: command.Comment},
		{name: "security audit issue", commandName: "security-audit", subject: "Issue", skill: testAuditSkillURL, action: command.SecurityAudit},
		{name: "security audit pull request", commandName: "security-audit", subject: "PullRequest", skill: testAuditSkillURL, action: command.SecurityAudit},
		{name: "fix", commandName: "fix", subject: "Issue", skill: testFixSkillURL, action: command.Fix},
	} {
		for _, mode := range []string{"exact", "combined over", "description over", "notes over", "reply retry"} {
			if mode == "reply retry" && tt.name != "comment issue" {
				continue
			}
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				if tt.action == command.SecurityAudit {
					fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
				}
				installActionTestSkills(t, tt.skill)
				limit := len(notes) + len(exactDescription)
				if tt.action != command.Fix {
					limit += len(title)
				}
				description, requestNotes := exactDescription, notes
				switch mode {
				case "combined over", "reply retry":
					description += "x"
				case "description over":
					description = strings.Repeat("x", limit+1)
				case "notes over":
					requestNotes = strings.Repeat("n", limit+1)
				}
				head := strings.Repeat("a", 40)
				if mode == "exact" {
					_, head, _ = gitFixtureRemote(t)
				}
				job := monitor.Job{
					Repository:  config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{tt.commandName}},
					SubjectType: tt.subject, Number: 7, ID: strings.Repeat("a", 64),
					Source: github.Item{
						Author: github.User{ID: 7, Login: "writer"}, Body: "@linkdataminion " + tt.commandName + "\n" + requestNotes,
						UpdatedAt: time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC),
					},
					Command: command.Command{Name: tt.commandName, Action: tt.action},
				}
				var replies []github.Comment
				var postAttempts int
				var mu sync.Mutex
				respond := func(w http.ResponseWriter, value any) {
					t.Helper()
					if err := json.NewEncoder(w).Encode(value); err != nil {
						t.Error(err)
					}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
						mu.Lock()
						respond(w, replies)
						mu.Unlock()
					case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
						mu.Lock()
						postAttempts++
						fail := mode == "reply retry" && postAttempts == 1
						mu.Unlock()
						if fail {
							http.Error(w, "temporary publication failure", http.StatusServiceUnavailable)
							return
						}
						var input struct {
							Body string `json:"body"`
						}
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							t.Error(err)
						}
						comment := github.Comment{
							ID: 1, NodeID: "reply-node", Body: input.Body,
							User: github.User{Login: "linkdataminion"}, UpdatedAt: time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC),
						}
						mu.Lock()
						replies = append(replies, comment)
						mu.Unlock()
						w.WriteHeader(http.StatusCreated)
						respond(w, comment)
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
						respond(w, github.Repository{ID: 1, FullName: "o/r", DefaultBranch: "main", Private: true, Visibility: "private", Owner: github.User{ID: 2, Login: "o"}})
					case r.Method == http.MethodGet && r.URL.Path == "/repos/linkdataminion/r":
						respond(w, github.Repository{
							ID: 2, FullName: "linkdataminion/r", Fork: true, Private: true, Visibility: "private",
							Parent: &github.Repository{ID: 1, FullName: "o/r"}, Source: &github.Repository{ID: 1, FullName: "o/r"},
						})
					case serveSafeForkChecks(w, r, "linkdataminion/r", true):
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
						respond(w, []string{})
					case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/linkdataminion/r/branches/minion/"):
						http.NotFound(w, r)
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
						body := description
						if tt.action == command.Fix {
							body = "REST body is not the rendered text sent to the model"
						}
						respond(w, github.Issue{NodeID: "issue-node", Title: title, Body: body})
					case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
						respond(w, github.PullRequest{
							Title: title, Body: description,
							Base: github.PullRequestRef{Ref: "main"}, Head: github.PullRequestRef{SHA: head},
						})
					case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
						respond(w, map[string]any{
							"sha":       strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"),
							"truncated": false, "tree": []any{},
						})
					case r.Method == http.MethodPost && r.URL.Path == "/graphql":
						var input struct {
							Query string `json:"query"`
						}
						if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
							t.Error(err)
						}
						switch {
						case strings.Contains(input.Query, "timelineItems"):
							respond(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
								"timelineItems": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
							}}}})
						case strings.Contains(input.Query, "bodyText"):
							respond(w, map[string]any{"data": map[string]any{"node": map[string]any{
								"title": title, "bodyText": description, "lastEditedAt": nil,
							}}})
						default:
							mu.Lock()
							body := ""
							if len(replies) != 0 {
								body = replies[0].Body
							}
							mu.Unlock()
							respond(w, map[string]any{"data": map[string]any{"node": map[string]any{
								"body": body, "author": map[string]any{"login": "linkdataminion"},
							}}})
						}
					default:
						t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				client := testGitHub(server, "test-token")
				var store *state.Store
				if tt.action == command.Fix {
					store = openState(t, job.Source.UpdatedAt)
				}
				var modelCalls int
				orchestrator := &Orchestrator{WorkRoot: t.TempDir(), GitHub: client,
					BotLogin:              "linkdataminion",
					Store:                 store,
					MaxGitHubContextBytes: limit,
					Commands:              map[string]config.CommandMapping{job.Command.Name: {Action: string(tt.action), Skills: []string{tt.skill}}},
					Runner: Runner{
						runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
							prompt := modelRequest.prompt
							modelCalls++
							if !strings.Contains(prompt, strconv.Quote(description)) || !strings.Contains(prompt, strconv.Quote(requestNotes)) ||
								(tt.action != command.Fix && !strings.Contains(prompt, strconv.Quote(title))) || strings.Contains(prompt, "[truncated]") {
								t.Errorf("model prompt lost GitHub text or clipped it: %q", prompt)
							}
							return aicli.Result{}, modelErr
						},
					},
				}
				err := orchestrator.Execute(context.Background(), job)
				if mode == "exact" {
					if !errors.Is(err, modelErr) || modelCalls != 1 {
						t.Fatalf("exact-limit execution = %v, model calls = %d; want model error and one call", err, modelCalls)
					}
					return
				}
				if mode == "reply retry" {
					if err == nil || errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrPostModelPublication) || modelCalls != 0 {
						t.Fatalf("failed refusal publication = %v, model calls = %d; want retryable error and zero calls", err, modelCalls)
					}
					err = orchestrator.Execute(context.Background(), job)
				}
				if err != nil || modelCalls != 0 {
					t.Fatalf("oversized execution = %v, model calls = %d; want fixed refusal and zero calls", err, modelCalls)
				}
				mu.Lock()
				got := append([]github.Comment(nil), replies...)
				mu.Unlock()
				want := fmt.Sprintf("Request exceeds the GitHub context limits: %d UTF-8 bytes combined and %d bytes per title. Shorten the title, thread text or requester notes included in this action, then post a new command.\n\n%s", limit, maxActionTitleBytes, job.Marker())
				if len(got) != 1 || got[0].Body != want {
					t.Fatalf("refusal replies = %+v; want %q", got, want)
				}
				if err := orchestrator.Execute(context.Background(), job); err != nil || modelCalls != 0 {
					t.Fatalf("repeated rejected request = %v, model calls = %d", err, modelCalls)
				}
				mu.Lock()
				replyCount := len(replies)
				mu.Unlock()
				if replyCount != 1 {
					t.Fatalf("rejected request posted %d replies", replyCount)
				}
			})
		}
	}
}
