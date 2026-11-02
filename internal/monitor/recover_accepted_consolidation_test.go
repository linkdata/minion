package monitor

import (
	"os"
	"testing"
	"time"
)

func TestOverdueUnassignedRecoverySkipsResultInspection(t *testing.T) {
	m, store, job, _, path := assignmentValidationFixture(t)
	at := time.Now().UTC().Add(-8 * time.Minute)
	accepted := store.Data.Accepted
	accepted.AcceptedAt, accepted.Deadline = at, at.Add(7*time.Minute)
	failure := store.Data.JobFailures[job.ID]
	failure.AcceptedAt, failure.Deadline = accepted.AcceptedAt, accepted.Deadline
	store.Data.JobFailures[job.ID] = failure
	m.Now = time.Now
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	// HasAcceptedResult rejects a directory; an unassigned expired action has
	// no required Result receipt and should reach terminal cleanup directly.
	if err := os.Mkdir(path+".result", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverAccepted(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "expired" ||
		store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() {
		t.Fatalf("overdue unassigned recovery changed terminal policy: decision=%+v state=%+v", decision, store.Data)
	}
}
