package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Item is one authored piece of content that may contain a bot command. Key
// identifies the source object and stays stable if its body is edited.
type Item struct {
	Key       string
	NodeID    string
	Body      string
	Author    User
	Editor    User
	Edited    bool
	UpdatedAt time.Time
	HTMLURL   string
}

// CurrentItem reads the body and edit provenance together. REST comment
// responses identify the original author but not the person who edited them.
func (c *Client) CurrentItem(ctx context.Context, item Item) (Item, error) {
	if item.NodeID == "" {
		return Item{}, errors.New("GitHub item has no node ID")
	}
	var result struct {
		Data struct {
			Node *struct {
				Body         string     `json:"body"`
				Author       User       `json:"author"`
				Editor       User       `json:"editor"`
				LastEditedAt *time.Time `json:"lastEditedAt"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_, _, err := c.request(ctx, http.MethodPost, "/graphql", map[string]any{
		"query":     `query($id: ID!) { node(id: $id) { ... on Comment { body author { login ... on User { id: databaseId } ... on Bot { id: databaseId } } editor { login } lastEditedAt } } }`,
		"variables": map[string]string{"id": item.NodeID},
	}, &result, nil, http.StatusOK)
	if err != nil {
		return Item{}, err
	}
	if len(result.Errors) != 0 {
		return Item{}, fmt.Errorf("GitHub item provenance: %s", result.Errors[0].Message)
	}
	if result.Data.Node == nil || result.Data.Node.Author.Login == "" {
		return Item{}, errors.New("GitHub item provenance is unavailable")
	}
	item.Body = result.Data.Node.Body
	item.Author = result.Data.Node.Author
	item.Editor = result.Data.Node.Editor
	item.Edited = result.Data.Node.LastEditedAt != nil || item.Editor.Login != ""
	return item, nil
}

type Issue struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
}

// IssueContent reads the issue text and edit timestamp in one response. The
// rendered text excludes Markdown that the requester cannot see, such as HTML
// comments, unlike the raw REST body.
type IssueContent struct {
	Title        string
	BodyText     string
	LastEditedAt *time.Time
}

func (c *Client) CurrentIssueContent(ctx context.Context, nodeID string) (IssueContent, error) {
	if nodeID == "" {
		return IssueContent{}, errors.New("GitHub issue has no node ID")
	}
	var result struct {
		Data struct {
			Node *struct {
				Title        string     `json:"title"`
				BodyText     string     `json:"bodyText"`
				LastEditedAt *time.Time `json:"lastEditedAt"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_, _, err := c.request(ctx, http.MethodPost, "/graphql", map[string]any{
		"query":     `query($id: ID!) { node(id: $id) { ... on Issue { title bodyText lastEditedAt } } }`,
		"variables": map[string]string{"id": nodeID},
	}, &result, nil, http.StatusOK)
	if err != nil {
		return IssueContent{}, err
	}
	if len(result.Errors) != 0 {
		return IssueContent{}, fmt.Errorf("GitHub issue content: %s", result.Errors[0].Message)
	}
	if result.Data.Node == nil || result.Data.Node.Title == "" {
		return IssueContent{}, errors.New("GitHub issue content is unavailable")
	}
	return IssueContent{Title: result.Data.Node.Title, BodyText: result.Data.Node.BodyText, LastEditedAt: result.Data.Node.LastEditedAt}, nil
}

// ThreadItems fetches the thread's original body and every comment surface.
// Issue and pull request numbers are repository-local. Since is an inclusive
// lower bound with a one-second overlap to avoid timestamp precision gaps;
// callers should deduplicate returned items by Key and content/version. The
// original issue/PR body uses its creation timestamp, since REST updated_at
// also changes when unrelated comments arrive. Later body edits are not
// detected by this method.
func (c *Client) ThreadItems(ctx context.Context, repo, subjectType string, number int, since time.Time) ([]Item, error) {
	path, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("invalid thread number %d", number)
	}
	var items []Item
	switch subjectType {
	case "Issue":
		items, err = c.issueItems(ctx, repo, path, number, since)
	case "PullRequest":
		items, err = c.pullRequestItems(ctx, repo, path, number, since)
	default:
		return nil, fmt.Errorf("unsupported GitHub notification subject type %q", subjectType)
	}
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].Key < items[j].Key
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	return items, nil
}

func (c *Client) issueItems(ctx context.Context, repo, path string, number int, since time.Time) ([]Item, error) {
	source, err := c.IssueDetails(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	var items []Item
	updated := source.CreatedAt
	if eligible(updated, since) {
		items = append(items, Item{
			Key:       fmt.Sprintf("%s#issue:%d", strings.ToLower(repo), number),
			NodeID:    source.NodeID,
			Body:      source.Body,
			Author:    source.User,
			UpdatedAt: updated,
			HTMLURL:   source.HTMLURL,
		})
	}
	comments, err := c.listComments(ctx, path+"/issues/"+strconv.Itoa(number)+"/comments", since)
	if err != nil {
		return nil, err
	}
	for _, comment := range comments {
		items = append(items, commentItem(repo, "issue-comment", comment))
	}
	return items, nil
}

func (c *Client) pullRequestItems(ctx context.Context, repo, path string, number int, since time.Time) ([]Item, error) {
	pull, err := c.PullRequestDetails(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	var items []Item
	updated := pull.CreatedAt
	if eligible(updated, since) {
		items = append(items, Item{
			Key:       fmt.Sprintf("%s#pull:%d", strings.ToLower(repo), number),
			NodeID:    pull.NodeID,
			Body:      pull.Body,
			Author:    pull.User,
			UpdatedAt: updated,
			HTMLURL:   pull.HTMLURL,
		})
	}
	issueComments, err := c.listComments(ctx, path+"/issues/"+strconv.Itoa(number)+"/comments", since)
	if err != nil {
		return nil, err
	}
	for _, comment := range issueComments {
		items = append(items, commentItem(repo, "issue-comment", comment))
	}
	reviewComments, err := c.listComments(ctx, path+"/pulls/"+strconv.Itoa(number)+"/comments", since)
	if err != nil {
		return nil, err
	}
	for _, comment := range reviewComments {
		items = append(items, commentItem(repo, "review-comment", comment))
	}
	if err := c.forEachReview(ctx, repo, number, since, func(item Item) (bool, error) {
		items = append(items, item)
		return false, nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

func commentItem(repo, kind string, comment Comment) Item {
	return Item{
		Key:       fmt.Sprintf("%s#%s:%d", strings.ToLower(repo), kind, comment.ID),
		NodeID:    comment.NodeID,
		Body:      comment.Body,
		Author:    comment.User,
		UpdatedAt: nonzeroTime(comment.UpdatedAt, comment.CreatedAt),
		HTMLURL:   comment.HTMLURL,
	}
}

func nonzeroTime(primary, fallback time.Time) time.Time {
	if primary.IsZero() {
		return fallback
	}
	return primary
}

func eligible(updated, since time.Time) bool {
	return since.IsZero() || updated.IsZero() || !updated.Before(since.Add(-time.Second))
}

// HasVerifiedBotReply reports whether a verified bot reply exists.
//
// It scans issue comments and, when includeReviews is true, pull request
// reviews. The since bound applies to comments and filters reviews by
// submission time.
func (c *Client) HasVerifiedBotReply(ctx context.Context, repo string, number int, since time.Time, botID int64, marker string, includeReviews bool) (bool, error) {
	return c.hasVerifiedBotReply(ctx, repo, number, since, User{ID: botID}, marker, true, includeReviews)
}

// HasVerifiedBotPublication checks only the recorded comment or review surface.
func (c *Client) HasVerifiedBotPublication(ctx context.Context, repo string, number int, since time.Time, bot User, marker, kind string) (bool, error) {
	if kind != "comment" && kind != "review" {
		return false, fmt.Errorf("invalid publication kind %q", kind)
	}
	return c.hasVerifiedBotReply(ctx, repo, number, since, bot, marker, kind == "comment", kind == "review")
}

func (c *Client) hasVerifiedBotReply(ctx context.Context, repo string, number int, since time.Time, bot User, marker string, scanComments, scanReviews bool) (bool, error) {
	if bot.ID <= 0 {
		return false, fmt.Errorf("invalid bot account ID %d", bot.ID)
	}
	path, err := repoPath(repo)
	if err != nil {
		return false, err
	}
	if number <= 0 {
		return false, fmt.Errorf("invalid thread number %d", number)
	}
	match := func(item Item) (bool, error) {
		if item.Author.ID != bot.ID || !strings.HasSuffix(strings.TrimSpace(item.Body), "\n\n"+marker) {
			return false, nil
		}
		current, err := c.CurrentItem(ctx, item)
		if err != nil {
			return false, fmt.Errorf("check bot reply provenance: %w", err)
		}
		return !current.Edited && current.Author.ID == bot.ID && strings.HasSuffix(strings.TrimSpace(current.Body), "\n\n"+marker), nil
	}

	if scanComments {
		// A reply cannot predate its command, so older comments need no scan.
		q := sinceQuery(since)
		for page := 1; ; page++ {
			if page > 1000 {
				return false, errors.New("too many GitHub comment pages")
			}
			q.Set("page", strconv.Itoa(page))
			var batch []Comment
			header, _, err := c.request(ctx, http.MethodGet, withQuery(path+"/issues/"+strconv.Itoa(number)+"/comments", q), nil, &batch, nil, http.StatusOK)
			if err != nil {
				return false, err
			}
			for _, comment := range batch {
				if !eligible(nonzeroTime(comment.UpdatedAt, comment.CreatedAt), since) {
					continue
				}
				if found, err := match(commentItem(repo, "issue-comment", comment)); found || err != nil {
					return found, err
				}
			}
			if !hasNextPage(header.Get("Link")) {
				break
			}
		}
	}
	if !scanReviews {
		return false, nil
	}
	var found bool
	err = c.forEachReview(ctx, repo, number, since, func(item Item) (bool, error) {
		var err error
		found, err = match(item)
		return found, err
	})
	return found, err
}

func sinceQuery(since time.Time) url.Values {
	q := url.Values{"per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.Add(-time.Second).UTC().Format(time.RFC3339))
	}
	return q
}

func (c *Client) listComments(ctx context.Context, path string, since time.Time) ([]Comment, error) {
	q := sinceQuery(since)
	var all []Comment
	for page := 1; ; page++ {
		if page > 1000 {
			return nil, errors.New("too many GitHub comment pages")
		}
		q.Set("page", strconv.Itoa(page))
		var batch []Comment
		header, _, err := c.request(ctx, http.MethodGet, withQuery(path, q), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		for _, comment := range batch {
			if eligible(nonzeroTime(comment.UpdatedAt, comment.CreatedAt), since) {
				all = append(all, comment)
			}
		}
		if !hasNextPage(header.Get("Link")) {
			return all, nil
		}
	}
}

// forEachReview queries the PR timeline by submission time. A review created
// as PENDING before since is still returned when it is submitted afterward.
func (c *Client) forEachReview(ctx context.Context, repo string, number int, since time.Time, visit func(Item) (bool, error)) error {
	owner, name, _ := strings.Cut(repo, "/") // callers already validated repoPath
	var sinceValue any
	if !since.IsZero() {
		sinceValue = since.Add(-time.Second).UTC().Format(time.RFC3339)
	}
	const query = `query($owner: String!, $name: String!, $number: Int!, $since: DateTime, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(first: 100, after: $after, since: $since, itemTypes: [PULL_REQUEST_REVIEW]) {
        nodes { ... on PullRequestReview {
          id fullDatabaseId body state submittedAt url
          author { login ... on User { id: databaseId } ... on Bot { id: databaseId } }
        } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
	var cursor string
	for page := 1; page <= 20; page++ {
		var result struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						TimelineItems *struct {
							Nodes []struct {
								ID             string    `json:"id"`
								FullDatabaseID string    `json:"fullDatabaseId"`
								Body           string    `json:"body"`
								State          string    `json:"state"`
								SubmittedAt    time.Time `json:"submittedAt"`
								URL            string    `json:"url"`
								Author         User      `json:"author"`
							} `json:"nodes"`
							PageInfo *struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"timelineItems"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_, _, err := c.request(ctx, http.MethodPost, "/graphql", map[string]any{
			"query":     query,
			"variables": map[string]any{"owner": owner, "name": name, "number": number, "since": sinceValue, "after": cursor},
		}, &result, nil, http.StatusOK)
		if err != nil {
			return err
		}
		if len(result.Errors) != 0 {
			return fmt.Errorf("GitHub review timeline: %s", result.Errors[0].Message)
		}
		if result.Data.Repository == nil || result.Data.Repository.PullRequest == nil {
			return errors.New("GitHub pull request review timeline is unavailable")
		}
		timeline := result.Data.Repository.PullRequest.TimelineItems
		if timeline == nil || timeline.PageInfo == nil {
			return errors.New("GitHub review timeline is incomplete")
		}
		for _, review := range timeline.Nodes {
			if review.ID == "" {
				return errors.New("GitHub review timeline contains an unknown item")
			}
			if review.State == "PENDING" || review.SubmittedAt.IsZero() || !eligible(review.SubmittedAt, since) {
				continue
			}
			id, err := strconv.ParseInt(review.FullDatabaseID, 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("GitHub review has invalid ID %q", review.FullDatabaseID)
			}
			stop, err := visit(Item{
				Key:       fmt.Sprintf("%s#review:%d", strings.ToLower(repo), id),
				NodeID:    review.ID,
				Body:      review.Body,
				Author:    review.Author,
				UpdatedAt: review.SubmittedAt,
				HTMLURL:   review.URL,
			})
			if stop || err != nil {
				return err
			}
		}
		if !timeline.PageInfo.HasNextPage {
			return nil
		}
		if timeline.PageInfo.EndCursor == "" || timeline.PageInfo.EndCursor == cursor {
			return errors.New("GitHub review timeline did not advance")
		}
		cursor = timeline.PageInfo.EndCursor
	}
	return errors.New("too many recent GitHub review pages")
}
