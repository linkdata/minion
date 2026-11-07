package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

func TestServiceRecoversTerminalDecisionBeforeAdminTLSFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	markers := startupCommandMarkers(t)
	t.Setenv("GH_TOKEN", "offline-token")
	t.Setenv("GITHUB_TOKEN", "")

	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")
	at := time.Now().UTC().Add(-time.Minute)
	accepted := state.AcceptedAction{MainProvider: "codex", MainSettings: frozenTestCodex,
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "comment:1", SourceNodeID: "source-node",
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: at, Deadline: at.Add(time.Hour), Assigned: true,
	}
	store, err := state.Open(statePath, at)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = frozenFailureForTest(accepted)
	store.Data.TerminalDecisions[accepted.JobID] = state.TerminalDecision{
		AttemptID: accepted.AttemptID, Outcome: "succeeded", DecidedAt: at,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/user" {
			t.Errorf("unexpected GitHub request: %s %s", request.Method, request.URL)
			return nil, errors.New("unexpected GitHub request")
		}
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	cfg := config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "offline-token"}, StatePath: statePath, WorkRoot: root,
		Admin: &config.AdminConfig{Listen: "127.0.0.1:8377",
			TLSCertFile: filepath.Join(root, "missing-cert.pem"), TLSKeyFile: filepath.Join(root, "missing-key.pem")},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	gitChecks := 0
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			gitChecks++
			return nil
		})
	})
	recovered := strings.Index(output, "confirmed accepted terminal decision job=job-1 outcome=succeeded")
	tlsFailure := strings.Index(output, "admin TLS preflight:")
	if code != 1 || gitChecks != 1 || recovered < 0 || tlsFailure <= recovered {
		t.Fatalf("terminal recovery did not precede admin TLS failure: exit=%d git_checks=%d log=%q", code, gitChecks, output)
	}
	assertNoStartupCommands(t, markers)

	store, err = state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
		len(store.Data.JobFailures) != 0 || store.Data.TerminalDecisions[accepted.JobID].Outcome != "succeeded" {
		t.Fatalf("terminal recovery was not durable before admin TLS failure: %+v", store.Data)
	}
}
