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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

type lateOutcomeFixture struct {
	owner     *Orchestrator
	store     *state.Store
	statePath string
	job       Job
	outcome   httpOutcome
	finish    func(error) error
}

type outcomeReadBarrier struct {
	io.ReadCloser
	entered chan struct{}
	release <-chan struct{}
	first   bool
}

type outcomeEOFSignal struct {
	io.ReadCloser
	decoded chan struct{}
	once    sync.Once
}

func (s *outcomeEOFSignal) Read(data []byte) (int, error) {
	n, err := s.ReadCloser.Read(data)
	if errors.Is(err, io.EOF) {
		s.once.Do(func() { close(s.decoded) })
	}
	return n, err
}

func (b *outcomeReadBarrier) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if !b.first {
		b.first = true
		n, err := b.ReadCloser.Read(data[:1])
		close(b.entered) // The server has acquired its read lane and begun decoding.
		return n, err
	}
	<-b.release
	return b.ReadCloser.Read(data)
}

func sendHTTPOutcome(ctx context.Context, client *http.Client, url string, outcome httpOutcome, hint, stall string) httpServerTestExchange {
	data, err := json.Marshal(outcome)
	if err != nil {
		return httpServerTestExchange{err: err}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return httpServerTestExchange{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	if hint != "" {
		request.Header.Set(httpOutcomeBindingHeader, hint)
	}
	if stall != "" {
		request.Header.Set("X-Test-Stall", stall)
	}
	response, err := client.Do(request)
	if err != nil {
		return httpServerTestExchange{err: err}
	}
	defer response.Body.Close()
	result := httpServerTestExchange{status: response.StatusCode}
	result.err = httpDecode(response.Body, httpControlBytes, &result.ack)
	return result
}

func newLateOutcomeFixture(t *testing.T, pending bool) *lateOutcomeFixture {
	return newLateOutcomeFixtureForJob(t, testJob(command.Comment), pending)
}

func newLateOutcomeFixtureForJob(t *testing.T, job Job, pending bool) *lateOutcomeFixture {
	t.Helper()
	resolveAcceptedFixtureRoles(&job)
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
	store.Data.JobFailures[job.Identity.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
		MainProvider: accepted.MainProvider, CriticProvider: accepted.CriticProvider,
		MainSettings: accepted.MainSettings, CriticSettings: accepted.CriticSettings}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	f.owner.Store, f.owner.ExecutorID, f.owner.remoteSelected, f.owner.assignmentRequired = store, "remote", true, true
	f.owner.remoteAttachedAt = time.Now().UTC() // This fixture's remote owner predates the terminal decision.
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
		call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Model: job.Main.Model, Effort: job.Main.Effort}
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
	owner.CodexModel, owner.CodexEffort = "fixture-main", "high"
	owner.ClaudeModel, owner.ClaudeEffort = "fixture-critic", "low"
	cfg := config.Config{Main: owner.Main, Adversarial: owner.Adversarial, Commands: commands,
		Codex:          config.CodexConfig{Model: owner.CodexModel, ReasoningEffort: owner.CodexEffort},
		Claude:         config.ClaudeConfig{Model: owner.ClaudeModel, Effort: owner.ClaudeEffort},
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := sendHTTPOutcome(ctx, client, origin+"/outcome", outcome, "", "")
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.status, got.ack
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

func TestLateRemoteOutcomeRejectsRotatedProviderKeys(t *testing.T) {
	const oldCodex, currentCodex, candidateCodex = "fixture-old-codex-key", "fixture-current-codex-key", "fixture-candidate-codex-key"
	const oldClaude, currentClaude, candidateClaude = "fixture-old-claude-key", "fixture-current-claude-key", "fixture-candidate-claude-key"
	for _, key := range []string{oldCodex, currentCodex, candidateCodex, oldClaude, currentClaude, candidateClaude} {
		t.Run(key, func(t *testing.T) {
			f := newLateOutcomeFixture(t, false)
			f.owner.SetRunnerCredentials(config.CodexConfig{APIKey: oldCodex}, config.ClaudeConfig{APIKey: oldClaude})
			f.owner.SetRunnerCredentials(config.CodexConfig{APIKey: currentCodex}, config.ClaudeConfig{APIKey: currentClaude})
			f.owner.SetRunnerCredentials(config.CodexConfig{APIKey: candidateCodex}, config.ClaudeConfig{APIKey: candidateClaude})
			f.owner.SetRunnerCredentials(config.CodexConfig{APIKey: currentCodex}, config.ClaudeConfig{APIKey: currentClaude}) // Failed preflight rolls back.
			if got := redactActionText("late "+key, f.owner.trustedSecrets()...); got != "late [redacted]" {
				t.Fatalf("rotated credential was not trusted: %q", got)
			}
			f.outcome.Result.Diagnostics[0].Message = "late " + key
			if err := f.owner.receiveLateOutcome("remote", f.outcome); !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), key) {
				t.Fatalf("late diagnostic with a provider credential was accepted: %v", err)
			}
			path := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("late diagnostic with a provider credential was retained: %v", err)
			}
		})
	}
}

func TestRunnerCredentialRotationConcurrentWithTrustedSecrets(t *testing.T) {
	owner := &Orchestrator{}
	old := config.CodexConfig{APIKey: "fixture-old-provider-key"}
	current := config.CodexConfig{APIKey: "fixture-current-provider-key"}
	owner.SetRunnerCredentials(old, config.ClaudeConfig{})
	owner.SetRunnerCredentials(current, config.ClaudeConfig{})
	owner.SetRunnerCredentials(old, config.ClaudeConfig{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			owner.SetRunnerCredentials(current, config.ClaudeConfig{})
			owner.SetRunnerCredentials(old, config.ClaudeConfig{})
		}
	}()
	for range 100 {
		secrets := owner.trustedSecrets()
		for _, key := range []string{old.APIKey, current.APIKey} {
			if got := redactActionText(key, secrets...); got != "[redacted]" {
				t.Fatalf("concurrent rotation lost provider credential: %q", got)
			}
		}
	}
	<-done
}

