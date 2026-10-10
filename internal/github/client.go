// Package github contains the small part of GitHub's API used by the bot.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Client calls GitHub with a user token. BaseURL and HTTPClient may be changed
// before the first request, which also makes the client usable with httptest.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	token      string
}

func New(token string) *Client {
	return &Client{
		BaseURL:    defaultBaseURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		token:      token,
	}
}

type User struct {
	ID     int64  `json:"id"`
	Login  string `json:"login"`
	NodeID string `json:"node_id"`
	Type   string `json:"type"`
}

type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	Owner         User   `json:"owner"`
}

type Subject struct {
	Title            string `json:"title"`
	URL              string `json:"url"`
	LatestCommentURL string `json:"latest_comment_url"`
	Type             string `json:"type"`
}

type Notification struct {
	ID         string     `json:"id"`
	Repository Repository `json:"repository"`
	Subject    Subject    `json:"subject"`
	Reason     string     `json:"reason"`
	Unread     bool       `json:"unread"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastReadAt *time.Time `json:"last_read_at"`
}

type Permission struct {
	Permission string `json:"permission"`
	RoleName   string `json:"role_name"`
	User       User   `json:"user"`
}

// Invitation is an open invitation addressed to the authenticated user.
// Permissions is GitHub's proposed repository role (for example, "write").
type Invitation struct {
	ID          int64      `json:"id"`
	Repository  Repository `json:"repository"`
	Invitee     User       `json:"invitee"`
	Inviter     User       `json:"inviter"`
	Permissions string     `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	Expired     bool       `json:"expired"`
	HTMLURL     string     `json:"html_url"`
}

type Comment struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
}

type PullRequestRef struct {
	Ref  string      `json:"ref"`
	SHA  string      `json:"sha"`
	Repo *Repository `json:"repo"`
}

type PullRequest struct {
	ID        int64          `json:"id"`
	NodeID    string         `json:"node_id"`
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	User      User           `json:"user"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	HTMLURL   string         `json:"html_url"`
	Head      PullRequestRef `json:"head"`
	Base      PullRequestRef `json:"base"`
	Draft     bool           `json:"draft"`
	State     string         `json:"state"`
}

type BranchCommit struct {
	SHA string `json:"sha"`
}

type Branch struct {
	Name   string       `json:"name"`
	Commit BranchCommit `json:"commit"`
}

type GitReference struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

type repositoryRuleset struct {
	ID           int64              `json:"id"`
	Target       string             `json:"target"`
	Enforcement  string             `json:"enforcement"`
	BypassActors *[]json.RawMessage `json:"bypass_actors"`
	Conditions   struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type string `json:"type"`
	} `json:"rules"`
}

type Review struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"node_id"`
	Body        string    `json:"body"`
	User        User      `json:"user"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
	HTMLURL     string    `json:"html_url"`
}

type Release struct {
	ID              int64  `json:"id"`
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Name            string `json:"name"`
	Body            string `json:"body"`
	Draft           bool   `json:"draft"`
	HTMLURL         string `json:"html_url"`
	Author          User   `json:"author"`
}

// StatusError preserves the HTTP status so callers can distinguish missing
// permissions from a missing repository without inspecting error text.
type StatusError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub %s %s: HTTP %d", e.Method, e.Path, e.StatusCode)
	}
	return fmt.Sprintf("GitHub %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func (c *Client) request(ctx context.Context, method, path string, input, output any, extra http.Header, expected ...int) (http.Header, int, error) {
	if c == nil {
		return nil, 0, errors.New("nil GitHub client")
	}
	if c.token == "" {
		return nil, 0, errors.New("GitHub token is empty")
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, 0, fmt.Errorf("encode GitHub request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/"+strings.TrimLeft(path, "/"), body)
	if err != nil {
		return nil, 0, fmt.Errorf("build GitHub request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range extra {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	// An old repository path can redirect to a renamed or transferred repo.
	// Never let an authenticated write follow that redirect outside the whitelist.
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // -- Closing the body releases resources without changing the HTTP result.
	for _, code := range expected {
		if resp.StatusCode == code {
			if output != nil && resp.StatusCode != http.StatusNotModified {
				if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
					return resp.Header, resp.StatusCode, fmt.Errorf("decode GitHub %s %s response: %w", method, path, err)
				}
			}
			return resp.Header, resp.StatusCode, nil
		}
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var ghErr struct {
		Message string `json:"message"`
	}
	message := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &ghErr) == nil && ghErr.Message != "" {
		message = ghErr.Message
	}
	return resp.Header, resp.StatusCode, &StatusError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: message}
}

// ReactEyes acknowledges the exact GitHub item containing a command. GraphQL
// covers pull request review bodies as well as the REST reaction surfaces.
func (c *Client) ReactEyes(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return errors.New("GitHub item has no node ID")
	}
	var result struct {
		Data struct {
			AddReaction struct {
				Subject struct {
					Reactions struct {
						ViewerHasReacted bool `json:"viewerHasReacted"`
					} `json:"reactions"`
				} `json:"subject"`
			} `json:"addReaction"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_, _, err := c.request(ctx, http.MethodPost, "/graphql", map[string]any{
		"query":     `mutation($id: ID!) { addReaction(input: {subjectId: $id, content: EYES}) { subject { reactions(content: EYES, first: 1) { viewerHasReacted } } } }`,
		"variables": map[string]string{"id": nodeID},
	}, &result, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if len(result.Errors) != 0 {
		return fmt.Errorf("GitHub addReaction: %s", result.Errors[0].Message)
	}
	if !result.Data.AddReaction.Subject.Reactions.ViewerHasReacted {
		return errors.New("GitHub did not confirm eyes reaction")
	}
	return nil
}

func repoPath(repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validSegment(parts[0]) || !validSegment(parts[1]) {
		return "", fmt.Errorf("invalid repository %q: want owner/name", repo)
	}
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/?# \\")
}

func withQuery(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

func hasNextPage(link string) bool {
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			return true
		}
	}
	return false
}

