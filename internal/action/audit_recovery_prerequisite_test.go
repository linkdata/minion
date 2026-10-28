package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
)

func TestCanceledSavedAuditDoesNotRetainIncompleteCopy(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	t.Setenv("PATH", t.TempDir())
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	store, _, request := savedRecoveryFixture(t, job, result)
	root := t.TempDir()
	e := &Orchestrator{Store: store, WorkRoot: root}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.preparePublication(ctx, &preparedAction{job: job, request: request}, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled saved audit did not preserve cancellation: %v", err)
	}
	if dirs, err := filepath.Glob(filepath.Join(root, "audits", "incomplete-*")); err != nil || len(dirs) != 0 {
		t.Fatalf("saved audit was mislabeled incomplete: dirs=%v error=%v", dirs, err)
	}
}

func TestInProcessAuditRetainsIncompleteWhenValidatorUnavailable(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	e := &Orchestrator{WorkRoot: root}
	if _, err := e.preparePublication(t.Context(), &preparedAction{job: job}, result); !errors.Is(err, monitor.ErrPostModelPublication) || !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, errAuditPublicationUnavailable) || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing validator was not terminal before a saved result: %v", err)
	}
	dirs, err := filepath.Glob(filepath.Join(root, "audits", "incomplete-*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("in-process audit lost private incomplete copy: dirs=%v error=%v", dirs, err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "INCOMPLETE.txt")); err != nil {
		t.Fatalf("incomplete audit marker missing: %v", err)
	}
}

func TestValidatedAuditDoesNotFailAfterScratchCleanupError(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	var journal bytes.Buffer
	e := &Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0)}
	original := removeAuditValidationDir
	t.Cleanup(func() { removeAuditValidationDir = original })
	removeAuditValidationDir = func(path string) error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return errors.New("cleanup unavailable")
	}
	if err := e.validateAuditForPublication(t.Context(), job, result.Artifacts); err != nil {
		t.Fatalf("validated audit was blocked by scratch cleanup: %v", err)
	}
	if !strings.Contains(journal.String(), "audit validation scratch cleanup failed") || !strings.Contains(journal.String(), "cleanup unavailable") {
		t.Fatalf("cleanup failure was not logged: %q", journal.String())
	}
}

func TestSavedAuditWaitsForPublishingValidator(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	nodePath := os.Getenv("PATH")
	job := testJob(command.SecurityAudit)
	job.Identity.Repository = "o/r"
	result := completeAuditResult(job)
	store, _, request := savedRecoveryFixture(t, job, result)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts.Add(1)
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
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, errAuditPublicationUnavailable) || !errors.Is(err, exec.ErrNotFound) || posts.Load() != 0 {
		t.Fatalf("missing validator discarded audit: posts=%d error=%v", posts.Load(), err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("missing validator lost saved result: present=%t error=%v", present, err)
	}
	t.Setenv("PATH", nodePath)
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 {
		t.Fatalf("restored validator did not publish saved audit: posts=%d error=%v", posts.Load(), err)
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
	if err := orchestrator.validateAuditForPublication(t.Context(), job, completeAuditResult(job).Artifacts); !errors.Is(err, errAuditPublicationUnavailable) || !errors.Is(err, os.ErrNotExist) {
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
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts.Add(1)
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
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, os.ErrPermission) || posts.Load() != 0 {
		t.Fatalf("retention I/O did not remain retryable: posts=%d error=%v", posts.Load(), err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("retention I/O lost saved result: present=%t error=%v", present, err)
	}
	if err := os.Chmod(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 {
		t.Fatalf("restored retention did not publish saved audit: posts=%d error=%v", posts.Load(), err)
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
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts.Add(1)
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
		!errors.Is(err, os.ErrPermission) || posts.Load() != 0 {
		t.Fatalf("unreadable validator discarded saved audit: posts=%d error=%v", posts.Load(), err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("unreadable validator lost saved result: present=%t error=%v", present, err)
	}
	if err := os.Chmod(script, 0600); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 {
		t.Fatalf("restored validator did not publish saved audit: posts=%d error=%v", posts.Load(), err)
	}
}

func TestAuditValidationHostIOClassification(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		retry bool
	}{
		{"artifact copy full", &os.PathError{Op: "write", Path: "findings.json", Err: syscall.ENOSPC}, true},
		{"artifact copy I/O", &os.PathError{Op: "write", Path: "findings.json", Err: syscall.EIO}, true},
		{"validator launch descriptors", &os.PathError{Op: "fork/exec", Path: "node", Err: syscall.EMFILE}, true},
		{"validator launch retryable", &os.PathError{Op: "fork/exec", Path: "node", Err: syscall.EAGAIN}, true},
		{"validator launch memory", &os.PathError{Op: "fork/exec", Path: "node", Err: syscall.ENOMEM}, true},
		{"validator text busy", &os.PathError{Op: "fork/exec", Path: "node", Err: syscall.ETXTBSY}, true},
		{"validator missing", os.ErrNotExist, true},
		{"invalid content", errors.New("audit metadata is invalid"), false},
		{"invalid content and I/O", errors.Join(errors.New("audit metadata is invalid"), syscall.ENOSPC), false},
		{"invalid content and missing validator", errors.Join(errors.New("audit metadata is invalid"), os.ErrNotExist), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := fmt.Errorf("audit validation: %w", test.cause)
			if got := auditValidationUnavailable(err); got != test.retry {
				t.Fatalf("host I/O classification = %t for %v", got, err)
			}
		})
	}
}

func TestInvalidAuditInputFailsBeforeUnavailableValidator(t *testing.T) {
	job := testJob(command.SecurityAudit)
	files := completeAuditResult(job).Artifacts
	for i := range files {
		if files[i].Path == "REPORT.md" {
			files[i].Data = nil
		}
	}
	e := &Orchestrator{WorkRoot: t.TempDir()}
	err := e.validateAuditForPublication(t.Context(), job, files)
	if err == nil || errors.Is(err, errAuditPublicationUnavailable) || !strings.Contains(err.Error(), "REPORT.md") {
		t.Fatalf("invalid report with unavailable validator: %v", err)
	}
}
