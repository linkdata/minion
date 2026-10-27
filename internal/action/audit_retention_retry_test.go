package action

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
)

func TestCompletedAuditRetentionIsReusableAcrossPlatforms(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	prepared := &preparedAction{job: job}
	root := t.TempDir()
	orchestrator := &Orchestrator{WorkRoot: root}
	for range 2 {
		if err := orchestrator.retainAudit(t.Context(), prepared, result.Artifacts, true); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "audits"))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "complete-") {
		t.Fatalf("retry duplicated retained audit: entries=%v error=%v", entries, err)
	}
	report := filepath.Join(root, "audits", entries[0].Name(), "REPORT.md")
	if err := os.WriteFile(report, []byte("corrupt report"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.retainAudit(t.Context(), prepared, result.Artifacts, true); err == nil || !strings.Contains(err.Error(), "retained completed audit differs") {
		t.Fatalf("corrupt retained report was accepted: %v", err)
	}
}

func TestCompletedAuditRetentionSurvivesReplyRetry(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts++
			if posts == 1 {
				http.Error(w, "reply failed", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	request := monitor.Job{ID: job.Identity.JobID, Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, SubjectType: "Issue", Number: 7,
		Source:  github.Item{Author: github.User{ID: job.Identity.RequesterID, Login: job.Identity.Requester}},
		Command: command.Command{Name: job.Command, Action: command.SecurityAudit}}
	prepared := &preparedAction{job: job, request: request}
	first := &Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "token")}
	if err := first.publish(t.Context(), prepared, result); !errors.Is(err, monitor.ErrPostModelPublication) || posts != 1 {
		t.Fatalf("failed reply: posts=%d error=%v", posts, err)
	}
	auditRoot := filepath.Join(root, "audits")
	entries, err := os.ReadDir(auditRoot)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "complete-") {
		t.Fatalf("first retention: entries=%v error=%v", entries, err)
	}
	dir := filepath.Join(auditRoot, entries[0].Name())
	report := filepath.Join(dir, "REPORT.md")
	if err := os.Remove(report); err != nil {
		t.Fatal(err)
	}
	// A partial or damaged retained copy must stop publication, not count as saved.
	restarted := &Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "token")}
	if err := restarted.publish(t.Context(), prepared, result); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "retained completed audit differs") || posts != 1 {
		t.Fatalf("missing retained report: posts=%d error=%v", posts, err)
	}
	for _, file := range result.Artifacts {
		if file.Path == "REPORT.md" {
			if err := os.WriteFile(report, file.Data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(report, os.FileMode(file.Mode&0777)); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if err := restarted.publish(t.Context(), prepared, result); err != nil || posts != 2 {
		t.Fatalf("recovered reply: posts=%d error=%v", posts, err)
	}
	entries, err = os.ReadDir(auditRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(dir) {
		t.Fatalf("retry duplicated retained audit: entries=%v error=%v", entries, err)
	}
	if err := os.WriteFile(report, []byte("corrupt report"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restarted.retainAudit(t.Context(), prepared, result.Artifacts, true); err == nil || !strings.Contains(err.Error(), "retained completed audit differs") {
		t.Fatalf("corrupt retained report was accepted: %v", err)
	}
}
