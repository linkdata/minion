package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testClient(server *httptest.Server) *Client {
	c := New("test-token")
	c.BaseURL = server.URL
	c.HTTPClient = server.Client()
	return c
}

func TestGitTreeUsesExactSHAAndBoundsResponse(t *testing.T) {
	sha := strings.Repeat("a", 40)
	response := fmt.Sprintf(`{"sha":%q,"truncated":false,"tree":[{"path":"file","type":"blob","mode":"100644","size":23}]}`, sha)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/git/trees/"+sha || r.URL.Query().Get("recursive") != "1" {
			t.Errorf("unexpected tree request: %s %s", r.Method, r.URL.String())
		}
		_, _ = fmt.Fprint(w, response)
	}))
	defer server.Close()
	client := testClient(server)
	tree, err := client.GitTree(context.Background(), "o/r", sha)
	if err != nil || tree.SHA != sha || len(tree.Tree) != 1 || tree.Tree[0].Mode != "100644" || tree.Tree[0].Size == nil || *tree.Tree[0].Size != 23 {
		t.Fatalf("GitTree() = (%+v, %v)", tree, err)
	}
	if _, err := client.GitTree(context.Background(), "o/r", strings.Repeat("/", 40)); err == nil {
		t.Fatal("invalid tree SHA accepted")
	}
	response = fmt.Sprintf(`{"sha":%q,"truncated":false,"tree":[]}`, sha) + strings.Repeat(" ", maxTreeResponseBytes)
	if _, err := client.GitTree(context.Background(), "o/r", sha); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized tree response error = %v", err)
	}
}

func TestReactEyesUsesExactNodeAndChecksGraphQLErrors(t *testing.T) {
	var calls int
	response := `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":true}}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected reaction request: %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode reaction request: %v", err)
		}
		if input.Variables["id"] != "review-node-14" || !strings.Contains(input.Query, "content: EYES") {
			t.Errorf("wrong reaction target or type: %+v", input)
		}
		if _, err := fmt.Fprint(w, response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := testClient(server)
	if err := c.ReactEyes(context.Background(), ""); err == nil || calls != 0 {
		t.Fatalf("missing node ID accepted or sent: %v, calls=%d", err, calls)
	}
	for range 2 {
		if err := c.ReactEyes(context.Background(), "review-node-14"); err != nil {
			t.Fatal(err)
		}
	}
	response = `{"errors":[{"message":"Resource not accessible"}]}`
	if err := c.ReactEyes(context.Background(), "review-node-14"); err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("GraphQL error ignored: %v", err)
	}
	response = `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":false}}}}}`
	if err := c.ReactEyes(context.Background(), "review-node-14"); err == nil {
		t.Fatal("unconfirmed reaction accepted")
	}
}

func TestCurrentItemUsesLiveBodyAndEditProvenance(t *testing.T) {
	response := `{"data":{"node":{"body":"@linkdataminion review","author":{"login":"allowed","id":7},"editor":{"login":"excluded"},"lastEditedAt":"2026-10-09T08:00:00Z"}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected provenance request: %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Variables["id"] != "comment-node" || !strings.Contains(input.Query, "lastEditedAt") || !strings.Contains(input.Query, "editor { login }") || !strings.Contains(input.Query, "id: databaseId") {
			t.Errorf("provenance query omitted edit metadata: %+v", input)
		}
		if _, err := fmt.Fprint(w, response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := testClient(server)
	item := Item{NodeID: "comment-node", Body: "stale body", Author: User{Login: "allowed"}}
	current, err := client.CurrentItem(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if current.Body != "@linkdataminion review" || !current.Edited || current.Editor.Login != "excluded" || current.Author.Login != "allowed" || current.Author.ID != 7 {
		t.Fatalf("lost live body or editor: %+v", current)
	}
	response = `{"data":{"node":{"body":"@linkdataminion review","author":{"login":"allowed"},"editor":null,"lastEditedAt":null}}}`
	current, err = client.CurrentItem(context.Background(), item)
	if err != nil || current.Edited {
		t.Fatalf("unedited original command rejected: item=%+v err=%v", current, err)
	}
	response = `{"errors":[{"message":"Resource not accessible"}]}`
	if _, err := client.CurrentItem(context.Background(), item); err == nil {
		t.Fatal("GraphQL provenance failure did not fail closed")
	}
}

func TestCurrentIssueContentUsesRenderedTextAndEditTime(t *testing.T) {
	response := `{"data":{"node":{"title":"Issue title","bodyText":"Visible request","lastEditedAt":"2026-10-09T08:00:00Z"}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected issue request: %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Variables["id"] != "issue-node" || !strings.Contains(input.Query, "bodyText") || !strings.Contains(input.Query, "lastEditedAt") {
			t.Errorf("issue query omitted rendered text or edit time: %+v", input)
		}
		_, _ = fmt.Fprint(w, response)
	}))
	defer server.Close()
	client := testClient(server)
	if _, err := client.CurrentIssueContent(context.Background(), ""); err == nil {
		t.Fatal("missing issue node ID accepted")
	}
	content, err := client.CurrentIssueContent(context.Background(), "issue-node")
	if err != nil || content.Title != "Issue title" || content.BodyText != "Visible request" || content.LastEditedAt == nil {
		t.Fatalf("content = %+v, error = %v", content, err)
	}
	response = `{"errors":[{"message":"Resource not accessible"}]}`
	if _, err := client.CurrentIssueContent(context.Background(), "issue-node"); err == nil {
		t.Fatal("GraphQL error did not fail closed")
	}
}