func TestLateRemoteOutcomeAuthenticatedAfterTransactionAndRestart(t *testing.T) {
	job := testJob(command.Comment)
	job.Main.Provider = "claude"
	f := newLateOutcomeFixtureForJob(t, job, true)
	estimate := 0.25
	f.outcome.Result.Calls[0].EstimatedCostUSD = &estimate
	f.outcome.Result.Calls[0].EstimatedCostComplete = true
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
		record.Tokens != f.outcome.Result.Calls[0].Tokens || record.Status != "failed" ||
		record.Calls[0].EstimatedCostUSD == nil || *record.Calls[0].EstimatedCostUSD != estimate ||
		!record.Calls[0].EstimatedCostComplete {
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
	restarted.Executor, restarted.Store, restarted.remoteAttachedAt = nil, reopened, time.Time{}
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
	retained, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(record, retained) {
		t.Fatalf("artifact retention changed estimated cost: before=%+v after=%+v found=%t err=%v", record, retained, found, err)
	}
}

func TestLateRemoteOutcomeExpiryKeepsPendingBillingUnchanged(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	decision := f.store.Data.TerminalDecisions[f.job.Identity.JobID]
	decision.DecidedAt = time.Now().UTC().Add(-privateRecordRetention)
	f.store.Data.TerminalDecisions[f.job.Identity.JobID] = decision
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	before, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || len(before.Calls) != 1 || before.Calls[0].FinishedAt != nil {
		t.Fatalf("expected pending usage before expiry: %+v found=%t error=%v", before, found, err)
	}
	beforeTotals, err := f.owner.Usage.Query(usage.Filter{}, nil, false)
	if err != nil || beforeTotals.Totals.Attempts != 1 {
		t.Fatalf("usage totals before expiry: %+v error=%v", beforeTotals.Totals, err)
	}
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusConflict {
		t.Fatalf("expired pending receipt status=%d", status)
	}
	after, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatalf("expired receipt changed pending usage: before=%+v after=%+v found=%t error=%v", before, after, found, err)
	}
	afterTotals, err := f.owner.Usage.Query(usage.Filter{}, nil, false)
	if err != nil || !reflect.DeepEqual(beforeTotals.Totals, afterTotals.Totals) {
		t.Fatalf("expired receipt changed billing totals: before=%+v after=%+v error=%v", beforeTotals.Totals, afterTotals.Totals, err)
	}
	if _, err := os.Stat(filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired receipt created private diagnostics: %v", err)
	}
}

