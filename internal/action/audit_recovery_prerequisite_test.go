package action

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestSavedAuditRetriesCompletedRetentionIO(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission fixture requires a non-root Unix account")
	}
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	job.Identity.Repository = "o/r"
	result := completeAuditResult(job)
	store, _, request := savedRecoveryFixture(t, job, result)
	posts := 0
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
	auditDir := filepath.Join(root, "audits")
	if err := os.Mkdir(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(auditDir, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(auditDir, 0700) })
	orchestrator := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: root,
		Runner: Runner{runModel: forbidModel(t)}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) ||
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, os.ErrPermission) || posts != 0 {
		t.Fatalf("retention I/O did not remain retryable: posts=%d error=%v", posts, err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("retention I/O lost saved result: present=%t error=%v", present, err)
	}
	if err := os.Chmod(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts != 1 {
		t.Fatalf("restored retention did not publish saved audit: posts=%d error=%v", posts, err)
	}
}

func TestSavedAuditWaitsForReadablePublishingValidator(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("script permission fixture requires a non-root Unix account")
	}
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	job.Identity.Repository = "o/r"
	result := completeAuditResult(job)
	store, _, request := savedRecoveryFixture(t, job, result)
	skill, installed, err := skills.InstalledPathFor(testAuditSkillURL, "codex")
	if err != nil || !installed {
		t.Fatalf("test audit skill: installed=%t error=%v", installed, err)
	}
	script := filepath.Join(filepath.Dir(skill), auditValidators[0].script)
	if err := os.Chmod(script, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(script, 0600) })
	posts := 0
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
	orchestrator := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) ||
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, errAuditPublicationUnavailable) ||
		!errors.Is(err, os.ErrPermission) || posts != 0 {
		t.Fatalf("unreadable validator discarded saved audit: posts=%d error=%v", posts, err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("unreadable validator lost saved result: present=%t error=%v", present, err)
	}
	if err := os.Chmod(script, 0600); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts != 1 {
		t.Fatalf("restored validator did not publish saved audit: posts=%d error=%v", posts, err)
	}
}