func TestListNotificationsPaginatesAndRepeatsAfterFailedAction(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header missing")
		}
		calls = append(calls, r.URL.RawQuery)
		if r.URL.Query().Get("all") != "true" || r.URL.Query().Get("participating") != "true" {
			t.Errorf("notification filters missing: %s", r.URL.RawQuery)
		}
		if got := r.URL.Query().Get("since"); got != "2026-10-09T06:59:59Z" {
			t.Errorf("notification cursor must overlap exclusive, second-precision filter: %q", got)
		}
		w.Header().Set("X-Poll-Interval", "90")
		if got := r.Header.Get("If-Modified-Since"); got != "" {
			t.Errorf("conditional request could hide a failed command: %q", got)
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://api.github.com/notifications?page=2>; rel="next"`)
			if _, err := fmt.Fprint(w, `[ {"id":"first"} ]`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `[ {"id":"second"} ]`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := testClient(server)
	since := time.Date(2026, 10, 9, 7, 0, 0, 500_000_000, time.UTC)
	rows, interval, err := c.ListNotifications(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if interval != 90*time.Second || len(rows) != 2 || rows[0].ID != "first" || rows[1].ID != "second" {
		t.Fatalf("unexpected first response: rows=%v interval=%s", rows, interval)
	}
	rows, interval, err = c.ListNotifications(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || interval != 90*time.Second {
		t.Fatalf("retry lost notifications: rows=%v interval=%s", rows, interval)
	}
	if len(calls) != 4 {
		t.Fatalf("expected two pages on each poll, got %d calls", len(calls))
	}
}

func TestPermissionSurfacesForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		if _, err := fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	_, err := testClient(server).Permission(context.Background(), "example-owner/example-repo", "alice")
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusForbidden || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("expected a clear 403 status error, got %v", err)
	}
}

func TestRepositoryNeverFollowsRedirectOutsideWhitelist(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if _, err := fmt.Fprint(w, `{"full_name":"other/repo"}`); err != nil {
			t.Error(err)
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/repos/other/repo")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	if _, err := testClient(source).Repository(context.Background(), "example-owner/example-repo"); err == nil || redirected {
		t.Fatalf("redirect followed or ignored: err=%v, redirected=%v", err, redirected)
	}
}

func TestRepositoryRejectsMismatchedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, `{"full_name":"other/repo"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	if _, err := testClient(server).Repository(context.Background(), "example-owner/example-repo"); err == nil {
		t.Fatal("accepted a different repository under whitelisted URL")
	}
}

func TestRepositoryInvitationsListAndAccept(t *testing.T) {
	var accepted int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/repository_invitations":
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("invitation list must request 100 per page: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<https://api.github.com/user/repository_invitations?page=2>; rel="next"`)
				if _, err := fmt.Fprint(w, `[{"id":17,"repository":{"full_name":"o/allowed","owner":{"login":"o"}},"invitee":{"login":"linkdataminion"},"inviter":{"login":"alice"},"permissions":"write","created_at":"2026-10-09T08:00:00Z","expired":false,"html_url":"https://github.com/o/allowed/invitations"}]`); err != nil {
					t.Error(err)
				}
				return
			}
			if _, err := fmt.Fprint(w, `[{"id":18,"repository":{"full_name":"o/unlisted"},"invitee":{"login":"linkdataminion"},"permissions":"read","expired":true}]`); err != nil {
				t.Error(err)
			}
		case r.Method == http.MethodPatch && r.URL.Path == "/user/repository_invitations/17":
			accepted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	invitations, err := c.ListRepositoryInvitations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(invitations) != 2 || invitations[0].ID != 17 || invitations[0].Repository.FullName != "o/allowed" || invitations[0].Repository.Owner.Login != "o" || invitations[0].Permissions != "write" || invitations[0].Invitee.Login != "linkdataminion" || invitations[0].Inviter.Login != "alice" || invitations[0].CreatedAt.IsZero() || invitations[1].Repository.FullName != "o/unlisted" || !invitations[1].Expired {
		t.Fatalf("wrong invitation metadata or pagination: %+v", invitations)
	}
	if err := c.AcceptRepositoryInvitation(context.Background(), invitations[0].ID); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 {
		t.Fatalf("expected one acceptance, got %d", accepted)
	}
	if err := c.AcceptRepositoryInvitation(context.Background(), 0); err == nil {
		t.Fatal("expected invalid invitation ID to fail")
	}
}

func TestAcceptRepositoryInvitationSurfacesForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		if _, err := fmt.Fprint(w, `{"message":"Resource not accessible"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	err := testClient(server).AcceptRepositoryInvitation(context.Background(), 17)
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusForbidden {
		t.Fatalf("expected typed 403, got %v", err)
	}
}

func TestPullRequestItemsCoverEveryCommentSurface(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/o/r/") && r.URL.Path != "/graphql" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/pulls/7":
			if _, err := fmt.Fprint(w, `{"number":7,"node_id":"pull-node-7","title":"PR title","body":"old command","user":{"login":"alice"},"created_at":"2026-10-08T08:00:00Z","updated_at":"2026-10-09T08:00:05Z","html_url":"https://github.com/o/r/pull/7","head":{"ref":"feature","sha":"abc"},"base":{"ref":"main","sha":"def"}}`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/issues/7/comments":
			if since := r.URL.Query().Get("since"); since != "" && since != "2026-10-09T07:59:59Z" {
				t.Errorf("missing overlap for issue comments: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<https://api.github.com/repos/o/r/issues/7/comments?page=2>; rel="next"`)
				if _, err := fmt.Fprint(w, `[{"id":11,"node_id":"issue-comment-node-11","body":"first issue comment","user":{"login":"bob"},"updated_at":"2026-10-09T08:00:01Z"}]`); err != nil {
					t.Error(err)
				}
			} else {
				if _, err := fmt.Fprint(w, `[{"id":12,"node_id":"issue-comment-node-12","body":"second issue comment","user":{"login":"carol"},"updated_at":"2026-10-09T08:00:02Z"}]`); err != nil {
					t.Error(err)
				}
			}
		case "/repos/o/r/pulls/7/comments":
			if _, err := fmt.Fprint(w, `[{"id":13,"node_id":"review-comment-node-13","body":"diff comment","user":{"login":"dave"},"updated_at":"2026-10-09T08:00:03Z"}]`); err != nil {
				t.Error(err)
			}
		case "/graphql":
			var input struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if !strings.Contains(input.Query, "itemTypes: [PULL_REQUEST_REVIEW]") ||
				!strings.Contains(input.Query, "since: $since") || input.Variables["owner"] != "o" || input.Variables["name"] != "r" {
				t.Errorf("review timeline query omitted filtering: %+v", input)
			}
			if got := input.Variables["since"]; got != nil && got != "2026-10-09T07:59:59Z" {
				t.Errorf("review timeline since = %v", got)
			}
			if _, err := fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"id":"review-node-14","fullDatabaseId":"14","body":"review body","author":{"login":"erin"},"state":"COMMENTED","submittedAt":"2026-10-09T08:00:04Z"},{"id":"review-node-15","fullDatabaseId":"15","body":"unsubmitted","state":"PENDING","submittedAt":null}],"pageInfo":{"hasNextPage":false}}}}}}`); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	since := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	items, err := c.ThreadItems(context.Background(), "o/r", "PullRequest", 7, since)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	want := []string{"o/r#issue-comment:11", "o/r#issue-comment:12", "o/r#review-comment:13", "o/r#review:14"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("wrong surfaces or order: got %v, want %v", keys, want)
	}
	if items[0].Author.Login != "bob" || items[3].Author.Login != "erin" {
		t.Fatalf("wrong authors: %+v", items)
	}
	for i, want := range []string{"issue-comment-node-11", "issue-comment-node-12", "review-comment-node-13", "review-node-14"} {
		if items[i].NodeID != want {
			t.Fatalf("item %d missing reaction node ID: %+v", i, items[i])
		}
	}
	pull, err := c.PullRequestDetails(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if pull.Base.Ref != "main" || pull.Head.SHA != "abc" || pull.Title != "PR title" {
		t.Fatalf("missing pull request details: %+v", pull)
	}
	items, err = c.ThreadItems(context.Background(), "o/r", "PullRequest", 7, time.Time{})
	if err != nil || len(items) != 5 || items[0].NodeID != "pull-node-7" {
		t.Fatalf("pull request body missing reaction node ID: %+v, %v", items, err)
	}
}

func TestHasVerifiedBotReplyUsesCommandTimeAndStopsAtMatch(t *testing.T) {
	since := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	marker := "<!-- linkdataminion:job -->"
	var pages, provenance int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/7/comments":
			pages++
			if r.URL.Query().Get("since") != "2026-10-09T07:59:59Z" || r.URL.Query().Get("page") != "1" {
				t.Errorf("precheck did not start at command time: %s", r.URL.RawQuery)
			}
			w.Header().Set("Link", `<https://api.github.com/repos/o/r/issues/7/comments?page=2>; rel="next"`)
			_, _ = fmt.Fprintf(w, `[{"id":1,"body":"old","user":{"login":"linkdataminion"},"updated_at":"2026-10-09T07:59:58Z"},{"id":2,"node_id":"reply-node","body":%q,"user":{"login":"linkdataminion"},"updated_at":"2026-10-09T08:00:01Z"}]`, "done\n\n"+marker)
		case "/graphql":
			provenance++
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"login":"linkdataminion"}}}}`, "done\n\n"+marker)
		default:
			t.Errorf("unexpected precheck request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testClient(server)
	found, err := client.HasVerifiedBotReply(context.Background(), "o/r", 7, since, "linkdataminion", marker, false)
	if err != nil || !found || pages != 1 || provenance != 1 {
		t.Fatalf("reply lookup: found=%t err=%v pages=%d provenance=%d", found, err, pages, provenance)
	}
}

func TestReviewTimelineFiltersOldAndPaginatesSubmittedReviews(t *testing.T) {
	since := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected review request: %s %s", r.Method, r.URL)
		}
		var input struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(input.Query, "itemTypes: [PULL_REQUEST_REVIEW]") || input.Variables["since"] != "2026-10-09T07:59:59Z" {
			t.Errorf("missing incremental review filter: %+v", input)
		}
		switch calls {
		case 1:
			if input.Variables["after"] != "" {
				t.Errorf("first page cursor = %v", input.Variables["after"])
			}
			_, _ = fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"id":"old","fullDatabaseId":"1","state":"COMMENTED","submittedAt":"2026-10-09T07:00:00Z"},{"id":"pending","fullDatabaseId":"2","state":"PENDING"},{"id":"submitted","fullDatabaseId":"3","body":"new command","author":{"login":"alice","id":7},"state":"COMMENTED","submittedAt":"2026-10-09T08:01:00Z"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}}`)
		case 2:
			if input.Variables["after"] != "next" {
				t.Errorf("second page cursor = %v", input.Variables["after"])
			}
			_, _ = fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"id":"later","fullDatabaseId":"4","body":"later command","author":{"login":"bob","id":8},"state":"APPROVED","submittedAt":"2026-10-09T08:02:00Z"}],"pageInfo":{"hasNextPage":false}}}}}}`)
		default:
			t.Errorf("unexpected review page %d", calls)
		}
	}))
	defer server.Close()
	var items []Item
	err := testClient(server).forEachReview(context.Background(), "o/r", 7, since, func(item Item) (bool, error) {
		items = append(items, item)
		return false, nil
	})
	if err != nil || calls != 2 || len(items) != 2 || items[0].Key != "o/r#review:3" || items[1].Key != "o/r#review:4" || items[0].Author.ID != 7 {
		t.Fatalf("filtered reviews: items=%+v calls=%d err=%v", items, calls, err)
	}
}

func TestReviewTimelineCapsRecentPages(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":%q}}}}}}`, fmt.Sprint(calls))
	}))
	defer server.Close()
	err := testClient(server).forEachReview(context.Background(), "o/r", 7, time.Time{}, func(Item) (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "too many recent") || calls != 20 {
		t.Fatalf("review page cap: calls=%d err=%v", calls, err)
	}
}

