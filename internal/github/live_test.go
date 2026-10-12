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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	liveCommentQuery = `query($id: ID!) { node(id: $id) { ... on Comment { body author { login ... on User { id: databaseId } } editor { login } lastEditedAt } } }`
	liveIssueQuery   = `query($id: ID!) { node(id: $id) { ... on Issue { title bodyText lastEditedAt } } }`
)

type liveReadOnlyTransport struct {
	base http.RoundTripper
}

type liveRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip liveRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func (transport liveReadOnlyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" || request.URL.User != nil || (request.Host != "" && request.Host != "api.github.com") {
		return nil, errors.New("live test blocked request outside api.github.com")
	}
	if request.Method == http.MethodGet && request.URL.Path != "/graphql" && (request.Body == nil || request.Body == http.NoBody) && request.Header.Get("X-HTTP-Method-Override") == "" {
		return transport.base.RoundTrip(request)
	}
	if request.Method != http.MethodPost || request.URL.Path != "/graphql" || request.URL.RawQuery != "" || request.Body == nil || request.Header.Get("X-HTTP-Method-Override") != "" {
		return nil, fmt.Errorf("live test blocked %s %s", request.Method, request.URL.Path)
	}
	data, readErr := io.ReadAll(io.LimitReader(request.Body, 4097))
	closeErr := request.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > 4096 {
		return nil, errors.New("live test blocked oversized GraphQL request")
	}
	var input struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("live test blocked invalid GraphQL request: %w", err)
	}
	switch input.Query {
	case liveCommentQuery, liveIssueQuery:
	default:
		return nil, errors.New("live test blocked unapproved GraphQL operation")
	}
	request.Body = io.NopCloser(bytes.NewReader(data))
	return transport.base.RoundTrip(request)
}

type liveTestConfig struct {
	OwnerLogin        string `json:"owner_login"`
	BotLogin          string `json:"bot_login"`
	RepositoryURL     string `json:"repository_url"`
	IssueNumber       int    `json:"issue_number"`
	PullRequestNumber int    `json:"pull_request_number"`
	ReleaseTag        string `json:"release_tag"`
	BotSSHKey         string `json:"bot_ssh_key"`
	Repo              string `json:"-"`
}

func loadLiveTestConfig(t *testing.T) liveTestConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "live-tests.local.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("live-tests.local.json is absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg liveTestConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerLogin == "" || cfg.BotLogin == "" || strings.EqualFold(cfg.OwnerLogin, cfg.BotLogin) {
		t.Fatal("live test requires two distinct GitHub accounts")
	}
	repoURL, err := url.Parse(cfg.RepositoryURL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.Trim(repoURL.Path, "/"), "/")
	if repoURL.Scheme != "https" || repoURL.Host != "github.com" || repoURL.User != nil || repoURL.RawQuery != "" || repoURL.Fragment != "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.EqualFold(parts[0], cfg.OwnerLogin) {
		t.Fatalf("invalid repository URL %q", cfg.RepositoryURL)
	}
	cfg.Repo = strings.Join(parts, "/")
	if _, err := repoPath(cfg.Repo); err != nil {
		t.Fatal(err)
	}
	if cfg.IssueNumber < 0 {
		t.Fatal("issue_number must be positive when set")
	}
	if cfg.PullRequestNumber < 0 {
		t.Fatal("pull_request_number must be positive when set")
	}
	return cfg
}

func liveClient(t *testing.T, ctx context.Context, cfg liveTestConfig) *Client {
	t.Helper()
	token, err := liveAccountToken(ctx, cfg.OwnerLogin)
	if err != nil {
		t.Fatal(err)
	}
	client := New(token)
	client.HTTPClient.Transport = liveReadOnlyTransport{base: http.DefaultTransport}
	return client
}

func liveAccountToken(ctx context.Context, login string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com", "--user", login)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token for %s: %w", login, err)
	}
	if token := strings.TrimSpace(string(output)); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("gh auth token for %s is empty", login)
}

func TestLiveSmoke(t *testing.T) {
	cfg := loadLiveTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, login := range []string{cfg.OwnerLogin, cfg.BotLogin} {
		t.Run(login, func(t *testing.T) {
			got := liveAPIField(t, ctx, cfg.OwnerLogin, "users/"+url.PathEscape(login), ".login")
			if !strings.EqualFold(got, login) {
				t.Fatalf("GitHub user = %q, want %q", got, login)
			}
		})
	}
	t.Run("repository", func(t *testing.T) {
		want := cfg.Repo
		got := liveAPIField(t, ctx, cfg.OwnerLogin, "repos/"+want, ".full_name")
		if !strings.EqualFold(got, want) {
			t.Fatalf("GitHub repository = %q, want %q", got, want)
		}
	})
}

func liveAPIField(t *testing.T, ctx context.Context, ownerLogin, endpoint, field string) string {
	t.Helper()
	token, err := liveAccountToken(ctx, ownerLogin)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", "--method", "GET", endpoint, "--jq", field)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_TOKEN="+token)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh api %s: %v: %s", endpoint, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func TestReadOnlyTransportGuard(t *testing.T) {
	for _, test := range []struct {
		name    string
		method  string
		url     string
		body    string
		allowed bool
	}{
		{"REST read", http.MethodGet, "https://api.github.com/user", "", true},
		{"comment query", http.MethodPost, "https://api.github.com/graphql", `{"query":` + strconv.Quote(liveCommentQuery) + `}`, true},
		{"issue query", http.MethodPost, "https://api.github.com/graphql", `{"query":` + strconv.Quote(liveIssueQuery) + `}`, true},
		{"REST write", http.MethodPost, "https://api.github.com/repos/o/r/issues", `{}`, false},
		{"GraphQL mutation", http.MethodPost, "https://api.github.com/graphql", `{"query":"mutation { deleteRepository(input: {}) { clientMutationId } }"}`, false},
		{"GraphQL query parameters", http.MethodPost, "https://api.github.com/graphql?operationName=Delete", `{"query":` + strconv.Quote(liveIssueQuery) + `}`, false},
		{"GraphQL GET", http.MethodGet, "https://api.github.com/graphql", "", false},
		{"REST GET body", http.MethodGet, "https://api.github.com/user", `{}`, false},
		{"other host", http.MethodGet, "https://example.com/user", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			guard := liveReadOnlyTransport{base: liveRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				called = true
				if test.body != "" {
					data, err := io.ReadAll(request.Body)
					if err != nil {
						t.Error(err)
					}
					if string(data) != test.body {
						t.Errorf("forwarded body = %q, want %q", data, test.body)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			request, err := http.NewRequest(test.method, test.url, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := guard.RoundTrip(request)
			if test.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if closeErr := response.Body.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			} else if err == nil {
				t.Fatal("unsafe request passed live-test guard")
			}
			if called != test.allowed {
				t.Fatalf("network called = %t, want %t", called, test.allowed)
			}
		})
	}
}
