package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestCompletedAuditRetentionIsReusable(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	prepared := &preparedAction{job: job}
	root := t.TempDir()
	auditRoot := filepath.Join(root, "audits")
	stage := filepath.Join(auditRoot, ".complete-"+job.Identity.AttemptID)
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "partial"), []byte("interrupted write"), 0600); err != nil {
		t.Fatal(err)
	}
	orchestrator := &Orchestrator{WorkRoot: root}
	for range 2 {
		if err := orchestrator.retainAudit(t.Context(), prepared, result.Artifacts, true); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(auditRoot)
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