func TestReviewTimelineRejectsIncompleteResponse(t *testing.T) {
	for _, response := range []string{
		`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[]}}}}}}`,
		`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}},"errors":[{"message":"partial failure"}]}`,
		`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[null],"pageInfo":{"hasNextPage":false}}}}}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, response)
		}))
		err := testClient(server).forEachReview(context.Background(), "o/r", 7, time.Time{}, func(Item) (bool, error) { return false, nil })
		server.Close()
		if err == nil {
			t.Fatalf("incomplete review timeline accepted: %s", response)
		}
	}
}

func TestHasVerifiedBotReplyStopsAtReviewMatch(t *testing.T) {
	marker := "<!-- linkdataminion:job -->"
	var timelineCalls, provenanceCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case "/graphql":
			var input struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(input.Query, "timelineItems") {
				timelineCalls++
				_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"id":"bot-review","fullDatabaseId":"5","body":%q,"author":{"login":"linkdataminion"},"state":"COMMENTED","submittedAt":"2026-10-09T08:01:00Z"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}}`, "done\n\n"+marker)
				return
			}
			provenanceCalls++
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"login":"linkdataminion"}}}}`, "done\n\n"+marker)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	since := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	found, err := testClient(server).HasVerifiedBotReply(context.Background(), "o/r", 7, since, "linkdataminion", marker, true)
	if err != nil || !found || timelineCalls != 1 || provenanceCalls != 1 {
		t.Fatalf("review precheck: found=%t err=%v timeline=%d provenance=%d", found, err, timelineCalls, provenanceCalls)
	}
}

func TestBranchLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/o/r/branches/minion%2Ffix-2":
			if _, err := fmt.Fprint(w, `{"name":"minion/fix-2","commit":{"sha":"abc123"}}`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/branches/missing":
			w.WriteHeader(http.StatusNotFound)
			if _, err := fmt.Fprint(w, `{"message":"Not Found"}`); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	branch, err := c.Branch(context.Background(), "o/r", "minion/fix-2")
	if err != nil || branch.Name != "minion/fix-2" || branch.Commit.SHA != "abc123" {
		t.Fatalf("branch lookup: %+v, %v", branch, err)
	}
	_, err = c.Branch(context.Background(), "o/r", "missing")
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
		t.Fatalf("missing branch should return typed 404, got %v", err)
	}
	if _, err := c.Branch(context.Background(), "o/r", "bad*"); err == nil {
		t.Fatal("branch lookup must reject wildcard names")
	}
}

func TestIssueBodyDoesNotReappearWhenOnlyCommentsChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/2":
			if _, err := fmt.Fprint(w, `{"number":2,"node_id":"issue-node-2","title":"Issue title","body":"old command","user":{"login":"alice"},"created_at":"2026-10-08T08:00:00Z","updated_at":"2026-10-09T08:00:00Z","html_url":"https://github.com/o/r/issues/2"}`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/issues/2/comments":
			if _, err := fmt.Fprint(w, `[]`); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	issue, err := c.IssueDetails(context.Background(), "o/r", 2)
	if err != nil || issue.Title != "Issue title" {
		t.Fatalf("issue details: %+v, %v", issue, err)
	}
	items, err := c.ThreadItems(context.Background(), "o/r", "Issue", 2, time.Date(2026, 10, 9, 7, 0, 0, 0, time.UTC))
	if err != nil || len(items) != 0 {
		t.Fatalf("old body reappeared after unrelated update: %+v, %v", items, err)
	}
	items, err = c.ThreadItems(context.Background(), "o/r", "Issue", 2, time.Time{})
	if err != nil || len(items) != 1 || items[0].Body != "old command" || items[0].NodeID != "issue-node-2" {
		t.Fatalf("initial body missing: %+v, %v", items, err)
	}
}

func TestWritesUseSafeGitHubModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode write body: %v", err)
		}
		switch r.URL.Path {
		case "/repos/o/r/issues/2/comments":
			w.WriteHeader(http.StatusCreated)
			if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/pulls":
			if body["draft"] != true {
				t.Errorf("pull request not draft: %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			if _, err := fmt.Fprint(w, `{"number":3}`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/pulls/3/reviews":
			if body["event"] != "COMMENT" || body["commit_id"] != "abc123" {
				t.Errorf("review missing explicit commit or changes review state: %v", body)
			}
			if _, err := fmt.Fprint(w, `{"id":4}`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/releases":
			if body["draft"] != true || body["tag_name"] != "v1.0.0" || body["target_commitish"] != strings.Repeat("a", 40) {
				t.Errorf("release not draft or not pinned to commit: %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			if _, err := fmt.Fprint(w, `{"id":5}`); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	ctx := context.Background()
	if _, err := c.CreateIssueComment(ctx, "o/r", 2, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePullRequest(ctx, "o/r", "main", "feature", "Fix", "body", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePullRequestReview(ctx, "o/r", 3, "review notes", "abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePullRequestReview(ctx, "o/r", 3, "review notes", ""); err == nil {
		t.Fatal("review without explicit commit SHA must fail")
	}
	if _, err := c.CreateDraftRelease(ctx, "o/r", "v1.0.0", strings.Repeat("a", 40), "v1.0.0", "notes"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryLookups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls":
			if r.URL.Query().Get("head") != "o:bot-branch" || r.URL.Query().Get("base") != "main" || r.URL.Query().Get("state") != "open" {
				t.Errorf("wrong PR recovery filter: %s", r.URL.RawQuery)
			}
			if _, err := fmt.Fprint(w, `[{"number":3,"title":"recovered"}]`); err != nil {
				t.Error(err)
			}
		case "/repos/o/r/releases":
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("release lookup did not request 100 per page: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<https://api.github.com/repos/o/r/releases?page=2>; rel="next"`)
				if _, err := fmt.Fprint(w, `[{"id":8,"tag_name":"older","draft":false}]`); err != nil {
					t.Error(err)
				}
				return
			}
			if _, err := fmt.Fprint(w, `[{"id":9,"tag_name":"v1.0.0","draft":true}]`); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := testClient(server)
	pull, err := c.FindPullRequestByHead(context.Background(), "o/r", "main", "bot-branch")
	if err != nil || pull == nil || pull.Number != 3 {
		t.Fatalf("recover PR: %+v, %v", pull, err)
	}
	release, err := c.ReleaseByTag(context.Background(), "o/r", "v1.0.0")
	if err != nil || release.ID != 9 || !release.Draft {
		t.Fatalf("recover release: %+v, %v", release, err)
	}
	_, err = c.ReleaseByTag(context.Background(), "o/r", "missing")
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != 404 {
		t.Fatalf("missing release should return typed 404, got %v", err)
	}
}