func pollInterval(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(header.Get("X-Poll-Interval"))
	if err != nil || seconds <= 0 {
		return time.Minute
	}
	return time.Duration(seconds) * time.Second
}

// ListNotifications includes read notifications, so another inbox client
// marking a thread read does not remove it from the bot's scan. It returns the
// server's minimum poll interval. The durable cursor makes conditional requests
// unsafe: a later action may fail and need the same notification again.
func (c *Client) ListNotifications(ctx context.Context, since time.Time) ([]Notification, time.Duration, error) {
	query := url.Values{"all": {"true"}, "participating": {"true"}, "per_page": {"50"}}
	if !since.IsZero() {
		query.Set("since", since.Add(-time.Second).UTC().Format(time.RFC3339))
	}
	query.Set("page", "1")
	var all []Notification
	var interval = time.Minute
	for page := 1; ; page++ {
		if page > 1000 {
			return nil, interval, errors.New("too many GitHub notification pages")
		}
		query.Set("page", strconv.Itoa(page))
		var batch []Notification
		header, _, err := c.request(ctx, http.MethodGet, withQuery("/notifications", query), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return nil, interval, err
		}
		if page == 1 {
			interval = pollInterval(header)
		}
		all = append(all, batch...)
		if !hasNextPage(header.Get("Link")) {
			break
		}
	}
	return all, interval, nil
}

func (c *Client) AuthenticatedUser(ctx context.Context) (User, error) {
	var user User
	_, _, err := c.request(ctx, http.MethodGet, "/user", nil, &user, nil, http.StatusOK)
	return user, err
}

// ListRepositoryInvitations lists all currently open invitations for the
// authenticated user. The caller decides whether an invitation is allowed.
func (c *Client) ListRepositoryInvitations(ctx context.Context) ([]Invitation, error) {
	q := url.Values{"per_page": {"100"}}
	var invitations []Invitation
	for page := 1; ; page++ {
		if page > 1000 {
			return nil, errors.New("too many GitHub repository invitation pages")
		}
		q.Set("page", strconv.Itoa(page))
		var batch []Invitation
		header, _, err := c.request(ctx, http.MethodGet, withQuery("/user/repository_invitations", q), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		invitations = append(invitations, batch...)
		if !hasNextPage(header.Get("Link")) {
			return invitations, nil
		}
	}
}

// AcceptRepositoryInvitation accepts one invitation for the authenticated user.
func (c *Client) AcceptRepositoryInvitation(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("invalid repository invitation id %d", id)
	}
	_, _, err := c.request(ctx, http.MethodPatch, "/user/repository_invitations/"+strconv.FormatInt(id, 10), nil, nil, nil, http.StatusNoContent)
	return err
}

func (c *Client) Repository(ctx context.Context, repo string) (Repository, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Repository{}, err
	}
	var details Repository
	_, _, err = c.request(ctx, http.MethodGet, path, nil, &details, nil, http.StatusOK)
	if err == nil && !strings.EqualFold(details.FullName, repo) {
		return Repository{}, fmt.Errorf("repository identity changed: requested %s, got %s", repo, details.FullName)
	}
	return details, err
}