func TestLateSuccessfulOutcomeDoesNotPublish(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	f.outcome.Failure = nil
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("successful late outcome status=%d", status)
	}
	if data, err := os.ReadFile(filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")); err != nil ||
		!bytes.Contains(data, []byte("private late detail")) {
		t.Fatalf("successful late outcome lost private facts: %q error=%v", data, err)
	}
	if result, err := f.store.HasAcceptedResult(); err != nil || result || f.store.Data.Accepted != nil ||
		f.store.Data.TerminalDecisions[f.job.Identity.JobID].Outcome != "lost" {
		t.Fatalf("successful late outcome reopened publication: result=%t accepted=%+v error=%v", result, f.store.Data.Accepted, err)
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Status != "failed" || record.FinishedAt == nil {
		t.Fatalf("worker success overrode durable terminal usage status: %+v found=%t error=%v", record, found, err)
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
	if err := f.owner.receiveLateOutcome("remote", f.outcome); err == nil {
		t.Fatal("unconfirmed terminal decision admitted a late receipt")
	}
	diagnosticPath := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
	if _, err := os.Stat(diagnosticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconfirmed decision retained diagnostics: %v", err)
	}
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	delete(f.store.Data.TerminalDecisions, f.job.Identity.JobID) // Simulate stale memory after a replaced state file.
	f.store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := f.owner.receiveLateOutcome("remote", f.outcome); err != nil {
		t.Fatalf("saved decision admission depended on mutable state: %v", err)
	}
}

func TestLateRemoteOutcomeRetriesUntilTerminalDecisionIsSaved(t *testing.T) {
	f := newLateOutcomeFixture(t, false)
	accepted := acceptedForJob(f.job)
	accepted.Assigned = true
	accepted.OutcomeBinding = httpBindingDigest(httpJobBinding(f.job))
	f.store.Data.Accepted = accepted
	delete(f.store.Data.TerminalDecisions, f.job.Identity.JobID)
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	priorCursor := f.store.Data.Cursor
	f.store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := f.store.WithLock(func() error {
		_, err := f.store.DecideTerminalLocked(f.job.Identity.JobID, f.job.Identity.AttemptID, "lost", time.Now())
		return err
	}); err == nil {
		t.Fatal("pre-replacement terminal decision save unexpectedly succeeded")
	}
	f.store.Data.Cursor = priorCursor
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusInternalServerError {
		t.Fatalf("unconfirmed saved assignment was not retryable: status=%d", status)
	}
	diagnosticPath := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
	if _, err := os.Stat(diagnosticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconfirmed decision retained diagnostics: %v", err)
	}
	if err := f.store.WithLock(func() error {
		_, err := f.store.DecideTerminalLocked(f.job.Identity.JobID, f.job.Identity.AttemptID, "lost", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.store.Data.Accepted = nil
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("late retry after terminal decision save status=%d", status)
	}
	if data, err := os.ReadFile(diagnosticPath); err != nil || !bytes.Contains(data, []byte("private late detail")) {
		t.Fatalf("durable terminal retry lost diagnostics: %q error=%v", data, err)
	}
}

func TestLateRemoteOutcomeFirstReceiptAfterRestart(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	if err := f.finish(errors.New("lost outcome")); err == nil {
		t.Fatal("final usage status lost action error")
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
	restarted.Executor, restarted.Store, restarted.remoteAttachedAt = nil, reopened, time.Time{}
	_, client, origin := lateOutcomeEndpoint(t, &restarted)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("first late receipt after restart status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(restarted.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-restart worker text was retained: %v", err)
	}
	record, found, err := restarted.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Status != "failed" || record.Tokens != f.outcome.Result.Calls[0].Tokens ||
		len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil {
		t.Fatalf("post-restart usage missing: %+v found=%t err=%v", record, found, err)
	}
	if saved, err := reopened.HasAcceptedResult(); err != nil || saved || reopened.Data.Accepted != nil {
		t.Fatalf("post-restart late answer became publishable: saved=%t accepted=%+v error=%v", saved, reopened.Data.Accepted, err)
	}
}

func TestLateRemoteOutcomeAfterRestartCannotRetainRotatedKey(t *testing.T) {
	const oldKey = "fixture-retired-provider-key"
	f := newLateOutcomeFixture(t, true)
	f.owner.SetRunnerCredentials(config.CodexConfig{APIKey: oldKey}, config.ClaudeConfig{})
	f.owner.SetRunnerCredentials(config.CodexConfig{APIKey: "fixture-current-provider-key"}, config.ClaudeConfig{})
	if err := f.finish(errors.New("lost outcome")); err == nil {
		t.Fatal("final usage status lost action error")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(f.statePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	decision := reopened.Data.TerminalDecisions[f.job.Identity.JobID]
	decision.DecidedAt = time.Now().UTC().Add(time.Minute) // Simulate a clock rollback after the previous process exited.
	reopened.Data.TerminalDecisions[f.job.Identity.JobID] = decision
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	restarted := *f.owner
	restarted.Executor, restarted.Store, restarted.remoteAttachedAt, restarted.retiredProviderKeys = nil, reopened, time.Time{}, nil
	if got := redactActionText(oldKey, restarted.trustedSecrets()...); got != oldKey {
		t.Fatalf("restart retained the old key: %q", got)
	}
	f.outcome.Result.Diagnostics[0].Message = "late " + oldKey
	f.outcome.Failure.Message = "failed " + oldKey
	_, client, origin := lateOutcomeEndpoint(t, &restarted)
	if !decision.DecidedAt.After(restarted.remoteAttachedAt) {
		t.Fatalf("clock rollback was not simulated: decision=%s attach=%s", decision.DecidedAt, restarted.remoteAttachedAt)
	}
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("first late receipt after restart status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(restarted.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotated key reached worker diagnostics: %v", err)
	}
	record, found, err := restarted.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens || len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil {
		t.Fatalf("safe late usage was lost: record=%+v found=%t error=%v", record, found, err)
	}
	summary := reopened.Data.TerminalDecisions[f.job.Identity.JobID].Summary
	detail, found, err := restarted.UserActionDetail(f.job.Identity.AttemptID, time.Now(), summary)
	if err != nil || !found || strings.Contains(detail.WorkerFailure+detail.WorkerObservedFailure+detail.WorkerNotice, oldKey) {
		t.Fatalf("rotated key reached requester detail: detail=%+v found=%t error=%v", detail, found, err)
	}
	for _, diagnostic := range detail.Diagnostics {
		if strings.Contains(diagnostic.Text, oldKey) {
			t.Fatalf("rotated key reached requester diagnostic: %+v", diagnostic)
		}
	}
}

func TestLateRemoteOutcomeRetainsDiagnosticsWithoutUsageStore(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	usageStore := f.owner.Usage
	before, found, err := usageStore.Load(f.job.Identity.AttemptID)
	if err != nil || !found {
		t.Fatalf("load pending usage: found=%t error=%v", found, err)
	}
	f.owner.Usage = nil
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("optional usage disabled late diagnostics: status=%d", status)
	}
	data, err := os.ReadFile(filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json"))
	if err != nil || !bytes.Contains(data, []byte("private late detail")) {
		t.Fatalf("late diagnostic missing: %q error=%v", data, err)
	}
	after, found, err := usageStore.Load(f.job.Identity.AttemptID)
	if err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatalf("optional usage changed stored calls: before=%+v after=%+v found=%t error=%v", before, after, found, err)
	}
}

func TestLateRemoteOutcomeReplaysParentCaptureLimitDiagnostic(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	capped := withCaptureLimitDiagnostic(f.outcome.Result)
	if err := f.owner.retainWorkerDiagnostics(f.job, capped, f.outcome.Failure.workerErr()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, client, origin := lateOutcomeEndpoint(t, f.owner)
	if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
		t.Fatalf("raw replay after parent capture marker status=%d", status)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("parent capture marker changed on replay: before=%q after=%q error=%v", before, after, err)
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens {
		t.Fatalf("capture marker replay lost usage: %+v found=%t error=%v", record, found, err)
	}
}

func TestLateRemoteOutcomeAugmentsParentOnlyCaptureLimit(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failFirst        bool
		afterReplacement bool
	}{
		{name: "direct"},
		{name: "retry before replacement", failFirst: true},
		{name: "retry after replacement", failFirst: true, afterReplacement: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLateOutcomeFixture(t, true)
			parent := Result{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head}
			if err := f.owner.retainWorkerDiagnostics(f.job, withCaptureLimitDiagnostic(parent), errors.New("outcome lost")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var initial retainedWorkerDiagnostics
			if err := json.Unmarshal(before, &initial); err != nil || initial.WorkerReportedFailure != nil ||
				!reflect.DeepEqual(initial.WorkerReportedDiagnostics, []Diagnostic{{Code: "capture_limit", Message: captureLimitMessage}}) {
				t.Fatalf("parent did not save only its fixed marker: %+v error=%v", initial, err)
			}
			writes := 0
			if tc.failFirst {
				f.owner.lateDiagnosticSave = func(path string, data []byte, dirs ...string) error {
					writes++
					if writes == 1 {
						if tc.afterReplacement {
							if err := workspace.WriteAtomic(path, data, dirs...); err != nil {
								return err
							}
						}
						return errors.New("injected ambiguous diagnostic save")
					}
					return workspace.WriteAtomic(path, data, dirs...)
				}
			}
			_, client, origin := lateOutcomeEndpoint(t, f.owner)
			if tc.failFirst {
				if status, ack := postLateOutcome(t, client, origin, f.outcome); status != http.StatusInternalServerError ||
					ack.Failure == nil || strings.Contains(ack.Failure.Message, "injected") {
					t.Fatalf("ambiguous diagnostic save ACK: status=%d ack=%+v", status, ack)
				}
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("late parent-marker augmentation status=%d", status)
			}
			if tc.failFirst {
				want := 2
				if tc.afterReplacement {
					want = 1
				}
				if writes != want {
					t.Fatalf("ambiguous diagnostic retry wrote %d times, want %d", writes, want)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || bytes.Equal(before, after) {
				t.Fatalf("late worker facts did not augment parent marker: before=%q after=%q error=%v", before, after, err)
			}
			var retained retainedWorkerDiagnostics
			if err := json.Unmarshal(after, &retained); err != nil || retained.Identity != f.job.Identity ||
				retained.SourceRef != f.job.Source.Head || retained.WorkerReportedFailure == nil ||
				retained.WorkerReportedFailure.Message != f.outcome.Failure.Message ||
				!reflect.DeepEqual(retained.WorkerReportedDiagnostics, []Diagnostic{
					f.outcome.Result.Diagnostics[0], {Code: "capture_limit", Message: captureLimitMessage},
				}) {
				t.Fatalf("augmented worker facts: %+v error=%v", retained, err)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("identical augmented replay status=%d", status)
			}
			conflict := f.outcome
			conflict.Result.Diagnostics = []Diagnostic{{Code: "late_detail", Message: "different private detail"}}
			if status, _ := postLateOutcome(t, client, origin, conflict); status != http.StatusConflict {
				t.Fatalf("conflicting augmented replay status=%d", status)
			}
			stable, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(stable, after) {
				t.Fatalf("augmented record changed on replay: before=%q after=%q error=%v", after, stable, err)
			}
			record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens {
				t.Fatalf("marker replay lost usage: %+v found=%t error=%v", record, found, err)
			}
		})
	}
}

func TestLateRemoteOutcomeRetriesAmbiguousFirstDiagnosticWrite(t *testing.T) {
	for _, afterReplacement := range []bool{false, true} {
		name := "before replacement"
		if afterReplacement {
			name = "after replacement"
		}
		t.Run(name, func(t *testing.T) {
			f := newLateOutcomeFixture(t, true)
			path := filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json")
			writes := 0
			f.owner.lateDiagnosticSave = func(path string, data []byte, dirs ...string) error {
				writes++
				if writes == 1 {
					if afterReplacement {
						if err := workspace.WriteAtomic(path, data, dirs...); err != nil {
							return err
						}
					}
					return errors.New("injected ambiguous first diagnostic write")
				}
				return workspace.WriteAtomic(path, data, dirs...)
			}
			_, client, origin := lateOutcomeEndpoint(t, f.owner)
			if status, ack := postLateOutcome(t, client, origin, f.outcome); status != http.StatusInternalServerError ||
				ack.Failure == nil || strings.Contains(ack.Failure.Message, "injected") {
				t.Fatalf("ambiguous first diagnostic write ACK: status=%d ack=%+v", status, ack)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("retry after ambiguous first diagnostic write status=%d", status)
			}
			wantWrites := 2
			if afterReplacement {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("diagnostic retry wrote %d times, want %d", writes, wantWrites)
			}
			beforeReplay, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(beforeReplay, []byte("private late detail")) ||
				!bytes.Contains(beforeReplay, []byte("worker failed")) {
				t.Fatalf("first diagnostic write lost worker facts: %q error=%v", beforeReplay, err)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("deduplicated first-write replay status=%d", status)
			}
			afterReplay, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(beforeReplay, afterReplay) || writes != wantWrites {
				t.Fatalf("first-write replay changed diagnostics: before=%q after=%q writes=%d error=%v", beforeReplay, afterReplay, writes, err)
			}
		})
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

func TestLateRemoteOutcomeKeepsDiagnosticsWhenUsageStorageFails(t *testing.T) {
	for _, failure := range []string{"load", "save"} {
		t.Run(failure, func(t *testing.T) {
			f := newLateOutcomeFixture(t, true)
			prior, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found {
				t.Fatalf("load pending usage: found=%t error=%v", found, err)
			}
			if failure == "load" {
				path := filepath.Join(f.owner.Usage.Dir, f.job.Identity.AttemptID+".json")
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				f.owner.lateUsageSave = func(usage.Record) error { return errors.New("injected usage save failure") }
			}
			_, client, origin := lateOutcomeEndpoint(t, f.owner)
			if status, ack := postLateOutcome(t, client, origin, f.outcome); status != http.StatusInternalServerError ||
				ack.Failure == nil || strings.Contains(ack.Failure.Message, "injected") {
				t.Fatalf("failed usage storage ACK: status=%d ack=%+v", status, ack)
			}
			data, err := os.ReadFile(filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json"))
			if err != nil || !bytes.Contains(data, []byte("private late detail")) || !bytes.Contains(data, []byte("worker failed")) {
				t.Fatalf("usage storage failure lost diagnostics: %q error=%v", data, err)
			}
			if failure == "save" {
				after, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
				if err != nil || !found || !reflect.DeepEqual(prior, after) {
					t.Fatalf("failed usage save changed durable record: before=%+v after=%+v found=%t error=%v", prior, after, found, err)
				}
			}
		})
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	active := sendHTTPOutcome(ctx, current.server.Client(), current.server.URL+"/outcome", current.outcome(1, nil),
		httpBindingDigest(httpJobBinding(newJob)), "")
	if active.err != nil || active.status != http.StatusOK || active.ack.Failure != nil {
		t.Fatalf("newer current outcome: %+v", active)
	}
	if got := current.result(t); got.err != nil {
		t.Fatalf("newer transaction failed after older receipt: %v", got.err)
	}
}

func TestLateRemoteOutcomeAcrossFencedTransactionHandoff(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	decision := f.store.Data.TerminalDecisions[f.job.Identity.JobID]
	delete(f.store.Data.TerminalDecisions, f.job.Identity.JobID)
	accepted := acceptedForJob(f.job)
	accepted.Assigned = true
	accepted.OutcomeBinding = httpBindingDigest(httpJobBinding(f.job))
	f.store.Data.Accepted = accepted
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	readStarts := make(chan chan struct{}, 2)
	released := make(chan struct{})
	close(released)
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				r.Body = &outcomeReadBarrier{ReadCloser: r.Body, entered: <-readStarts, release: released}
			}
			next.ServeHTTP(w, r)
		})
	}
	current := newHTTPServerTest(t, t.Context(), f.job, runnerExecutionCheckpoints{}, wrap)
	current.executor.lateOutcome = func(outcome httpOutcome) error { return f.owner.receiveLateOutcome("remote", outcome) }
	current.pull(t)
	current.tx.mu.Lock()
	current.tx.fenced = true
	current.tx.stopUntil = time.Now().Add(-time.Second)
	current.tx.mu.Unlock()
	send := func() httpServerTestExchange {
		data, err := json.Marshal(f.outcome)
		if err != nil {
			return httpServerTestExchange{err: err}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		reader, writer := io.Pipe()
		entered := make(chan struct{})
		readStarts <- entered
		written := make(chan struct{})
		go func() {
			defer close(written)
			defer writer.Close()
			if _, err := writer.Write(data[:1]); err != nil {
				return
			}
			select {
			case <-entered:
			case <-ctx.Done():
				return
			}
			_, _ = writer.Write(data[1:])
		}()
		defer func() { _ = reader.Close(); cancel(); <-written }()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, current.server.URL+"/outcome", reader)
		if err != nil {
			return httpServerTestExchange{err: err}
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(httpOutcomeBindingHeader, httpBindingDigest(httpJobBinding(f.job)))
		response, err := current.server.Client().Do(request)
		if err != nil {
			return httpServerTestExchange{err: err}
		}
		defer response.Body.Close()
		got := httpServerTestExchange{status: response.StatusCode}
		got.err = httpDecode(response.Body, httpControlBytes, &got.ack)
		return got
	}
	if got := send(); got.err != nil || got.status != http.StatusInternalServerError || got.ack.Failure == nil {
		t.Fatalf("fenced hinted outcome before terminal save was not retryable: %+v", got)
	}
	f.store.Data.TerminalDecisions[f.job.Identity.JobID] = decision
	f.store.Data.Accepted = nil
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	got := send()
	if got.err != nil || got.status != http.StatusOK || got.ack.Failure != nil || got.ack.Sequence != f.outcome.Sequence {
		t.Fatalf("fenced hinted outcome did not reach late receiver: %+v", got)
	}
	current.tx.mu.Lock()
	sequence := current.tx.sequence
	current.tx.mu.Unlock()
	if sequence != 0 {
		t.Fatalf("fenced transaction consumed late sequence %d", sequence)
	}
}

func TestLateRemoteOutcomeAfterDurableStopBeforeTransactionFence(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	delete(f.store.Data.TerminalDecisions, f.job.Identity.JobID)
	accepted := acceptedForJob(f.job)
	accepted.Assigned = true
	accepted.OutcomeBinding = httpBindingDigest(httpJobBinding(f.job))
	f.store.Data.Accepted = accepted
	if err := f.store.Save(); err != nil {
		t.Fatal(err)
	}
	decoded := make(chan struct{})
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				r.Body = &outcomeEOFSignal{ReadCloser: r.Body, decoded: decoded}
			}
			next.ServeHTTP(w, r)
		})
	}
	current := newHTTPServerTest(t, t.Context(), f.job, runnerExecutionCheckpoints{}, wrap)
	current.executor.lateOutcome = func(outcome httpOutcome) error { return f.owner.receiveLateOutcome("remote", outcome) }
	current.pull(t)
	current.tx.mutation.Lock()
	releaseMutation := sync.OnceFunc(current.tx.mutation.Unlock)
	defer releaseMutation()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan httpServerTestExchange, 1)
	go func() {
		done <- sendHTTPOutcome(ctx, current.server.Client(), current.server.URL+"/outcome", f.outcome,
			httpBindingDigest(httpJobBinding(f.job)), "")
	}()
	select {
	case <-decoded:
	case <-ctx.Done():
		t.Fatal("current Outcome did not finish decoding before stop")
	}
	// Monitor's assigned-stop path saves this decision before Execute has fenced the transaction.
	if err := f.store.WithLock(func() error {
		_, err := f.store.DecideTerminalLocked(f.job.Identity.JobID, f.job.Identity.AttemptID, "expired", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	current.tx.mu.Lock()
	current.tx.stopUntil = time.Now().Add(-time.Second)
	current.tx.mu.Unlock()
	releaseMutation()
	select {
	case got := <-done:
		if got.err != nil || got.status != http.StatusOK || got.ack.Failure != nil || got.ack.Sequence != f.outcome.Sequence {
			t.Fatalf("decoded Outcome did not reach durable late receiver: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("decoded Outcome did not finish after durable stop")
	}
	current.tx.mu.Lock()
	sequence, fenced := current.tx.sequence, current.tx.fenced
	current.tx.mu.Unlock()
	if sequence != 0 || fenced {
		t.Fatalf("late Outcome changed active transaction: sequence=%d fenced=%t", sequence, fenced)
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil ||
		record.Tokens != f.outcome.Result.Calls[0].Tokens {
		t.Fatalf("durable stop lost pending usage receipt: %+v found=%t error=%v", record, found, err)
	}
	data, err := os.ReadFile(filepath.Join(f.owner.WorkRoot, "worker-diagnostics", f.job.Identity.AttemptID+".json"))
	if err != nil || !bytes.Contains(data, []byte("private late detail")) {
		t.Fatalf("durable stop lost late diagnostics: %q error=%v", data, err)
	}
}

func TestRemoteOutcomeStopAtSequenceAdmissionLeavesSequenceForLateReceipt(t *testing.T) {
	tx := &httpTransaction{binding: httpBinding{Version: ProtocolVersion}, delivered: true,
		stopUntil: time.Now().Add(-time.Second)}
	gap, err := tx.enter(tx.binding, 3, "outcome", false, true)
	if gap || !errors.Is(err, errLateOutcomeAfterStop) || tx.sequence != 0 {
		t.Fatalf("expired remote Outcome consumed active sequence: gap=%t error=%v sequence=%d", gap, err, tx.sequence)
	}
}

func TestNewOutcomePassesStalledPriorActiveUpload(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" && r.Header.Get("X-Test-Stall") == "old" {
				r.Body = &outcomeReadBarrier{ReadCloser: r.Body, entered: entered, release: release}
			}
			next.ServeHTTP(w, r)
		})
	}
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, wrap)
	f.executor.lateOutcome = func(httpOutcome) error { return nil }
	f.pull(t)
	oldTx := f.tx
	oldDone := make(chan httpServerTestExchange, 1)
	oldCtx, cancelOld := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelOld()
	go func() {
		oldDone <- sendHTTPOutcome(oldCtx, f.server.Client(), f.server.URL+"/outcome", f.outcome(1, nil), "", "old")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old active upload did not enter its read lane")
	}
	// The current worker can finish while an older headerless upload is decoding.
	firstCtx, cancelFirst := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelFirst()
	first := sendHTTPOutcome(firstCtx, f.server.Client(), f.server.URL+"/outcome", f.outcome(1, nil),
		httpBindingDigest(httpJobBinding(f.job)), "")
	if first.err != nil || first.status != http.StatusOK || first.ack.Failure != nil {
		t.Fatalf("first current outcome: %+v", first)
	}
	if got := f.result(t); got.err != nil {
		t.Fatalf("first transaction failed: %v", got.err)
	}
	newJob := f.job
	newJob.Identity.JobID = "new-job"
	newJob.Identity.AttemptID = strings.Repeat("b", 32)
	newDone := make(chan httpServerTestResult, 1)
	go func() {
		result, err := f.executor.Execute(t.Context(), newJob, runnerExecutionCheckpoints{})
		newDone <- httpServerTestResult{result: result, err: err}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		f.executor.mu.Lock()
		tx, changed := f.executor.active, f.executor.changed
		f.executor.mu.Unlock()
		if tx != nil && tx != oldTx {
			tx.mu.Lock()
			ready := tx.ready
			tx.mu.Unlock()
			if ready {
				f.job, f.tx, f.done = newJob, tx, newDone
				break
			}
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("new transaction did not become ready")
		}
	}
	f.pull(t)
	currentCtx, cancelCurrent := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCurrent()
	currentDone := make(chan httpServerTestExchange, 1)
	go func() {
		currentDone <- sendHTTPOutcome(currentCtx, f.server.Client(), f.server.URL+"/outcome", f.outcome(1, nil),
			httpBindingDigest(httpJobBinding(newJob)), "")
	}()
	var current httpServerTestExchange
	select {
	case current = <-currentDone:
	case <-currentCtx.Done():
		close(release)
		t.Fatal("new active upload waited for the stalled old read")
	}
	if current.err != nil || current.status != http.StatusOK || current.ack.Failure != nil {
		t.Fatalf("new active upload waited for old read: %+v", current)
	}
	if got := f.result(t); got.err != nil {
		t.Fatalf("new transaction failed: %v", got.err)
	}
	close(release)
	var old httpServerTestExchange
	select {
	case old = <-oldDone:
	case <-oldCtx.Done():
		t.Fatal("old upload did not finish after its read barrier was released")
	}
	if old.err != nil || old.status != http.StatusOK || old.ack.Failure != nil {
		t.Fatalf("old fenced upload failed late receipt: %+v", old)
	}
}

func TestQueuedCurrentOutcomeMovesBehindLateReadAfterFence(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	old := &httpTransaction{}
	old.mu.Lock()
	old.outcomeRead.Lock()          // An old current upload holds its read lane.
	executor.lateOutcomeRead.Lock() // A separate late upload holds the late lane.
	executor.outcomeReads <- struct{}{}
	releaseOld := sync.OnceFunc(old.outcomeRead.Unlock)
	releaseLate := sync.OnceFunc(executor.lateOutcomeRead.Unlock)
	releaseState := sync.OnceFunc(old.mu.Unlock)
	done := make(chan struct{})
	defer func() {
		releaseState()
		releaseOld()
		releaseLate()
		close(done)
	}()
	type selection struct {
		lane    *sync.Mutex
		current bool
	}
	queued := make(chan selection, 1)
	go func() {
		lane, current := executor.lockOutcomeRead(old, true)
		queued <- selection{lane, current}
		<-done
		lane.Unlock()
	}()
	releaseOld()
	deadline := time.Now().Add(5 * time.Second)
	for old.outcomeRead.TryLock() {
		old.outcomeRead.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("queued old upload did not acquire its original read lane")
		}
		runtime.Gosched()
	}
	old.fenced = true // It closes while the queued upload waits for old.mu.
	releaseState()
	newer := &httpTransaction{}
	newSelected := make(chan selection, 1)
	go func() {
		lane, current := executor.lockOutcomeRead(newer, true)
		newSelected <- selection{lane, current}
		<-done
		lane.Unlock()
	}()
	var newRead selection
	select {
	case newRead = <-newSelected:
	case <-time.After(5 * time.Second):
		releaseLate()
		t.Fatal("new current upload waited behind the stalled late lane")
	}
	if !newRead.current || newRead.lane != &newer.outcomeRead {
		t.Fatal("new current upload lost its own read lane")
	}
	select {
	case executor.outcomeReads <- struct{}{}:
		<-executor.outcomeReads
	default:
		t.Fatal("stale queued upload consumed the newer Result's body slot")
	}
	releaseLate()
	select {
	case got := <-queued:
		if got.current || got.lane != &executor.lateOutcomeRead {
			t.Fatalf("stale queued upload retained current lane: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale queued upload did not move to the late lane")
	}
}

func TestRemoteCurrentOutcomeRequiresHeaderAfterHeaderlessLateUpload(t *testing.T) {
	old := newLateOutcomeFixture(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	currentEntered := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				switch r.Header.Get("X-Test-Stall") {
				case "old":
					r.Body = &outcomeReadBarrier{ReadCloser: r.Body, entered: entered, release: release}
				case "current":
					close(currentEntered)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	job := testJob(command.Comment)
	job.Identity.JobID = "new-job"
	job.Identity.AttemptID = strings.Repeat("b", 32)
	job.Identity.ExecutorID = "remote"
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, wrap)
	f.executor.lateOutcome = func(outcome httpOutcome) error { return old.owner.receiveLateOutcome("remote", outcome) }
	f.ack(t, "/next", httpPull{Version: 20, ExecutorID: "remote"}, http.StatusConflict, 0)
	f.pull(t)
	oldCtx, cancelOld := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelOld()
	oldDone := make(chan httpServerTestExchange, 1)
	go func() {
		oldDone <- sendHTTPOutcome(oldCtx, f.server.Client(), f.server.URL+"/outcome", old.outcome, "", "old")
	}()
	select {
	case <-entered:
	case <-oldCtx.Done():
		t.Fatal("headerless late upload did not enter its read lane")
	}
	currentCtx, cancelCurrent := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCurrent()
	currentDone := make(chan httpServerTestExchange, 1)
	go func() {
		currentDone <- sendHTTPOutcome(currentCtx, f.server.Client(), f.server.URL+"/outcome", f.outcome(1, nil), "", "current")
	}()
	select {
	case <-currentEntered:
	case <-currentCtx.Done():
		t.Fatal("headerless current upload did not reach the receiver")
	}
	close(release)
	select {
	case got := <-oldDone:
		if got.err != nil || got.status != http.StatusOK || got.ack.Failure != nil {
			t.Fatalf("headerless late upload: %+v", got)
		}
	case <-oldCtx.Done():
		t.Fatal("headerless late upload did not finish")
	}
	select {
	case got := <-currentDone:
		if got.err != nil || got.status != http.StatusConflict || got.ack.Failure == nil {
			t.Fatalf("headerless current upload was accepted: %+v", got)
		}
	case <-currentCtx.Done():
		t.Fatal("headerless current upload did not finish")
	}
	f.tx.mu.Lock()
	sequence := f.tx.sequence
	f.tx.mu.Unlock()
	if sequence != 0 {
		t.Fatalf("headerless current upload consumed sequence %d", sequence)
	}
	activeCtx, cancelActive := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelActive()
	active := sendHTTPOutcome(activeCtx, f.server.Client(), f.server.URL+"/outcome", f.outcome(1, nil),
		httpBindingDigest(httpJobBinding(job)), "")
	if active.err != nil || active.status != http.StatusOK || active.ack.Failure != nil {
		t.Fatalf("hinted current outcome after headerless uploads: %+v", active)
	}
	if got := f.result(t); got.err != nil {
		t.Fatalf("hinted current outcome failed: %v", got.err)
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
			if calls != 2 {
				t.Fatalf("retry failed to resync usage after replacement: saves=%d", calls)
			}
			beforeReplay, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found {
				t.Fatalf("load usage after retry: found=%t error=%v", found, err)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("deduplicated replay status=%d", status)
			}
			record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found || record.Tokens != f.outcome.Result.Calls[0].Tokens ||
				len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil || !reflect.DeepEqual(beforeReplay, record) {
				t.Fatalf("ambiguous retry usage: %+v found=%t err=%v", record, found, err)
			}
		})
	}
}

func TestLateRemoteOutcomeCompletesUsageAfterFailedTerminalSave(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "zero calls"
		if pending {
			name = "saved pending call"
		}
		t.Run(name, func(t *testing.T) {
			f := newLateOutcomeFixture(t, pending)
			finalSaves := 0
			f.owner.finalUsageSave = func(record usage.Record) error {
				finalSaves++
				if record.Status != "failed" || record.FinishedAt == nil {
					t.Fatalf("terminal usage save did not reach finished status: %+v", record)
				}
				return errors.New("injected failed terminal usage save")
			}
			if err := f.finish(errors.New("lost outcome")); err == nil || finalSaves != 1 {
				t.Fatalf("terminal usage save did not fail before replacement: error=%v saves=%d", err, finalSaves)
			}
			before, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found || before.Status != "running" || before.FinishedAt != nil {
				t.Fatalf("failed terminal save changed durable usage: %+v found=%t error=%v", before, found, err)
			}
			lateSaves := 0
			f.owner.lateUsageSave = func(record usage.Record) error {
				lateSaves++
				if lateSaves == 1 {
					return errors.New("injected failed late usage save")
				}
				return f.owner.Usage.Save(record)
			}
			_, client, origin := lateOutcomeEndpoint(t, f.owner)
			if status, ack := postLateOutcome(t, client, origin, f.outcome); status != http.StatusInternalServerError ||
				ack.Failure == nil || strings.Contains(ack.Failure.Message, "injected") {
				t.Fatalf("failed late status save ACK: status=%d ack=%+v", status, ack)
			}
			stillRunning, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found || !reflect.DeepEqual(stillRunning, before) {
				t.Fatalf("failed late save changed durable usage: before=%+v after=%+v found=%t error=%v", before, stillRunning, found, err)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("late terminal usage retry status=%d", status)
			}
			if lateSaves != 2 {
				t.Fatalf("late terminal usage retry saves=%d", lateSaves)
			}
			after, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			decision := f.store.Data.TerminalDecisions[f.job.Identity.JobID]
			if err != nil || !found || after.Status != "failed" || after.FinishedAt == nil ||
				!after.FinishedAt.Equal(decision.DecidedAt) || len(after.Calls) != len(f.outcome.Result.Calls) {
				t.Fatalf("late upload did not finish usage from terminal decision: %+v found=%t error=%v", after, found, err)
			}
			if pending && (after.Tokens != f.outcome.Result.Calls[0].Tokens || after.Calls[0].FinishedAt == nil) {
				t.Fatalf("late terminal usage lost call receipt: %+v", after)
			}
			if status, _ := postLateOutcome(t, client, origin, f.outcome); status != http.StatusOK {
				t.Fatalf("completed terminal usage replay status=%d", status)
			}
			replayed, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
			if err != nil || !found || !reflect.DeepEqual(after, replayed) {
				t.Fatalf("terminal usage replay changed record: before=%+v after=%+v found=%t error=%v", after, replayed, found, err)
			}
		})
	}
}

func TestLateRemoteUsageSurvivesFinalStatusSaveOverlap(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	entered, release, lateSaved := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.owner.lateUsageSave = func(record usage.Record) error {
		close(entered)
		<-release
		err := f.owner.Usage.Save(record)
		close(lateSaved)
		return err
	}
	lateDone := make(chan error, 1)
	go func() { lateDone <- f.owner.receiveLateOutcome("remote", f.outcome) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("late usage save did not start")
	}
	finishDone := make(chan error, 1)
	finishWaiting, finishEntered := make(chan struct{}), make(chan struct{})
	orderingViolation := make(chan struct{}, 1)
	f.owner.finalUsageWaiting = func() { close(finishWaiting) }
	f.owner.finalUsageEntered = func() {
		select {
		case <-lateSaved:
		default:
			orderingViolation <- struct{}{}
		}
		close(finishEntered)
	}
	go func() {
		finishDone <- f.finish(errors.New("lost outcome"))
	}()
	select {
	case <-finishWaiting:
	case <-time.After(5 * time.Second):
		t.Fatal("final usage did not reach the private-record lock")
	}
	select {
	case <-finishEntered:
		t.Fatal("final usage entered its save before late usage finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := waitLateError(t, lateDone); err != nil {
		t.Fatal(err)
	}
	if err := waitLateError(t, finishDone); err == nil {
		t.Fatal("final usage status lost original action error")
	}
	select {
	case <-finishEntered:
	default:
		t.Fatal("final usage never entered its save")
	}
	select {
	case <-orderingViolation:
		t.Fatal("final usage save preceded the late usage receipt")
	default:
	}
	record, found, err := f.owner.Usage.Load(f.job.Identity.AttemptID)
	if err != nil || !found || record.Status != "failed" || record.Tokens != f.outcome.Result.Calls[0].Tokens ||
		len(record.Calls) != 1 || record.Calls[0].FinishedAt == nil {
		t.Fatalf("final usage overwrite erased late receipt: %+v found=%t err=%v", record, found, err)
	}
}

func TestLateRemoteOutcomeDoesNotInvertStateAndPrivateLocks(t *testing.T) {
	f := newLateOutcomeFixture(t, true)
	stateHeld, lateEntered, releaseLate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	abort := make(chan struct{})
	defer close(abort)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLate) }) }
	defer release()
	f.owner.lateUsageSave = func(record usage.Record) error {
		close(lateEntered) // The late receipt already holds the private-record lock.
		<-releaseLate
		return f.owner.Usage.Save(record)
	}
	terminalDone := make(chan error, 1)
	go func() {
		terminalDone <- f.store.WithLock(func() error {
			close(stateHeld)
			select {
			case <-lateEntered: // Admission must reach the private lock without taking the state lock.
			case <-abort:
				return nil
			}
			f.owner.CompleteTerminalUsage(monitor.Job{ID: f.job.Identity.JobID, AttemptID: f.job.Identity.AttemptID}, "lost", time.Now())
			return nil
		})
	}()
	select {
	case <-stateHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal save did not hold the state lock")
	}
	lateDone := make(chan error, 1)
	go func() { lateDone <- f.owner.receiveLateOutcome("remote", f.outcome) }()
	select {
	case <-lateEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("late receipt waited for the held state lock")
	}
	release()
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
	var releaseOnce sync.Once
	releaseSave := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSave()
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
	selected := make(chan struct{})
	f.owner.pruneSelected = func() { close(selected) }
	go func() { pruneDone <- f.owner.PrunePrivateRecords(t.Context(), f.store, time.Now().UTC()) }()
	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("prune did not select the expired attempt")
	}
	releaseSave()
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
