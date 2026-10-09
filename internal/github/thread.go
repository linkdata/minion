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
	UpdatedAt time.Time
	HTMLURL   string
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
	reviews, err := c.listReviews(ctx, path+"/pulls/"+strconv.Itoa(number)+"/reviews")
	if err != nil {
		return nil, err
	}
	for _, review := range reviews {
		if review.State == "PENDING" || !eligible(review.SubmittedAt, since) {
			continue
		}
		items = append(items, Item{
			Key:       fmt.Sprintf("%s#review:%d", strings.ToLower(repo), review.ID),
			NodeID:    review.NodeID,
			Body:      review.Body,
			Author:    review.User,
			UpdatedAt: review.SubmittedAt,
			HTMLURL:   review.HTMLURL,
		})
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

func (c *Client) listReviews(ctx context.Context, path string) ([]Review, error) {
	q := url.Values{"per_page": {"100"}}
	var all []Review
	for page := 1; ; page++ {
		if page > 1000 {
			return nil, errors.New("too many GitHub review pages")
		}
		q.Set("page", strconv.Itoa(page))
		var batch []Review
		header, _, err := c.request(ctx, http.MethodGet, withQuery(path, q), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if !hasNextPage(header.Get("Link")) {
			return all, nil
		}
	}
}
