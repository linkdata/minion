//go:build windows

package action

import (
	"errors"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestWindowsAuditPublicationRemainsRecoverable(t *testing.T) {
	job := testJob(command.SecurityAudit)
	validator := &Orchestrator{}
	if err := validator.validateAuditForPublication(t.Context(), job, nil); err == nil || errors.Is(err, errAuditPublicationUnavailable) {
		t.Fatalf("invalid audit input was treated as unavailable validator: %v", err)
	}
	err := validator.validateAuditForPublication(t.Context(), job, completeAuditResult(job).Artifacts)
	if !errors.Is(err, errAuditPublicationUnavailable) {
		t.Fatalf("Windows audit publication error = %v, want unavailable validator", err)
	}
}
