package action

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestSavedAuditDamagedPrivateCopyKeepsReceipt(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	store, _, request := savedRecoveryFixture(t, job, result)
	root, err := workspace.CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Orchestrator{Store: store, WorkRoot: root}
	prepared := &preparedAction{job: job, request: request}
	if _, err := e.preparePublication(t.Context(), prepared, result); err != nil {
		t.Fatalf("initial audit retention: %v", err)
	}
	report := filepath.Join(root, "audits", "complete-"+job.Identity.AttemptID, "REPORT.md")
	if err := os.WriteFile(report, []byte("damaged private copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.preparePublication(t.Context(), prepared, result); !errors.Is(err, monitor.ErrPostModelPublication) ||
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, errRetainedAuditMismatch) {
		t.Fatalf("damaged private copy discarded paid result: %v", err)
	} else if !strings.Contains(err.Error(), filepath.Dir(report)) {
		t.Fatalf("damaged private copy hid its operator path: %v", err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("damaged private copy lost receipt: present=%t error=%v", present, err)
	}
}
