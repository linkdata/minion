// Package github contains the small part of GitHub's API used by the bot.
package github

import (
	"bytes"
	"context"
	"encoding/hex"
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

const maxTreeResponseBytes = 8 << 20

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
	ID            int64       `json:"id"`
	Name          string      `json:"name"`
	FullName      string      `json:"full_name"`
	DefaultBranch string      `json:"default_branch"`
	HTMLURL       string      `json:"html_url"`
	Private       bool        `json:"private"`
	Visibility    string      `json:"visibility"`
	Archived      bool        `json:"archived"`
	Fork          bool        `json:"fork"`
	Parent        *Repository `json:"parent"`
	Source        *Repository `json:"source"`
	Owner         User        `json:"owner"`
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

// GitTree is GitHub's recursive listing of a content-addressed tree.
type GitTree struct {
	SHA       string
	Truncated *bool
	Tree      []GitTreeEntry
}

type GitTreeEntry struct {
	Path string
	Type string
	Mode string
	Size *int64
}

type repositoryRuleset struct {
	ID           int64              `json:"id"`
	Target       string             `json:"target"`
	Enforcement  string             `json:"enforcement"`
	BypassActors *[]json.RawMessage `json:"bypass_actors"`
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
	return c.requestBounded(ctx, method, path, input, output, extra, 0, expected...)
}

func (c *Client) requestBounded(ctx context.Context, method, path string, input, output any, extra http.Header, maxBytes int64, expected ...int) (http.Header, int, error) {
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
				if maxBytes > 0 {
					data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
					if err != nil {
						return resp.Header, resp.StatusCode, fmt.Errorf("read GitHub %s %s response: %w", method, path, err)
					}
					if int64(len(data)) > maxBytes {
						return resp.Header, resp.StatusCode, fmt.Errorf("GitHub %s %s response exceeds %d bytes", method, path, maxBytes)
					}
					if err := json.Unmarshal(data, output); err != nil {
						return resp.Header, resp.StatusCode, fmt.Errorf("decode GitHub %s %s response: %w", method, path, err)
					}
				} else if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
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

// GitTree returns bounded size metadata without downloading blob contents.
func (c *Client) GitTree(ctx context.Context, repo, treeSHA string) (GitTree, error) {
	var tree GitTree
	path, err := repoPath(repo)
	if err != nil {
		return tree, err
	}
	if len(treeSHA) != 40 {
		return tree, errors.New("invalid Git tree SHA")
	}
	if _, err := hex.DecodeString(treeSHA); err != nil {
		return tree, errors.New("invalid Git tree SHA")
	}
	_, _, err = c.requestBounded(ctx, http.MethodGet, path+"/git/trees/"+treeSHA+"?recursive=1", nil, &tree, nil, maxTreeResponseBytes, http.StatusOK)
	return tree, err
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

// ListRepositoryNotifications lists a repository's read and unread notifications.
//
// It returns GitHub's minimum poll interval and discards partial results if a
// page fails. Marking a thread read in another client does not hide it.
func (c *Client) ListRepositoryNotifications(ctx context.Context, repo string, since time.Time) ([]Notification, time.Duration, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, 0, err
	}
	query := url.Values{"all": {"true"}, "participating": {"true"}, "per_page": {"100"}}
	if !since.IsZero() {
		query.Set("since", since.Add(-time.Second).UTC().Format(time.RFC3339))
	}
	query.Set("page", "1")
	var all []Notification
	interval := time.Minute
	for page := 1; ; page++ {
		if page > 20 {
			return nil, interval, fmt.Errorf("too many GitHub notification pages for %s", repo)
		}
		query.Set("page", strconv.Itoa(page))
		var batch []Notification
		header, _, err := c.request(ctx, http.MethodGet, withQuery(path+"/notifications", query), nil, &batch, nil, http.StatusOK)
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

// ListRepositoryInvitations lists open invitations for the authenticated user.
//
// It reads at most ten pages and returns fetched invitations if a later page fails.
func (c *Client) ListRepositoryInvitations(ctx context.Context) ([]Invitation, error) {
	q := url.Values{"per_page": {"100"}}
	var invitations []Invitation
	for page := 1; ; page++ {
		if page > 10 {
			return invitations, errors.New("more than ten GitHub repository invitation pages")
		}
		q.Set("page", strconv.Itoa(page))
		var batch []Invitation
		header, _, err := c.request(ctx, http.MethodGet, withQuery("/user/repository_invitations", q), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return invitations, err
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

// DeclineRepositoryInvitation removes an invitation from the authenticated account.
func (c *Client) DeclineRepositoryInvitation(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("invalid repository invitation id %d", id)
	}
	_, _, err := c.request(ctx, http.MethodDelete, "/user/repository_invitations/"+strconv.FormatInt(id, 10), nil, nil, nil, http.StatusNoContent)
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

// ActionsEnabled reports whether a repository can run GitHub Actions.
func (c *Client) ActionsEnabled(ctx context.Context, repo string) (bool, error) {
	path, err := repoPath(repo)
	if err != nil {
		return false, err
	}
	var policy struct {
		Enabled *bool `json:"enabled"`
	}
	_, _, err = c.request(ctx, http.MethodGet, path+"/actions/permissions", nil, &policy, nil, http.StatusOK)
	if err != nil {
		return false, err
	}
	if policy.Enabled == nil {
		return false, errors.New("GitHub Actions policy omitted enabled state")
	}
	return *policy.Enabled, nil
}

// SafePrivateForkWorkflows refuses private fork PRs with base secrets or write tokens.
func (c *Client) SafePrivateForkWorkflows(ctx context.Context, repo string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	var policy struct {
		SendWriteTokens *bool `json:"send_write_tokens_to_workflows"`
		SendSecrets     *bool `json:"send_secrets_and_variables"`
	}
	_, _, err = c.request(ctx, http.MethodGet, path+"/actions/permissions/fork-pr-workflows-private-repos", nil, &policy, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if policy.SendWriteTokens == nil || policy.SendSecrets == nil {
		return errors.New("private fork PR workflow policy is incomplete")
	}
	if *policy.SendWriteTokens || *policy.SendSecrets {
		return errors.New("private fork PR workflows receive secrets or write tokens")
	}
	return nil
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

// SafeFixPushRules checks effective push rules for bot publication.
//
// It refuses active rules with bypass actors or unreadable bypass lists, and incomplete responses.
func (c *Client) SafeFixPushRules(ctx context.Context, repo string) error {
	path, err := repoPath(repo)
	if err != nil {
		return err
	}
	q := url.Values{"per_page": {"100"}, "page": {"1"}, "targets": {"push"}, "includes_parents": {"true"}}
	var summaries []struct {
		ID int64 `json:"id"`
	}
	header, _, err := c.request(ctx, http.MethodGet, withQuery(path+"/rulesets", q), nil, &summaries, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if summaries == nil || header.Get("Link") != "" || len(summaries) >= 100 {
		return errors.New("GitHub push ruleset list is incomplete")
	}
	for _, summary := range summaries {
		if summary.ID <= 0 {
			return errors.New("GitHub returned a push ruleset without a valid ID")
		}
		var detail repositoryRuleset
		_, _, err := c.request(ctx, http.MethodGet, path+"/rulesets/"+strconv.FormatInt(summary.ID, 10)+"?includes_parents=true", nil, &detail, nil, http.StatusOK)
		if err != nil {
			return err
		}
		if detail.ID != summary.ID || detail.Target != "push" {
			return errors.New("GitHub returned mismatched push ruleset details")
		}
		switch detail.Enforcement {
		case "disabled", "evaluate":
			continue
		case "active":
			if detail.BypassActors == nil {
				return fmt.Errorf("active push ruleset %d has an unreadable bypass list", summary.ID)
			}
			if len(*detail.BypassActors) != 0 {
				return fmt.Errorf("active push ruleset %d can be bypassed", summary.ID)
			}
		default:
			return fmt.Errorf("push ruleset %d has unknown enforcement %q", summary.ID, detail.Enforcement)
		}
	}
	return nil
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