func (c *Client) Permission(ctx context.Context, repo, login string) (Permission, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Permission{}, err
	}
	if !validSegment(login) {
		return Permission{}, fmt.Errorf("invalid GitHub login %q", login)
	}
	var permission Permission
	_, _, err = c.request(ctx, http.MethodGet, path+"/collaborators/"+url.PathEscape(login)+"/permission", nil, &permission, nil, http.StatusOK)
	return permission, err
}

func (c *Client) PullRequestDetails(ctx context.Context, repo string, number int) (PullRequest, error) {
	path, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	if number <= 0 {
		return PullRequest{}, fmt.Errorf("invalid pull request number %d", number)
	}
	var pull PullRequest
	_, _, err = c.request(ctx, http.MethodGet, path+"/pulls/"+strconv.Itoa(number), nil, &pull, nil, http.StatusOK)
	return pull, err
}

func (c *Client) Branch(ctx context.Context, repo, branch string) (Branch, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Branch{}, err
	}
	if strings.TrimSpace(branch) == "" || strings.ContainsAny(branch, "*?") {
		return Branch{}, fmt.Errorf("invalid branch name %q", branch)
	}
	var details Branch
	_, _, err = c.request(ctx, http.MethodGet, path+"/branches/"+url.PathEscape(branch), nil, &details, nil, http.StatusOK)
	return details, err
}

func (c *Client) TagReference(ctx context.Context, repo, tag string) (GitReference, error) {
	path, err := repoPath(repo)
	if err != nil {
		return GitReference{}, err
	}
	if strings.TrimSpace(tag) == "" {
		return GitReference{}, errors.New("tag is required")
	}
	var ref GitReference
	_, _, err = c.request(ctx, http.MethodGet, path+"/git/ref/tags/"+url.PathEscape(tag), nil, &ref, nil, http.StatusOK)
	return ref, err
}

func (c *Client) CreateTagReference(ctx context.Context, repo, tag, sha string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	if strings.TrimSpace(tag) == "" || strings.TrimSpace(sha) == "" {
		return errors.New("tag and SHA are required")
	}
	_, _, err = c.request(ctx, http.MethodPost, path+"/git/refs", map[string]string{
		"ref": "refs/tags/" + tag,
		"sha": sha,
	}, nil, nil, http.StatusCreated)
	return err
}

// ProtectedReleaseTags verifies an active all-tag integrity ruleset and refuses
// release creation while any active tag ruleset restricts creation. The latter
// is conservative: the bot must not create a tag by bypassing a restriction
// that applies to the requester. GitHub omits bypass_actors for tokens without
// ruleset write access; omission is not proof that no actor can bypass.
func (c *Client) ProtectedReleaseTags(ctx context.Context, repo string) (bool, error) {
	path, err := repoPath(repo)
	if err != nil {
		return false, err
	}
	q := url.Values{"per_page": {"100"}, "targets": {"tag"}, "includes_parents": {"true"}}
	protected := false
	for page := 1; page <= 1000; page++ {
		q.Set("page", strconv.Itoa(page))
		var summaries []struct {
			ID int64 `json:"id"`
		}
		header, _, err := c.request(ctx, http.MethodGet, withQuery(path+"/rulesets", q), nil, &summaries, nil, http.StatusOK)
		if err != nil {
			return false, err
		}
		for _, summary := range summaries {
			if summary.ID <= 0 {
				return false, errors.New("GitHub returned a ruleset without a valid ID")
			}
			var detail repositoryRuleset
			_, _, err := c.request(ctx, http.MethodGet, path+"/rulesets/"+strconv.FormatInt(summary.ID, 10)+"?includes_parents=true", nil, &detail, nil, http.StatusOK)
			if err != nil {
				return false, err
			}
			if detail.ID != summary.ID {
				return false, errors.New("GitHub returned mismatched ruleset details")
			}
			if detail.Target != "tag" || detail.Enforcement != "active" {
				continue
			}
			var updates, deletions bool
			for _, rule := range detail.Rules {
				if rule.Type == "creation" {
					return false, nil
				}
				updates = updates || rule.Type == "update"
				deletions = deletions || rule.Type == "deletion"
			}
			protected = protected || updates && deletions && detail.BypassActors != nil && len(*detail.BypassActors) == 0 &&
				len(detail.Conditions.RefName.Exclude) == 0 && containsString(detail.Conditions.RefName.Include, "~ALL")
		}
		if !hasNextPage(header.Get("Link")) {
			return protected, nil
		}
	}
	return false, errors.New("too many GitHub ruleset pages")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (c *Client) IssueDetails(ctx context.Context, repo string, number int) (Issue, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Issue{}, err
	}
	if number <= 0 {
		return Issue{}, fmt.Errorf("invalid issue number %d", number)
	}
	var issue Issue
	_, _, err = c.request(ctx, http.MethodGet, path+"/issues/"+strconv.Itoa(number), nil, &issue, nil, http.StatusOK)
	return issue, err
}

