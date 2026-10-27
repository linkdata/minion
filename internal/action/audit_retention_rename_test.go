package action

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestCompletedAuditRenameFailureClassification(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "complete-audit")
	files := []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: []byte("accepted")}}
	renameErr := &os.LinkError{Op: "rename", Old: "stage", New: dir, Err: syscall.ENOSPC}

	// A failed rename with no destination is a host I/O failure, not a content mismatch.
	err := completedAuditRenameFailure(t.Context(), dir, files, renameErr)
	classified := retainedRecordFailure("retain completed audit", err)
	if !errors.Is(err, renameErr) || errors.Is(err, os.ErrNotExist) ||
		!errors.Is(classified, monitor.ErrPostModelPublication) || errors.Is(classified, monitor.ErrTerminalJob) {
		t.Fatalf("absent destination lost retryable rename error: %v", classified)
	}

	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(dir, files); err != nil {
		t.Fatal(err)
	}
	if err := completedAuditRenameFailure(t.Context(), dir, files, renameErr); err != nil {
		t.Fatalf("matching concurrent retention was rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "REPORT.md"), []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	err = completedAuditRenameFailure(t.Context(), dir, files, renameErr)
	classified = retainedRecordFailure("retain completed audit", err)
	if !errors.Is(err, renameErr) || !strings.Contains(err.Error(), "retained completed audit differs") ||
		!errors.Is(classified, monitor.ErrTerminalJob) || errors.Is(classified, monitor.ErrPostModelPublication) {
		t.Fatalf("content mismatch was not terminal: %v", classified)
	}
}
