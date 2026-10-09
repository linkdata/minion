package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestAuditIssueAndPullRequest(t *testing.T) {
	for _, tt := range []struct {
		name, subject, wantError            string
		failCodex, missingReport, movedHead bool
	}{
		{name: "issue default branch", subject: "Issue"},
		{name: "pull request head", subject: "PullRequest"},
		{name: "Codex failure", subject: "Issue", failCodex: true, wantError: "Codex unavailable"},
		{name: "missing report", subject: "PullRequest", missingReport: true, wantError: "REPORT.md"},
		{name: "moved pull request", subject: "PullRequest", movedHead: true, wantError: "pull request head changed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, head := auditGitFixture(t)
			expected := base
			if tt.subject == "PullRequest" {
				expected = head
			}
			apiHead := head
			if tt.movedHead {
				apiHead = strings.Repeat("f", 40)
			}
			job := monitor.Job{
				Repository:  config.Repository{Name: "o/r", Actions: []string{"audit"}},
				SubjectType: tt.subject, Number: 7, ID: strings.Repeat("a", 64),
				Source:  github.Item{Author: github.User{Login: "writer"}, Body: "@linkdataminion audit"},
				Command: command.Command{Action: command.Audit},
			}
			var comments []github.Comment
			usageStore := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					if _, err := fmt.Fprint(w, `{"full_name":"o/r","default_branch":"main"}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					if _, err := fmt.Fprint(w, `{"title":"Audit request","body":"Check authorization."}`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
					if _, err := fmt.Fprintf(w, `{"title":"Audit request","body":"Check authorization.","base":{"ref":"main"},"head":{"sha":%q}}`, apiHead); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
					if err := json.NewEncoder(w).Encode(comments); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && (r.URL.Path == "/repos/o/r/pulls/7/comments" || r.URL.Path == "/repos/o/r/pulls/7/reviews"):
					if _, err := fmt.Fprint(w, `[]`); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					usageRecords, err := usage.ReadAll(usageStore.Dir)
					if err != nil || len(usageRecords) != 1 || usageRecords[0].Tokens.Total() != 120 || !usageRecords[0].UsageComplete {
						t.Errorf("usage was not persisted before publishing: records=%+v err=%v", usageRecords, err)
					}
					var payload struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if !strings.Contains(payload.Body, expected) || !strings.Contains(payload.Body, "Audit summary") || !strings.Contains(payload.Body, job.Marker()) {
						t.Errorf("missing audit result, commit, or marker: %s", payload.Body)
					}
					comments = append(comments, github.Comment{Body: payload.Body, User: github.User{Login: "linkdataminion"}})
					w.WriteHeader(http.StatusCreated)
					if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("test-token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			skill := filepath.Join(t.TempDir(), "security-audit", "SKILL.md")
			var calls int
			var sourceDir, reportsDir string
			e := &Executor{
				GitHub: client, BotLogin: "linkdataminion", WorkRoot: t.TempDir(), Usage: usageStore,
				Codex: config.CodexConfig{Model: "test-model", ReasoningEffort: "high", Skills: []string{skill}},
				RunCodex: func(ctx context.Context, dir, prompt string, writable bool, outputDir string) (codexcli.Result, error) {
					calls++
					records, err := usage.ReadAll(usageStore.Dir)
					if err != nil || len(records) == 0 {
						t.Fatalf("read usage checkpoint: records=%+v err=%v", records, err)
					}
					current := records[len(records)-1]
					if current.Status != "running" || current.CodexCalls != 1 || current.UsageComplete {
						t.Fatalf("invalid pre-call checkpoint: %+v", current)
					}
					sourceDir, reportsDir = dir, outputDir
					if writable || outputDir == "" || strings.HasPrefix(outputDir, dir+string(os.PathSeparator)) {
						t.Fatalf("audit source writable or reports not separate: writable=%v output=%q source=%q", writable, outputDir, dir)
					}
					for _, want := range []string{skill, expected, outputDir, "full six-phase workflow", "independent agents", "needs_validation", "Check authorization."} {
						if !strings.Contains(prompt, want) {
							t.Fatalf("audit prompt missing %q", want)
						}
					}
					got, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
					if err != nil || strings.TrimSpace(string(got)) != expected {
						t.Fatalf("auditing wrong commit: %s, %v", got, err)
					}
					if tt.failCodex {
						return codexcli.Result{Tokens: usage.Tokens{Input: 10, CachedInput: 8, Output: 2}}, errors.New("Codex unavailable")
					}
					if !tt.missingReport {
						if err := os.WriteFile(filepath.Join(outputDir, "REPORT.md"), []byte("Audit report\n"), 0600); err != nil {
							return codexcli.Result{}, err
						}
					}
					return codexcli.Result{Answer: "Audit summary", Tokens: usage.Tokens{Input: 100, CachedInput: 80, Output: 20}, UsageComplete: true}, nil
				},
			}
			err := e.Execute(context.Background(), job)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) || len(comments) != 0 {
					t.Fatalf("error=%v comments=%d, want %q and no comment", err, len(comments), tt.wantError)
				}
				if tt.movedHead && calls != 0 {
					t.Fatal("Codex ran on changed PR head")
				}
				if tt.failCodex {
					if retryErr := e.Execute(context.Background(), job); retryErr == nil {
						t.Fatal("retry unexpectedly succeeded")
					}
				}
			} else {
				if err != nil || calls != 1 || len(comments) != 1 {
					t.Fatalf("audit failed: err=%v calls=%d comments=%d", err, calls, len(comments))
				}
				if _, err := os.Stat(filepath.Join(reportsDir, "REPORT.md")); err != nil {
					t.Fatalf("reports not retained: %v", err)
				}
				if err := e.Execute(context.Background(), job); err != nil || calls != 1 || len(comments) != 1 {
					t.Fatalf("duplicate audit executed: err=%v calls=%d comments=%d", err, calls, len(comments))
				}
			}
			records, usageErr := usage.ReadAll(usageStore.Dir)
			wantAttempts := 1
			if tt.failCodex {
				wantAttempts = 2
			}
			if usageErr != nil || len(records) != wantAttempts {
				t.Fatalf("usage attempts=%+v err=%v", records, usageErr)
			}
			for _, record := range records {
				wantStatus := "succeeded"
				if tt.wantError != "" {
					wantStatus = "failed"
				}
				wantCalls, wantTokens := 1, int64(120)
				if tt.movedHead {
					wantCalls, wantTokens = 0, 0
				}
				if tt.failCodex {
					wantTokens = 12
				}
				if record.Status != wantStatus || record.FinishedAt == nil || record.CodexCalls != wantCalls || record.Tokens.Total() != wantTokens || record.UsageComplete == tt.failCodex {
					t.Fatalf("unexpected final usage: %+v", record)
				}
				if record.JobID != job.ID || record.Repo != "o/r" || record.User != "writer" || record.Action != "audit" || record.Model != "test-model" || record.Effort != "high" {
					t.Fatalf("missing usage attribution: %+v", record)
				}
			}
			if sourceDir != "" {
				if _, err := os.Stat(sourceDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("temporary audit checkout retained: %v", err)
				}
			}
		})
	}
}

func auditGitFixture(t *testing.T) (base, head string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source, remote := filepath.Join(root, "source"), filepath.Join(root, "remote.git")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main", source)
	git("-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "base")
	base = git("-C", source, "rev-parse", "HEAD")
	git("clone", "--bare", source, remote)
	git("-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "PR")
	head = git("-C", source, "rev-parse", "HEAD")
	git("-C", source, "push", remote, "HEAD:refs/pull/7/head")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %q -c %q \"$@\"\n", realGit, "url."+remote+".insteadOf=https://github.com/o/r.git")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return base, head
}
