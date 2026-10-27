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
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
)

func TestSavedAuditWaitsForPublishingValidator(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	nodePath := os.Getenv("PATH")
	job := testJob(command.SecurityAudit)
	job.Identity.Repository = "o/r"
	result := completeAuditResult(job)
	store, _, request := savedRecoveryFixture(t, job, result)
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts++
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected audit recovery request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	orchestrator := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: root,
		Runner: Runner{runModel: forbidModel(t)}}
	t.Setenv("PATH", t.TempDir()) // The publishing validator is missing after the result was saved.
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) ||
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, errAuditPublicationUnavailable) || posts != 0 {
		t.Fatalf("missing validator discarded audit: posts=%d error=%v", posts, err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("missing validator lost saved result: present=%t error=%v", present, err)
	}
	t.Setenv("PATH", nodePath)
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts != 1 {
		t.Fatalf("restored validator did not publish saved audit: posts=%d error=%v", posts, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "audits"))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "complete-") {
		t.Fatalf("published audit retention: entries=%v error=%v", entries, err)
	}
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "run-metadata.json" {
			result.Artifacts[i].Data = []byte(`{}`)
		}
	}
	if err := orchestrator.validateAuditForPublication(t.Context(), job, result.Artifacts); err == nil || errors.Is(err, errAuditPublicationUnavailable) {
		t.Fatalf("invalid report was treated as unavailable validator: %v", err)
	}
}

func TestMissingAuditScriptIsPublishingPrerequisite(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	job := testJob(command.SecurityAudit)
	skill, installed, err := skills.InstalledPathFor(testAuditSkillURL, "codex")
	if err != nil || !installed {
		t.Fatalf("test audit skill: installed=%t error=%v", installed, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(skill), auditValidators[0].script)); err != nil {
		t.Fatal(err)
	}
	orchestrator := &Orchestrator{WorkRoot: t.TempDir()}
	if err := orchestrator.validateAuditForPublication(t.Context(), job, completeAuditResult(job).Artifacts); !errors.Is(err, errAuditPublicationUnavailable) {
		t.Fatalf("missing audit validator was treated as invalid result: %v", err)
	}
}