func TestProtectedReleaseTagsRequiresVisibleCompleteRuleset(t *testing.T) {
	good := `{"id":42,"target":"tag","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["~ALL"],"exclude":[]}},"rules":[{"type":"update"},{"type":"deletion"}]}`
	for _, tt := range []struct {
		name   string
		list   string
		detail string
		status int
		want   bool
	}{
		{name: "complete ruleset", list: `[{"id":42}]`, detail: good, want: true},
		{name: "no rulesets", list: `[]`},
		{name: "hidden bypass list", list: `[{"id":42}]`, detail: strings.Replace(good, `"bypass_actors":[],`, "", 1)},
		{name: "null bypass list", list: `[{"id":42}]`, detail: strings.Replace(good, `"bypass_actors":[]`, `"bypass_actors":null`, 1)},
		{name: "bypass actor", list: `[{"id":42}]`, detail: strings.Replace(good, `"bypass_actors":[]`, `"bypass_actors":[{"actor_type":"RepositoryRole","actor_id":5}]`, 1)},
		{name: "branch target", list: `[{"id":42}]`, detail: strings.Replace(good, `"target":"tag"`, `"target":"branch"`, 1)},
		{name: "disabled", list: `[{"id":42}]`, detail: strings.Replace(good, `"enforcement":"active"`, `"enforcement":"disabled"`, 1)},
		{name: "partial coverage", list: `[{"id":42}]`, detail: strings.Replace(good, `"~ALL"`, `"refs/tags/v*"`, 1)},
		{name: "excluded tag", list: `[{"id":42}]`, detail: strings.Replace(good, `"exclude":[]`, `"exclude":["refs/tags/test"]`, 1)},
		{name: "no update rule", list: `[{"id":42}]`, detail: strings.Replace(good, `{"type":"update"},`, "", 1)},
		{name: "plan blocks rulesets", status: http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method: %s", r.Method)
				}
				switch r.URL.Path {
				case "/repos/o/r/rulesets":
					if r.URL.Query().Get("targets") != "tag" || r.URL.Query().Get("includes_parents") != "true" || r.URL.Query().Get("per_page") != "100" {
						t.Errorf("wrong ruleset query: %s", r.URL.RawQuery)
					}
					if tt.status != 0 {
						w.WriteHeader(tt.status)
						_, _ = fmt.Fprint(w, `{"message":"rulesets unavailable"}`)
						return
					}
					_, _ = fmt.Fprint(w, tt.list)
				case "/repos/o/r/rulesets/42":
					if r.URL.Query().Get("includes_parents") != "true" {
						t.Errorf("parent rulesets omitted: %s", r.URL.RawQuery)
					}
					_, _ = fmt.Fprint(w, tt.detail)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			protected, err := testClient(server).ProtectedReleaseTags(context.Background(), "o/r")
			if tt.status != 0 {
				var status *StatusError
				if !errors.As(err, &status) || status.StatusCode != tt.status {
					t.Fatalf("error = %v, want HTTP %d", err, tt.status)
				}
				return
			}
			if err != nil || protected != tt.want {
				t.Fatalf("protected = %v, error = %v, want %v", protected, err, tt.want)
			}
		})
	}
}

