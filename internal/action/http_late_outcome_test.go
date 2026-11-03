package action

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
)

type lateOutcomeFixture struct {
	owner     *Orchestrator
	store     *state.Store
	statePath string
	job       Job
	outcome   httpOutcome
	finish    func(error) error
}

func newLateOutcomeFixture(t *testing.T, pending bool) *lateOutcomeFixture {
	return newLateOutcomeFixtureForJob(t, testJob(command.Comment), pending)
}

func newLateOutcomeFixtureForJob(t *testing.T, job Job, pending bool) *lateOutcomeFixture {
	t.Helper()
	job.Identity.ExecutorID = "remote"
	f := newExecutionAdapterFixture(t, job)
	job = f.job
	statePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(statePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accepted := acceptedForJob(job)
	store.Data.Accepted = accepted
	store.Data.JobFailures[job.Identity.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	f.owner.Store, f.owner.ExecutorID, f.owner.remoteSelected, f.owner.assignmentRequired = store, "remote", true, true
	f.owner.Usage = &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	f.owner.ValidateAssignment = func(context.Context, monitor.Job) error { return nil }
	f.sink.assignmentContext = t.Context()
	f.sink.assignmentRequest = monitor.Job{Deadline: accepted.Deadline}
	if err := f.sink.BeforeAssignment(job); err != nil {
		t.Fatal(err)
	}
	if accepted.OutcomeBinding != httpBindingDigest(httpJobBinding(job)) {
		t.Fatal("assignment did not save the full outcome binding")
	}
	request := monitor.Job{ID: job.Identity.JobID, AttemptID: job.Identity.AttemptID,
		Repository:  config.Repository{Name: job.Identity.Repository, ID: job.Identity.RepositoryID, OwnerID: job.Identity.OwnerID},
		SubjectType: job.Identity.SubjectType, Number: job.Identity.Number,
		Source:  github.Item{Author: github.User{Login: job.Identity.Requester, ID: job.Identity.RequesterID}},
		Command: command.Command{Name: job.Command, Action: job.Action}}
	finish, err := f.owner.trackUsage(request)
	if err != nil {
		t.Fatal(err)
	}
	var calls []CallUsage
	if pending {
		if job.Critic != nil {
			f.saveReview(nil, nil, "")
		}
		call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1}
		if err := f.sink.BeforeCall(f.call(call)); err != nil {
			t.Fatal(err)
		}
		call.Tokens, call.UsageCoverage, call.Complete = usage.Tokens{Input: 11, Output: 2}, usage.Coverage{Scope: "invocation"}, true
		calls = []CallUsage{call}
	}
	decidedAt := time.Now().UTC()
	if err := store.WithLock(func() error {
		_, err := store.DecideTerminalLocked(job.Identity.JobID, job.Identity.AttemptID, "lost", decidedAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if store.Data.TerminalDecisions[job.Identity.JobID].OutcomeBinding != accepted.OutcomeBinding {
		t.Fatal("terminal decision lost the assignment binding")
	}
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	outcome := httpOutcome{Sequence: 3, Result: Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Answer: "late answer must remain private", Calls: calls,
		Diagnostics: []Diagnostic{{Code: "late_detail", Message: "private late detail"}}},
		Failure: &httpFailure{Message: "worker failed"}}
	return &lateOutcomeFixture{owner: f.owner, store: store, statePath: statePath, job: job, outcome: outcome, finish: finish}
}

func lateOutcomeEndpoint(t *testing.T, owner *Orchestrator) (*RemoteEndpoint, *http.Client, string) {
	t.Helper()
	certificate, leaf := testRemoteCertificate(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	const skill = "https://github.com/example/skills/tree/main/skills/not-installed"
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skill}}}
	owner.Commands = commands
	cfg := config.Config{Main: owner.Main, Adversarial: owner.Adversarial, Commands: commands,
		Repositories:   []config.Repository{{Name: "owner/repo", Actions: []string{"inspect"}}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:0", Token: token}}
	_, endpoint, err := owner.AttachRemoteWorker(cfg, certificate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	endpoint.server.ErrorLog = log.New(io.Discard, "", 0)
	origin := "https://" + endpoint.Addr().String()
	client, err := NewRemoteExecutionClient(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client.Transport.(*authenticatedExecutionTransport).TLSClientConfig.RootCAs = roots
	return endpoint, client, origin
}

func postLateOutcome(t *testing.T, client *http.Client, origin string, outcome httpOutcome) (int, httpAcknowledgement) {
	t.Helper()
	data, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/outcome", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var ack httpAcknowledgement
	if err := httpDecode(response.Body, httpControlBytes, &ack); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, ack
}

func waitLateError(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("late receipt or competing save did not finish")
		return nil
	}
}

func TestLateRemoteOutcomeAuthenticatedAfterTransactionAndRestart(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	if err := f.finish(errors.New("lost outcome")); err == nil {
		t.Fatal("final usage status lost action error")
	}
	endpoint, client, origin := lateOutcomeEndpoint(t, f.owner)
	status, ack := postLateOutcome(t, client, origin, f.outcome)
	if status != http.StatusOK || ack.Failure != nil || ack.Sequence != f.outcome.Sequence || ack.Kind != "outcome" {
		t.Fatalf("late outcome ACK: status=%d ack=%+v", status, ack)
	}
	diagnosticPath := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
	data, err := os.ReadFile(diagnosticPath)
	if err != nil || !bytes.Contains(data, []byte("private late detail")) {
		t.Fatalf("late private diagnostic missing: %q error=%v", data, err)
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil ||
		record.Tokens != f.outcome.Result.Calls[0].Tokens || record.Status != "failed" {
		t.Fatalf("late usage receipt missing or changed status: %+v found=%t err=%v", record, found, err)
	}
	if f.store.Data.Accepted != nil || f.store.Data.TerminalDecisions[f.job.Identity.JobID].Outcome != "lost" {
		t.Fatal("late outcome reopened the terminal action")
	}
	if saved, err := f.store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("late answer became publishable: saved=%t error=%v", saved, err)
	}
	foreign := f.outcome
	foreign.Result.Identity.RepositoryID++
	if status, _ := postLateOutcome(t, client, origin, foreign); status != http.StatusConflict {
		t.Fatalf("foreign binding status=%d", status)
	}
	unknown := f.outcome
	unknown.Result.Identity.JobID = "unknown"
	if status, _ := postLateOutcome(t, client, origin, unknown); status != http.StatusConflict {
		t.Fatalf("unknown attempt status=%d", status)
	}
	conflict := f.outcome
	conflict.Result.Calls = append([]CallUsage(nil), f.outcome.Result.Calls...)
	conflict.Result.Calls[0].Tokens.Input++
	if status, _ := postLateOutcome(t, client, origin, conflict); status != http.StatusConflict {
		t.Fatalf("conflicting usage replay status=%d", status)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(f.statePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := *f.owner
	restarted.Executor, restarted.Store = nil, reopened
	f.owner = &restarted
	_, client, origin = lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("post-restart replay status=%d", status)
	}
	again, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(record, again) {
		t.Fatalf("post-restart usage duplicated: before=%+v after=%+v found=%t err=%v", record, again, found, err)
	}
	decision := reopened.Data.TerminalDecisions[f.job.Identity.JobID]
	decision.DecidedAt = time.Now().UTC().Add(-privateRecordRetention)
	reopened.Data.TerminalDecisions[f.job.Identity.JobID] = decision
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.PrunePrivateRecords(t.Context(), reopened, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusConflict {
		t.Fatalf("expired outcome status=%d", status)
	}
	if _, err := os.Stat(diagnosticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired private diagnostic recreated: %v", err)
	}
}

func TestLateRemoteOutcomeRequiresSavedPendingCall(t *testing.T) {
	f := newLateOutcomeFixture(t, false)
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	f.outcome.Result.Calls = []CallUsage{{Provider: "codex", Role: roleMain, Round: 1,
		Tokens: usage.Tokens{Input: 11}, UsageCoverage: usage.Coverage{Scope: "invocation"}, Complete: true}}
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusConflict {
		t.Fatalf("unsaved call usage status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsaved call result wrote diagnostics: %v", err)
	}
	f.outcome.Result.Calls = nil
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("no-call diagnostic status=%d", status)
	}
}

func TestLateRemoteOutcomeAcceptsCompletedPrefixBeforeLostCallStartACK(t *testing.T) {
	f := newLateOutcomeFixtureForJob(t, withClaudeCritic(testJob(command.Comment)), true)
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found {
		t.Fatalf("load saved call start: found=%t error=%v", found, err)
	}
	first := usageCall(1, f.outcome.Result.Calls[0])
	finished := time.Now().UTC()
	first.FinishedAt = &finished
	if _, err := record.FinishCall(first); err != nil {
		t.Fatal(err)
	}
	second := usage.Call{Ordinal: 2, Provider: "claude", Model: "default", Role: roleCritic,
		Round: 1, ReviewPhase: reviewVerification, StartedAt: finished}
	if _, err := record.BeginCall(second); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Usage.Save(record); err != nil {
		t.Fatal(err)
	}
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("completed prefix with trailing pending call status=%d", status)
	}
	after, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || len(after.Calls) != 2 || after.Calls[1].FinishedAt != nil || after.Tokens != first.Tokens {
		t.Fatalf("trailing pending call was changed: %+v found=%t error=%v", after, found, err)
	}
}

func TestLateRemoteOutcomeConfirmsTerminalDecisionBeforeRetention(t *testing.T) {
	f := newLateOutcomeFixture(t, false)
	decision := f.store.Data.TerminalDecisions[f.job.Identity.JobID]
	delete(f.store.Data.TerminalDecisions, f.job.Identity.JobID)
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	f.store.Data.TerminalDecisions[f.job.Identity.JobID] = decision // Simulate a failed pre-replacement decision save.
	priorCursor := f.store.Data.Cursor
	f.store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := f.owner.receiveLateOutcome("remote", f.outcome); err == nil {
		t.Fatal("unconfirmed terminal decision admitted a late receipt")
	}
	diagnosticPath := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
	if _, err := os.Stat(diagnosticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconfirmed decision retained diagnostics: %v", err)
	}
	f.store.Data.Cursor = priorCursor
	if err := f.owner.receiveLateOutcome("remote", f.outcome); err != nil {
		t.Fatalf("durable decision confirmation failed: %v", err)
	}
}

func TestLateRemoteOutcomeReplaysParentCaptureLimitDiagnostic(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	capped := withCaptureLimitDiagnostic(f.outcome.Result)
	if err := f.owner.retainWorkerDiagnostics(f.job, capped, f.outcome.Failure.workerErr()); err != nil {
		t.Fatal(err)
	}
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("raw replay after parent capture marker status=%d", status)
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens {
		t.Fatalf("capture marker replay lost usage: %+v found=%t error=%v", record, found, err)
	}
}

func TestLateRemoteOutcomeKeepsUsageWhenDiagnosticsConflict(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	prior := f.outcome.Result
	prior.Diagnostics = []Diagnostic{{Code: "late_detail", Message: "different private detail"}}
	if err := f.owner.retainWorkerDiagnostics(f.job, prior, f.outcome.Failure.workerErr()); err != nil {
		t.Fatal(err)
	}
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusConflict {
		t.Fatalf("conflicting diagnostic status=%d", status)
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens {
		t.Fatalf("diagnostic conflict erased valid usage: %+v found=%t error=%v", record, found, err)
	}
}

func TestLateRemoteOutcomeWhileNewerTransactionIsActive(t *testing.T) {
	old := newLateOutcomeFixture(t, true)
	newJob := testJob(command.Comment)
	newJob.Identity.JobID = "new-job"
	newJob.Identity.AttemptID = strings.Repeat("b", 32)
	newJob.Identity.ExecutorID = "remote"
	current := newHTTPServerTest(t, t.Context(), newJob, runnerExecutionCheckpoints{}, nil)
	current.executor.lateOutcome = func(outcome httpOutcome) error { return old.owner.receiveLateOutcome("remote", outcome) }
	current.pull(t)
	current.tx.mu.Lock()
	current.tx.stopUntil = time.Now().Add(-time.Second)
	current.tx.mu.Unlock()
	current.ack(t, "/outcome", old.outcome, http.StatusOK, old.outcome.Sequence)
	current.tx.mu.Lock()
	sequence, fenced := current.tx.sequence, current.tx.fenced
	current.tx.stopUntil = time.Time{}
	current.tx.mu.Unlock()
	if sequence != 0 || fenced {
		t.Fatalf("older receipt changed active transaction: sequence=%d fenced=%t", sequence, fenced)
	}
	current.ack(t, "/outcome", current.outcome(1, nil), http.StatusOK, 1)
	if got := current.result(t); got.err != nil {
		t.Fatalf("newer transaction failed after older receipt: %v", got.err)
	}
}

func TestLateRemoteOutcomeAcrossFencedTransactionHandoff(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	current := newHTTPServerTest(t, t.Context(), f.job, runnerExecutionCheckpoints{}, nil)
	current.executor.lateOutcome = func(outcome httpOutcome) error { return f.owner.receiveLateOutcome("remote", outcome) }
	current.pull(t)
	current.tx.mu.Lock()
	current.tx.fenced = true
	current.tx.mu.Unlock()
	current.ack(t, "/outcome", f.outcome, http.StatusOK, f.outcome.Sequence)
	current.tx.mu.Lock()
	sequence := current.tx.sequence
	current.tx.mu.Unlock()
	if sequence != 0 {
		t.Fatalf("fenced transaction consumed late sequence %d", sequence)
	}
}

func TestLateRemoteUsageReloadsAfterAmbiguousSave(t *testing.T) {
	for _, afterReplacement := range []bool{false, true} {
		name := "before replacement"
		if afterReplacement {
			name = "after replacement"
		}
		t.Run(name, func(t *testing.T) {
			f := newLateOutcomeFixture(t, true)
			_, client, origin := lateOutcomeEndpoint(t, f.owner)
			calls := 0
			f.owner.lateUsageSave = func(record usage.Record) error {
				calls++
				if calls == 1 {
					if afterReplacement {
						if err := f.owner.Usage.Save(record); err != nil {
							return err
						}
					}
					return errors.New("injected ambiguous usage save")
				}
				return f.owner.Usage.Save(record)
			}
			if status, ack := postLateOutcome(t, client, origin, f.outcome); status != http.StatusInternalServerError ||
				ack.Failure == nil || strings.Contains(ack.Failure.Message, "injected") {
				t.Fatalf("failed save ACK exposed storage details: status=%d ack=%+v", status, ack)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("late retry after ambiguous save status=%d", status)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("deduplicated replay status=%d", status)
			}
			if calls != 3 {
				t.Fatalf("retry failed to resync usage after replacement: saves=%d", calls)
			}
			record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens ||
				len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil {
				t.Fatalf("ambiguous retry usage: %+v found=%t err=%v", record, found, err)
			}
		})
	}
}

func TestLateRemoteUsageSurvivesFinalStatusSaveOverlap(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	f.owner.lateUsageSave = func(record usage.Record) error {
		close(entered)
		<-release
		return f.owner.Usage.Save(record)
	}
	lateDone := make(chan error, 1)
	go func() { lateDone <- f.owner.receiveLateOutcome("remote", f.outcome) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("late usage save did not start")
	}
	finishDone := make(chan error, 1)
	finishStarted := make(chan struct{})
	go func() {
		close(finishStarted)
		finishDone <- f.finish(errors.New("lost outcome"))
	}()
	<-finishStarted
	select {
	case err := <-finishDone:
		t.Fatalf("final usage save bypassed the held late receipt: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := waitLateError(t, lateDone); err != nil {
		t.Fatal(err)
	}
	if err := waitLateError(t, finishDone); err == nil {
		t.Fatal("final usage status lost original action error")
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Status != "failed" || record.Tokens != f.outcome.Result.Calls[0].Tokens ||
		len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil {
		t.Fatalf("final usage overwrite erased late receipt: %+v found=%t err=%v", record, found, err)
	}
}

func TestLateRemoteOutcomeDoesNotInvertStateAndPrivateLocks(t *testing.T) {
	f := newLateOutcomeFixture(t, false)
	stateHeld, release := make(chan struct{}), make(chan struct{})
	terminalDone := make(chan error, 1)
	go func() {
		terminalDone <- f.store.WithLock(func() error {
			close(stateHeld)
			<-release
			f.owner.CompleteTerminalUsage(monitor.Job{ID: f.job.Identity.JobID, AttemptID: f.job.Identity.AttemptID}, "lost", time.Now())
			return nil
		})
	}()
	<-stateHeld
	lateDone := make(chan error, 1)
	go func() { lateDone <- f.owner.receiveLateOutcome("remote", f.outcome) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := waitLateError(t, terminalDone); err != nil {
		t.Fatal(err)
	}
	if err := waitLateError(t, lateDone); err != nil {
		t.Fatal(err)
	}
}

func TestLateRemoteReceiptCannotRecreatePrunedDiagnostics(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	f.owner.lateUsageSave = func(record usage.Record) error {
		close(entered)
		<-release
		return f.owner.Usage.Save(record)
	}
	lateDone := make(chan error, 1)
	go func() { lateDone <- f.owner.receiveLateOutcome("remote", f.outcome) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("late receipt did not enter its save")
	}
	diagnosticPath := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
	if _, err := os.Stat(diagnosticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostic was saved before the pruning barrier: %v", err)
	}
	if err := f.store.WithLock(func() error {
		decision := f.store.Data.TerminalDecisions[f.job.Identity.JobID]
		decision.DecidedAt = time.Now().UTC().Add(-privateRecordRetention)
		f.store.Data.TerminalDecisions[f.job.Identity.JobID] = decision
		return f.store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	pruneDone := make(chan error, 1)
	go func() { pruneDone <- f.owner.PrunePrivateRecords(t.Context(), f.store, time.Now().UTC()) }()
	close(release)
	if err := waitLateError(t, lateDone); err != nil {
		t.Fatal(err)
	}
	if err := waitLateError(t, pruneDone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diagnosticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pruned diagnostic was recreated: %v", err)
	}
	if err := f.owner.receiveLateOutcome("remote", f.outcome); !errors.Is(err, errLateOutcomeRejected) {
		t.Fatalf("expired retry was admitted: %v", err)
	}
}
