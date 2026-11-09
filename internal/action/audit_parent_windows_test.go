//go:build windows

package action

import (
	"errors"
	"testing"
)

func TestWindowsAuditPublicationRemainsRecoverable(t *testing.T) {
	err := (&Orchestrator{}).validateAuditForPublication(t.Context(), Job{}, nil)
	if !errors.Is(err, errAuditPublicationUnavailable) {
		t.Fatalf("Windows audit publication error = %v, want unavailable validator", err)
	}
}