func TestProtectedReleaseTagsRejectsCreationConstraintsAfterIntegrityRule(t *testing.T) {
	for _, tt := range []struct {
		name   string
		detail string
		want   bool
	}{
		{"active creation rule", `{"id":43,"target":"tag","enforcement":"active","conditions":{"ref_name":{"include":["refs/tags/v*"]}},"rules":[{"type":"creation"}]}`, false},
		{"active tag name restriction", `{"id":43,"target":"tag","enforcement":"active","rules":[{"type":"tag_name_pattern","parameters":{"operator":"starts_with","pattern":"release-","negate":true}}]}`, false},
		{"allowed rule followed by tag restriction", `{"id":43,"target":"tag","enforcement":"active","rules":[{"type":"non_fast_forward"},{"type":"tag_name_pattern"}]}`, false},
		{"active commit metadata restriction", `{"id":43,"target":"tag","enforcement":"active","rules":[{"type":"commit_message_pattern"}]}`, false},
		{"unknown active tag rule", `{"id":43,"target":"tag","enforcement":"active","rules":[{"type":"future_rule"}]}`, false},
		{"non-fast-forward rule", `{"id":43,"target":"tag","enforcement":"active","rules":[{"type":"non_fast_forward"}]}`, true},
		{"disabled creation rule", `{"id":43,"target":"tag","enforcement":"disabled","rules":[{"type":"creation"}]}`, true},
		{"branch creation rule", `{"id":43,"target":"branch","enforcement":"active","rules":[{"type":"creation"}]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var details int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/o/r/rulesets":
					_, _ = fmt.Fprint(w, `[{"id":42},{"id":43}]`)
				case "/repos/o/r/rulesets/42":
					details++
					_, _ = fmt.Fprint(w, `{"id":42,"target":"tag","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["~ALL"],"exclude":[]}},"rules":[{"type":"update"},{"type":"deletion"}]}`)
				case "/repos/o/r/rulesets/43":
					details++
					_, _ = fmt.Fprint(w, tt.detail)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			protected, err := testClient(server).ProtectedReleaseTags(context.Background(), "o/r")
			if err != nil || protected != tt.want || details != 2 {
				t.Fatalf("protected = %v, error = %v, details = %d; want %v, nil, 2", protected, err, details, tt.want)
			}
		})
	}
}
