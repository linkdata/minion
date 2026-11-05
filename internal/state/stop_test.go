package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

func stopStoreFixture(t *testing.T) (*Store, string, AcceptedAction) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accepted := acceptedActionFixture()
	accepted.AcceptedAt = time.Now().UTC()
	accepted.Deadline = accepted.AcceptedAt.Add(time.Hour)
	store.Data.Accepted = &accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	return store, path, accepted
}

func TestStopRequesterTypedGitHubID(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"operator", true}, {"github-user-3", true}, {"github-user:3", true},
		{"github-user:9223372036854775807", true},
		{"github-user:", false}, {"github-user:0", false}, {"github-user:03", false},
		{"github-user:+3", false}, {"github-user:-3", false}, {"github-user:3:4", false},
		{"github-user:abc", false}, {"github-user:9223372036854775808", false},
		{"github-user:11111111111111111111", false},
		{"github-user:３", false}, {"github-user:3 ", false},
	} {
		if got := validStopRequester(tc.name); got != tc.want {
			t.Errorf("validStopRequester(%q) = %t, want %t", tc.name, got, tc.want)
		}
	}
	if workspace.ValidIdentitySegment("github-user:3") {
		t.Fatal("typed GitHub user ID also qualifies as a local admin name")
	}
	store, _, accepted := stopStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	for _, invalid := range []string{"github-user:0", "github-user:03", "github-user:abc"} {
		if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, invalid, time.Now().UTC()); err == nil || err.Error() != "invalid stop request" {
			t.Fatalf("invalid requester %q: %v", invalid, err)
		}
	}
	if ctx.Err() != nil || store.Data.Accepted.StopRequest != (StopRequest{}) {
		t.Fatal("invalid requester changed selected action")
	}
	request, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "github-user:3", time.Now().UTC())
	if err != nil || ctx.Err() != context.Canceled || !request.valid() {
		t.Fatalf("typed GitHub stop: request=%+v err=%v canceled=%v", request, err, ctx.Err())
	}
	saved, err := store.ReadSavedDurable()
	if err != nil || saved.Accepted == nil || saved.Accepted.StopRequest != request {
		t.Fatalf("typed GitHub stop not persisted: accepted=%+v err=%v", saved.Accepted, err)
	}
}

func TestRequestStopChecksAttemptAndPreservesFirstRequester(t *testing.T) {
	store, _, accepted := stopStoreFixture(t)
	accepted.OutcomeBinding = strings.Repeat("a", 64)
	store.Data.Accepted.OutcomeBinding = accepted.OutcomeBinding
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	at := time.Now().UTC()
	if _, err := store.RequestStop(accepted.JobID, "stale-attempt", "operator", at); !errors.Is(err, ErrStopConflict) {
		t.Fatalf("stale attempt stop = %v, want conflict", err)
	}
	if ctx.Err() != nil || store.Data.Accepted.Terminal {
		t.Fatal("stale attempt canceled current worker")
	}
	request, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", at)
	if err != nil || ctx.Err() != context.Canceled || !store.Data.Accepted.Terminal || store.Data.LastStopJobID != accepted.JobID {
		t.Fatalf("stop request: request=%+v error=%v state=%+v", request, err, store.Data)
	}
	savedPath := store.path
	store.path = filepath.Join(filepath.Dir(savedPath), "missing", "state.json")
	if repeated, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "other-admin", at.Add(time.Minute)); err != nil || repeated != request {
		t.Fatalf("repeat rewrote a durable request: request=%+v error=%v", repeated, err)
	}
	unregister()
	if repeated, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "other-admin", at.Add(2*time.Minute)); err != nil || repeated != request {
		t.Fatalf("pending cleanup repeat rewrote a durable request: request=%+v error=%v", repeated, err)
	}
	store.path = savedPath
	decision, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", at.Add(time.Second))
	if err != nil || decision.Outcome != "canceled" || decision.StopRequest != request ||
		decision.OutcomeBinding != accepted.OutcomeBinding || decision.Deadline != accepted.Deadline {
		t.Fatalf("completion overwrote stop: decision=%+v error=%v", decision, err)
	}
	if err := store.ResolveStop(accepted.JobID, accepted.AttemptID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if store.Data.TerminalDecisions[accepted.JobID].StopRequest.Status != "confirmed" {
		t.Fatal("terminal decision did not record verified cleanup")
	}
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if repeated, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "other-admin", at.Add(time.Hour)); err != nil || repeated.Status != "confirmed" || repeated.RequestedBy != "operator" {
		t.Fatalf("completed repeat: request=%+v error=%v", repeated, err)
	}
}

