package action

import "testing"

func TestStopCleanupEvidenceKeepsUncleanAndUnreachableWorkersUnverified(t *testing.T) {
	localExecutor, err := NewHTTPExecutor(LocalExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	local := &localWorker{executor: localExecutor, exited: make(chan struct{})}
	owner := &Orchestrator{Executor: local}
	if !owner.StopCleanupVerified() {
		t.Fatal("idle clean local worker was unverified")
	}
	local.unclean.Store(true)
	if owner.StopCleanupVerified() {
		t.Fatal("unclean local worker was confirmed")
	}
	local.unclean.Store(false)
	local.reuse.Store(workerRetired)
	if owner.StopCleanupVerified() {
		t.Fatal("local worker not yet reaped was confirmed")
	}
	close(local.exited)
	if !owner.StopCleanupVerified() {
		t.Fatal("reaped clean local worker remained unverified")
	}

	remoteExecutor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	remote := &remoteWorker{executor: remoteExecutor, endpoint: &RemoteEndpoint{serveDone: make(chan struct{})}}
	owner.Executor = remote
	if !owner.StopCleanupVerified() {
		t.Fatal("connected clean remote worker was unverified")
	}
	remote.unclean.Store(true)
	if owner.StopCleanupVerified() {
		t.Fatal("unclean remote worker was confirmed")
	}
	remote.unclean.Store(false)
	remoteExecutor.unavailable = true
	if owner.StopCleanupVerified() {
		t.Fatal("quarantined remote worker was confirmed")
	}
	remoteExecutor.unavailable = false
	close(remote.endpoint.serveDone)
	if owner.StopCleanupVerified() {
		t.Fatal("unreachable remote worker was confirmed")
	}
}