// FindPullRequestByHead finds the open draft candidate for one base and head.
// A bare head branch is qualified with the repository owner.
func (c *Client) FindPullRequestByHead(ctx context.Context, repo, base, head string) (*PullRequest, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(base) == "" || strings.TrimSpace(head) == "" {
		return nil, errors.New("pull request base and head are required")
	}
	if !strings.Contains(head, ":") {
		head = strings.Split(repo, "/")[0] + ":" + head
	}
	q := url.Values{"base": {base}, "head": {head}, "state": {"open"}, "per_page": {"100"}}
	var pulls []PullRequest
	_, _, err = c.request(ctx, http.MethodGet, withQuery(path+"/pulls", q), nil, &pulls, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	if len(pulls) != 1 {
		return nil, fmt.Errorf("multiple open pull requests for %s to %s", head, base)
	}
	return &pulls[0], nil
}

func (c *Client) CreateIssueComment(ctx context.Context, repo string, number int, body string) (Comment, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Comment{}, err
	}
	if number <= 0 || strings.TrimSpace(body) == "" {
		return Comment{}, errors.New("issue number and comment body are required")
	}
	var comment Comment
	_, _, err = c.request(ctx, http.MethodPost, path+"/issues/"+strconv.Itoa(number)+"/comments", map[string]string{"body": body}, &comment, nil, http.StatusCreated)
	return comment, err
}

func (c *Client) CreatePullRequest(ctx context.Context, repo, base, head, title, body string, draft bool) (PullRequest, error) {
	path, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	if base == "" || head == "" || strings.TrimSpace(title) == "" {
		return PullRequest{}, errors.New("pull request base, head, and title are required")
	}
	input := struct {
		Base  string `json:"base"`
		Head  string `json:"head"`
		Title string `json:"title"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}{base, head, title, body, draft}
	var pull PullRequest
	_, _, err = c.request(ctx, http.MethodPost, path+"/pulls", input, &pull, nil, http.StatusCreated)
	return pull, err
}

// CreatePullRequestReview submits a comment review. It never approves or
// requests changes on behalf of the bot.
func (c *Client) CreatePullRequestReview(ctx context.Context, repo string, number int, body, commitSHA string) (Review, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Review{}, err
	}
	if number <= 0 || strings.TrimSpace(body) == "" || strings.TrimSpace(commitSHA) == "" {
		return Review{}, errors.New("pull request number, review body, and commit SHA are required")
	}
	var review Review
	_, _, err = c.request(ctx, http.MethodPost, path+"/pulls/"+strconv.Itoa(number)+"/reviews", map[string]string{"body": body, "commit_id": commitSHA, "event": "COMMENT"}, &review, nil, http.StatusOK)
	return review, err
}

func (c *Client) CreateDraftRelease(ctx context.Context, repo, tag, target, name, body string) (Release, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Release{}, err
	}
	if strings.TrimSpace(tag) == "" {
		return Release{}, errors.New("release tag is required")
	}
	input := struct {
		TagName         string `json:"tag_name"`
		TargetCommitish string `json:"target_commitish,omitempty"`
		Name            string `json:"name"`
		Body            string `json:"body"`
		Draft           bool   `json:"draft"`
	}{tag, target, name, body, true}
	var release Release
	_, _, err = c.request(ctx, http.MethodPost, path+"/releases", input, &release, nil, http.StatusCreated)
	return release, err
}

// ReleaseByTag scans releases because GitHub's get-by-tag endpoint returns
// published releases only; draft releases appear in the list to users with
// push access. This lets callers recover a previously created draft.
func (c *Client) ReleaseByTag(ctx context.Context, repo, tag string) (Release, error) {
	path, err := repoPath(repo)
	if err != nil {
		return Release{}, err
	}
	if strings.TrimSpace(tag) == "" {
		return Release{}, errors.New("release tag is required")
	}
	q := url.Values{"per_page": {"100"}}
	for page := 1; ; page++ {
		if page > 1000 {
			return Release{}, errors.New("too many GitHub release pages")
		}
		q.Set("page", strconv.Itoa(page))
		var batch []Release
		header, _, err := c.request(ctx, http.MethodGet, withQuery(path+"/releases", q), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return Release{}, err
		}
		for _, release := range batch {
			if release.TagName == tag {
				return release, nil
			}
		}
		if !hasNextPage(header.Get("Link")) {
			return Release{}, &StatusError{Method: http.MethodGet, Path: path + "/releases", StatusCode: http.StatusNotFound, Message: "release tag not found"}
		}
	}
}