func TestRequestStopRequiresLiveRunAndFencesBeforeAssignment(t *testing.T) {
	store, _, accepted := stopStoreFixture(t)
	store.Data.Accepted.Assigned = false
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); !errors.Is(err, ErrStopConflict) {
		t.Fatalf("preparation stop = %v, want conflict", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled || !store.Data.Accepted.Terminal {
		t.Fatal("request-before-assignment missed selected execution")
	}
}

func TestPreparationUnregisterKeepsWorkerStopRegistered(t *testing.T) {
	store, _, accepted := stopStoreFixture(t)
	prepCtx, cancelPrep := context.WithCancel(t.Context())
	defer cancelPrep()
	unregisterPrep := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancelPrep)
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	defer cancelWorker()
	unregisterWorker := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancelWorker)
	defer unregisterWorker()
	unregisterPrep()
	if !store.SelectedStopActive(accepted.JobID, accepted.AttemptID) {
		t.Fatal("preparation cleanup erased the worker stop registration")
	}
	if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if workerCtx.Err() != context.Canceled || prepCtx.Err() != nil {
		t.Fatalf("stop canceled the wrong registration: worker=%v preparation=%v", workerCtx.Err(), prepCtx.Err())
	}
}

func TestPreparationReleaseOrdersStopRequest(t *testing.T) {
	t.Run("stop first", func(t *testing.T) {
		store, _, accepted := stopStoreFixture(t)
		unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, func() {})
		if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if !unregister() {
			t.Fatal("release missed the saved stop")
		}
	})
	t.Run("release first", func(t *testing.T) {
		store, _, accepted := stopStoreFixture(t)
		unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, func() {})
		if unregister() {
			t.Fatal("release found an absent stop")
		}
		if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); !errors.Is(err, ErrStopConflict) {
			t.Fatalf("stop after release = %v, want conflict", err)
		}
	})
}

func TestRequestStopCannotReplaceCompletedDecision(t *testing.T) {
	store, _, accepted := stopStoreFixture(t)
	if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); !errors.Is(err, ErrStopConflict) {
		t.Fatalf("completed action stop = %v, want conflict", err)
	}
	if decision := store.Data.TerminalDecisions[accepted.JobID]; decision.Outcome != "succeeded" || decision.StopRequest != (StopRequest{}) {
		t.Fatalf("completed action decision changed: %+v", decision)
	}
}

func TestRequestStopPostReplacementErrorStillCancelsAndRecovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not sync directories after atomic replacement")
	}
	store, path, accepted := stopStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	store.syncDir = filepath.Join(filepath.Dir(path), "missing-sync")
	request, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC())
	if err == nil || ctx.Err() != context.Canceled || request.Status != "requested" {
		t.Fatalf("post-replacement error: request=%+v error=%v canceled=%v", request, err, ctx.Err())
	}
	saved, err := store.ReadSaved()
	if err != nil || saved.Accepted == nil || saved.Accepted.StopRequest != request || !saved.Accepted.Terminal {
		t.Fatalf("post-replacement stop not durable: state=%+v error=%v", saved, err)
	}
	unregister()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.Accepted == nil || reopened.Data.Accepted.StopRequest != request || !reopened.Data.Accepted.Terminal {
		t.Fatalf("restart forgot durable stop: %+v", reopened.Data)
	}
}

func TestRequestStopUnconfirmedDirectorySyncCancelsWithoutAcknowledgment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not sync directories after atomic replacement")
	}
	store, path, accepted := stopStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	store.syncDir = filepath.Join(filepath.Dir(path), "missing-sync")
	readWithoutConfirmedSync := func() (Data, error) {
		return store.readSavedDurable(func(dirs ...string) error {
			if len(dirs) != 1 || dirs[0] != filepath.Dir(path) {
				t.Fatalf("wrong state directory sync: %v", dirs)
			}
			return os.ErrPermission
		})
	}
	request, err := store.requestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC(), readWithoutConfirmedSync)
	if err == nil || ctx.Err() != context.Canceled || request.Status != "requested" {
		t.Fatalf("unconfirmed directory sync: request=%+v error=%v canceled=%v", request, err, ctx.Err())
	}
	if saved, err := store.ReadSaved(); err != nil || saved.Accepted == nil || saved.Accepted.StopRequest != request {
		t.Fatalf("replacement is not visible: state=%+v error=%v", saved, err)
	}
	unregister()
	if _, err := store.requestStop(accepted.JobID, accepted.AttemptID, "other-admin", time.Now().UTC(), readWithoutConfirmedSync); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("repeat acknowledged unsynced request: %v", err)
	}
	if _, err := store.requestStop(accepted.JobID, "stale-attempt", "other-admin", time.Now().UTC(), readWithoutConfirmedSync); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("stale attempt acknowledged unsynced request: %v", err)
	}
	if repeated, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "other-admin", time.Now().UTC()); err != nil || repeated != request {
		t.Fatalf("repeat after directory sync: request=%+v error=%v", repeated, err)
	}
	if _, err := store.RequestStop(accepted.JobID, "stale-attempt", "other-admin", time.Now().UTC()); !errors.Is(err, ErrStopConflict) {
		t.Fatalf("stale attempt accepted after directory sync: %v", err)
	}
}

func TestRequestStopBeforeReplacementFailureCanRetry(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block atomic replacement on this runner")
	}
	store, path, accepted := stopStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	_, stopErr := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC())
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if stopErr == nil || ctx.Err() != nil || store.Data.Accepted.StopRequest != (StopRequest{}) || store.Data.Accepted.Terminal {
		t.Fatalf("pre-replacement failure accepted an unsaved stop: error=%v canceled=%v state=%+v", stopErr, ctx.Err(), store.Data)
	}
	saved, err := store.ReadSaved()
	if err != nil || saved.Accepted == nil || saved.Accepted.StopRequest != (StopRequest{}) {
		t.Fatalf("pre-replacement failure changed saved state: state=%+v error=%v", saved, err)
	}
	if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); err != nil || ctx.Err() != context.Canceled {
		t.Fatalf("retry did not cancel after save: error=%v canceled=%v", err, ctx.Err())
	}
}

func TestResolveStopRetriesPreReplacementFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block atomic replacement on this runner")
	}
	store, path, accepted := stopStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	if _, err := store.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); err != nil || ctx.Err() != context.Canceled {
		t.Fatalf("request: %v canceled=%v", err, ctx.Err())
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	resolveErr := store.ResolveStop(accepted.JobID, accepted.AttemptID, "confirmed")
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if resolveErr == nil {
		t.Fatal("pre-replacement status save unexpectedly succeeded")
	}
	saved, err := store.ReadSaved()
	if err != nil || saved.Accepted == nil || saved.Accepted.StopRequest.Status != "requested" {
		t.Fatalf("failed status save changed disk: state=%+v error=%v", saved, err)
	}
	if err := store.ResolveStop(accepted.JobID, accepted.AttemptID, "unverified"); err != nil {
		t.Fatal(err)
	}
	saved, err = store.ReadSaved()
	if err != nil || saved.Accepted == nil || saved.Accepted.StopRequest.Status != "confirmed" {
		t.Fatalf("resolution retry lost first clean evidence: state=%+v error=%v", saved, err)
	}
}
