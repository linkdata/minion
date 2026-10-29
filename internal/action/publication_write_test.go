package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

func permitSelectedPublication(context.Context, monitor.Job, bool) error { return nil }

func TestFixPushCheckpointKeepsExactTargetAndOriginalGraceAcrossRestart(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	e := Orchestrator{Store: store, BotLogin: "bot", BotID: 8, PublicationGrace: 42 * time.Second}
	at := request.AcceptedAt.Add(time.Second)
	sha := strings.Repeat("b", 40)
	branch := "minion/fix-7-aaaaaaaaaaaa"
	if err := e.beginFixPushAt(t.Context(), request, "bot/repo", branch, sha, nil, func() time.Time { return at }); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[fixPushWriteKey(request.ID)]
	if write.AttemptID != request.AttemptID || write.Repository != "bot/repo" || write.Branch != branch || write.SHA != sha ||
		write.Status != state.PublicationWritePending || !write.StartedAt.Equal(at) || !write.Deadline.Equal(at.Add(42*time.Second)) ||
		store.Data.PendingFixes[request.ID].SHA != sha || !store.Data.Accepted.Deadline.Equal(request.Deadline) {
		t.Fatalf("issued push changed identity, SHA, or original deadline: write=%+v pending=%+v accepted=%+v", write,
			store.Data.PendingFixes[request.ID], store.Data.Accepted)
	}
	if err := e.beginFixPush(t.Context(), request, "bot/repo", branch, sha, nil); !errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("issued push was reserved twice: %v", err)
	}
	withoutAttempt := request
	withoutAttempt.AttemptID = ""
	if err := e.beginFixPush(t.Context(), withoutAttempt, "bot/repo", branch, sha, nil); !errors.Is(err, monitor.ErrAcceptanceState) {
		t.Fatalf("missing attempt ID was generated or accepted: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e.Store = store
	if got := store.Data.PublicationWrites[fixPushWriteKey(request.ID)]; got != write {
		t.Fatalf("pending push checkpoint changed after restart: got=%+v want=%+v", got, write)
	}
	if err := e.observeFixPush(request); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.Store = store
	if got := store.Data.PublicationWrites[fixPushWriteKey(request.ID)]; got.Status != state.PublicationWriteObserved ||
		got.ResolvedAt.IsZero() || got.Branch != branch || got.SHA != sha || !got.Deadline.Equal(write.Deadline) {
		t.Fatalf("observed push checkpoint changed after restart: %+v", got)
	}
}

func TestFixPushRequiresCommittedResult(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	store.Data.Accepted.ResultReady = false
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	e := Orchestrator{Store: store, BotLogin: "bot", BotID: 8}
	err := e.beginFixPushAt(t.Context(), request, "bot/repo", "minion/fix-7-aaaaaaaaaaaa", strings.Repeat("b", 40), nil,
		func() time.Time { return request.AcceptedAt.Add(time.Second) })
	if !errors.Is(err, monitor.ErrAcceptanceState) || len(store.Data.PublicationWrites) != 0 || len(store.Data.PendingFixes) != 0 {
		t.Fatalf("uncommitted result reserved a push: err=%v writes=%v fixes=%v", err, store.Data.PublicationWrites, store.Data.PendingFixes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(reopened.Data.PublicationWrites) != 0 || len(reopened.Data.PendingFixes) != 0 {
		t.Fatalf("uncommitted result left a durable push: writes=%v fixes=%v", reopened.Data.PublicationWrites, reopened.Data.PendingFixes)
	}
}

func TestFixPushDeadlineAfterCheckpointSaveRollsBackBothRecords(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	e := Orchestrator{Store: store, BotLogin: "bot", BotID: 8}
	checks := 0
	err := e.beginFixPushAt(t.Context(), request, "bot/repo", "minion/fix-7-aaaaaaaaaaaa", strings.Repeat("b", 40), nil,
		func() time.Time {
			checks++
			if checks == 1 {
				return request.AcceptedAt.Add(time.Second)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var saved state.Data
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.PublicationWrites[fixPushWriteKey(request.ID)].Status != state.PublicationWritePending ||
				saved.PendingFixes[request.ID].SHA != strings.Repeat("b", 40) {
				t.Fatalf("push checkpoint was not saved before deadline check: writes=%v fixes=%v", saved.PublicationWrites, saved.PendingFixes)
			}
			return request.Deadline.Add(time.Nanosecond)
		})
	if !errors.Is(err, context.DeadlineExceeded) || checks != 2 || len(store.Data.PublicationWrites) != 0 || len(store.Data.PendingFixes) != 0 {
		t.Fatalf("expired unissued push retained a checkpoint: err=%v checks=%d writes=%v fixes=%v", err, checks, store.Data.PublicationWrites, store.Data.PendingFixes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(reopened.Data.PublicationWrites) != 0 || len(reopened.Data.PendingFixes) != 0 {
		t.Fatalf("expired unissued push survived restart: writes=%v fixes=%v", reopened.Data.PublicationWrites, reopened.Data.PendingFixes)
	}
}

func TestFixPushReconciliationRequiresExactRemoteBranchAndSHA(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	sha := strings.Repeat("b", 40)
	branch := "minion/fix-7-aaaaaaaaaaaa"
	e := Orchestrator{Store: store, BotLogin: "bot", BotID: 8}
	if err := e.beginFixPush(t.Context(), request, "bot/repo", branch, sha, nil); err != nil {
		t.Fatal(err)
	}
	var mode string
	lookups := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/bot/repo/branches/"+branch {
			t.Errorf("unexpected reconciliation request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		lookups++
		switch mode {
		case "missing":
			http.NotFound(w, r)
		case "wrong SHA":
			_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, strings.Repeat("c", 40))
		case "wrong branch":
			_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch+"-other", sha)
		default:
			_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, sha)
		}
	}))
	t.Cleanup(server.Close)
	e.GitHub = testGitHub(server, "token")
	for _, mismatch := range []string{"missing", "wrong SHA", "wrong branch"} {
		mode = mismatch
		if err := e.reconcileFixPush(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
			t.Fatalf("%s remote branch authorized publication: %v", mismatch, err)
		}
		if got := store.Data.PublicationWrites[fixPushWriteKey(request.ID)]; got.Status != state.PublicationWritePending ||
			store.Data.PendingFixes[request.ID].SHA != sha {
			t.Fatalf("%s remote branch changed checkpoint: %+v", mismatch, got)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e.Store = store
	mode = "match"
	if err := e.reconcileFixPush(t.Context(), request); err != nil || lookups != 4 ||
		store.Data.PublicationWrites[fixPushWriteKey(request.ID)].Status != state.PublicationWriteObserved {
		t.Fatalf("matching remote SHA was not observed after restart: lookups=%d write=%+v error=%v", lookups,
			store.Data.PublicationWrites[fixPushWriteKey(request.ID)], err)
	}
}

func TestUncertainFixPushCannotResumeAfterBranchAppears(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	sha := strings.Repeat("b", 40)
	branch := "minion/fix-7-aaaaaaaaaaaa"
	e := Orchestrator{Store: store, BotLogin: "bot", BotID: 8}
	if err := e.beginFixPush(t.Context(), request, "bot/repo", branch, sha, nil); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[fixPushWriteKey(request.ID)]
	write.Status, write.ResolvedAt = state.PublicationWriteUncertain, time.Now().UTC()
	store.Data.PublicationWrites[fixPushWriteKey(request.ID)] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e.Store = store
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, sha)
	}))
	t.Cleanup(server.Close)
	e.GitHub = testGitHub(server, "token")
	if err := e.reconcileFixPush(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) ||
		lookups.Load() != 0 || store.Data.PublicationWrites[fixPushWriteKey(request.ID)] != write {
		t.Fatalf("uncertain push was reused after branch appeared: lookups=%d write=%+v error=%v",
			lookups.Load(), store.Data.PublicationWrites[fixPushWriteKey(request.ID)], err)
	}
}

func TestExpiredSelectedFixObservesPushWithoutExtendingActionDeadline(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	now := time.Now().UTC()
	request.AcceptedAt, request.Deadline = now.Add(-3*time.Minute), now.Add(-time.Minute)
	store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = request.AcceptedAt, request.Deadline
	data, err := store.ReadAcceptedResult(AcceptedResultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	record, err := DecodeAcceptedResultRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	record.Accepted = *store.Data.Accepted
	data, err = httpEncode(record, int(AcceptedResultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult(data); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	branch := "minion/fix-7-aaaaaaaaaaaa"
	e := Orchestrator{Store: store, BotLogin: "bot", BotID: 8, PublicationGrace: 3 * time.Minute}
	if err := e.beginFixPushAt(t.Context(), request, "bot/repo", branch, sha, nil,
		func() time.Time { return now.Add(-2 * time.Minute) }); err != nil {
		t.Fatal(err)
	}
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/bot/repo/branches/"+branch {
			reads.Add(1)
			_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, sha)
			return
		}
		writes.Add(1)
		t.Errorf("expired fix reached GitHub publication: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	e.GitHub = testGitHub(server, "token")
	if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrTerminalJob) ||
		!errors.Is(err, context.DeadlineExceeded) || reads.Load() != 1 || writes.Load() != 0 ||
		store.Data.PublicationWrites[fixPushWriteKey(request.ID)].Status != state.PublicationWriteObserved ||
		!store.Data.Accepted.Deadline.Equal(request.Deadline) {
		t.Fatalf("expired fix was republished or remote push was lost: reads=%d writes=%d accepted=%+v push=%+v error=%v",
			reads.Load(), writes.Load(), store.Data.Accepted, store.Data.PublicationWrites[fixPushWriteKey(request.ID)], err)
	}
}

func TestExpiredIssuedCommentNeverRepostsOrRerunsModel(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8, PublicationGrace: time.Minute}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[request.ID]
	write.StartedAt = time.Now().UTC().Add(-2 * time.Minute)
	write.Deadline = write.StartedAt.Add(time.Minute)
	store.Data.PublicationWrites[request.ID] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var lookups, taskPosts, unexpected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			lookups.Add(1)
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			taskPosts.Add(1)
			http.Error(w, "second task publication", http.StatusInternalServerError)
		default:
			unexpected.Add(1)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.GitHub = testGitHub(server, "token")
	reconciler := &monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion"}, GitHub: orchestrator.GitHub}
	if err := reconciler.ReconcilePublicationWrites(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.Data.PublicationWrites[request.ID]; got.Status != state.PublicationWriteUncertain || got.ResolvedAt.IsZero() {
		t.Fatalf("expired issued write was not closed as uncertain: %+v", got)
	}
	var workChecks, modelCalls int
	orchestrator.CheckWorkRoot = func() error { workChecks++; return nil }
	orchestrator.Runner = Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
		modelCalls++
		return aicli.Result{}, errors.New("model should not run")
	}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("recovered uncertain publication = %v", err)
	}
	orchestrator.assignmentRequired = true
	if err := orchestrator.Execute(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("executed uncertain publication = %v", err)
	}
	if lookups.Load() != 1 || taskPosts.Load() != 0 || unexpected.Load() != 0 || workChecks != 0 || modelCalls != 0 {
		t.Fatalf("uncertain write started work or issued another publication: lookups=%d posts=%d unexpected=%d work=%d models=%d",
			lookups.Load(), taskPosts.Load(), unexpected.Load(), workChecks, modelCalls)
	}
}

func TestTaskPublicationUsesConfiguredDefaultGrace(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	reserveActionRequest(t, store, &request, true)
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := e.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[request.ID]
	if got, want := write.Deadline.Sub(write.StartedAt), time.Duration(config.DefaultPublicationGraceSeconds)*time.Second; got != want {
		t.Fatalf("publication grace = %s, want %s", got, want)
	}
}

func TestUnissuedPublicationIsClearedWhenDeadlineExpiresAfterSave(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	before := request.Deadline.Add(-time.Second)
	checks := 0
	err := e.beginTaskPublicationAt(t.Context(), request, "comment", func() time.Time {
		checks++
		if checks == 1 {
			return before
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var saved state.Data
		if unmarshalErr := json.Unmarshal(data, &saved); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		if write := saved.PublicationWrites[request.ID]; write.Status != state.PublicationWritePending {
			t.Fatalf("issued write was not saved before the deadline check: %+v", write)
		}
		return request.Deadline.Add(time.Nanosecond)
	})
	if !errors.Is(err, context.DeadlineExceeded) || checks != 2 || len(store.Data.PublicationWrites) != 0 {
		t.Fatalf("known unissued write remained pending: err=%v checks=%d writes=%v", err, checks, store.Data.PublicationWrites)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(reopened.Data.PublicationWrites) != 0 {
		t.Fatalf("unissued write survived restart: %+v", reopened.Data.PublicationWrites)
	}
}

func TestTaskPublicationRequiresValidBotIdentityBeforeWrite(t *testing.T) {
	for _, login := range []string{"", "owner/bot"} {
		t.Run(login, func(t *testing.T) {
			store := openState(t, time.Now())
			request := usageJob()
			request.Command = command.Command{Name: "comment", Action: command.Comment}
			reserveActionRequest(t, store, &request, true)
			e := Orchestrator{Store: store, BotLogin: login, BotID: 8}
			if err := e.beginTaskPublication(t.Context(), request, "comment"); !errors.Is(err, monitor.ErrAcceptanceState) || len(store.Data.PublicationWrites) != 0 {
				t.Fatalf("invalid bot login issued a write: err=%v writes=%v", err, store.Data.PublicationWrites)
			}
		})
	}
	t.Run("missing ID", func(t *testing.T) {
		store := openState(t, time.Now())
		request := usageJob()
		request.Command = command.Command{Name: "comment", Action: command.Comment}
		reserveActionRequest(t, store, &request, true)
		e := Orchestrator{Store: store, BotLogin: "linkdataminion"}
		if err := e.beginTaskPublication(t.Context(), request, "comment"); !errors.Is(err, monitor.ErrAcceptanceState) || len(store.Data.PublicationWrites) != 0 {
			t.Fatalf("missing bot ID issued a write: err=%v writes=%v", err, store.Data.PublicationWrites)
		}
	})
}

func TestObservedTaskPublicationUpdatesEarlierUncertainResolution(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	reserveActionRequest(t, store, &request, true)
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := e.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[request.ID]
	write.StartedAt = time.Now().UTC().Add(-2 * time.Minute)
	write.Deadline = write.StartedAt.Add(time.Minute)
	write.Status = state.PublicationWriteUncertain
	write.ResolvedAt = write.Deadline
	store.Data.PublicationWrites[request.ID] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if err := e.observeTaskPublication(request); err != nil {
		t.Fatal(err)
	}
	observed := store.Data.PublicationWrites[request.ID]
	if observed.Status != state.PublicationWriteObserved || observed.ResolvedAt.Before(started) || observed.ResolvedAt.After(time.Now().UTC()) {
		t.Fatalf("observed write retained stale resolution: %+v", observed)
	}
}

func TestObservedTaskPublicationSaveFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name, kind  string
		action      command.Action
		resultReady bool
	}{
		{name: "saved comment", kind: "comment", action: command.Comment, resultReady: true},
		{name: "saved review", kind: "review", action: command.Review, resultReady: true},
		{name: "fixed reply", kind: "comment", action: command.Comment},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := state.Open(path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			request := usageJob()
			request.Command = command.Command{Name: test.kind, Action: test.action}
			if test.action == command.Review {
				request.SubjectType = "PullRequest"
			}
			reserveActionRequest(t, store, &request, true)
			store.Data.Accepted.ResultReady = test.resultReady
			store.Data.Accepted.RetryAt = request.Deadline
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
			if err := e.beginTaskPublication(t.Context(), request, test.kind); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "child"), []byte("occupied"), 0600); err != nil {
				t.Fatal(err)
			}
			err = e.observeTaskPublication(request)
			if test.resultReady && (!errors.Is(err, monitor.ErrPostModelPublication) || errors.Is(err, monitor.ErrAcceptanceState)) ||
				!test.resultReady && (!errors.Is(err, monitor.ErrAcceptanceState) || errors.Is(err, monitor.ErrPostModelPublication)) {
				t.Fatalf("save failure after observed %s has wrong classification: %v", test.name, err)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := e.observeTaskPublication(request); err != nil {
				t.Fatalf("observed %s did not persist on retry: %v", test.kind, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved state.Data
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if got := saved.PublicationWrites[request.ID].Status; got != state.PublicationWriteObserved {
				t.Fatalf("retried %s observation was not durable: %s", test.kind, got)
			}
			if got := saved.Accepted.RetryAt; test.resultReady && !got.IsZero() ||
				!test.resultReady && !got.Equal(request.Deadline) {
				t.Fatalf("observed %s changed accepted retry incorrectly: %s", test.name, got)
			}
		})
	}
}

func TestUnissuedTaskPublicationSaveFailureRemainsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	reserveActionRequest(t, store, &request, true)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	e := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8}
	if err := e.beginTaskPublication(t.Context(), request, "comment"); !errors.Is(err, monitor.ErrAcceptanceState) ||
		errors.Is(err, monitor.ErrPostModelPublication) {
		t.Fatalf("pre-publication save failure changed classification: %v", err)
	}
}

func TestIssuedCommentReconcilesLostResponseAfterRestartWithoutSecondWrite(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8, PublicationGrace: time.Minute}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if write := reopened.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWritePending || write.BotLogin != "linkdataminion" || write.BotID != 8 {
		t.Fatalf("issued write did not survive restart: %+v", write)
	}
	var lookups, posts atomic.Int32
	markerBody := "Published before the response was lost\n\n" + request.Marker()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			if lookups.Add(1) == 1 {
				_, _ = fmt.Fprint(w, `[]`)
			} else {
				_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"id":8,"login":"renamed-bot"},"updated_at":%q}]`,
					markerBody, request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"id":8,"login":"renamed-bot"}}}}`, markerBody)
		default:
			posts.Add(1)
			http.Error(w, "unexpected second write", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.Store, orchestrator.GitHub, orchestrator.BotLogin, orchestrator.BotID = reopened, testGitHub(server, "token"), "replacement-bot", 9
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) || posts.Load() != 0 {
		t.Fatalf("unobserved issued write was retried: err=%v posts=%d", err, posts.Load())
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 0 || lookups.Load() != 2 {
		t.Fatalf("published marker was not reconciled: err=%v posts=%d lookups=%d", err, posts.Load(), lookups.Load())
	}
	if write := reopened.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWriteObserved || write.ResolvedAt.IsZero() {
		t.Fatalf("observed remote effect was not saved: %+v", write)
	}
	if got := reopened.Data.TerminalDecisions[request.ID].Outcome; got != "succeeded" {
		t.Fatalf("reconciled effect did not record successful local outcome: %q", got)
	}
}

func TestTaskPublicationRecoversAcceptedPostWithLostResponseAfterBotSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		action     command.Action
		status     int
	}{
		{"comment", "/repos/owner/repo/issues/7/comments", command.Comment, http.StatusCreated},
		{"review", "/repos/owner/repo/pulls/7/reviews", command.Review, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := testJob(tc.action)
			store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
			nodeID := "reply-node"
			if tc.action == command.Review {
				nodeID = "review-node"
			}
			var posts, provenance atomic.Int32
			var mu sync.Mutex
			var postedBody string
			body := func() string {
				mu.Lock()
				defer mu.Unlock()
				return postedBody
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
					if current := body(); current != "" && tc.action == command.Comment {
						_ = json.NewEncoder(w).Encode([]map[string]any{{
							"id": 1, "node_id": nodeID, "body": current,
							"user":       map[string]any{"id": 8, "login": "linkdataminion"},
							"updated_at": request.Source.UpdatedAt.Add(time.Second),
						}})
					} else {
						_, _ = fmt.Fprint(w, `[]`)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"owner/repo","owner":{"id":2,"login":"owner"},"default_branch":"main"}`)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					var input struct {
						Query     string `json:"query"`
						Variables struct {
							ID string `json:"id"`
						} `json:"variables"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode GraphQL request: %v", err)
						return
					}
					current := body()
					if strings.Contains(input.Query, "timelineItems") && tc.action == command.Review {
						var nodes []map[string]any
						if current != "" {
							nodes = []map[string]any{{
								"id": nodeID, "fullDatabaseId": "1", "body": current,
								"author": map[string]any{"id": 8, "login": "linkdataminion"}, "state": "COMMENTED",
								"submittedAt": request.Source.UpdatedAt.Add(time.Second),
							}}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
							"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
					} else if current != "" && input.Variables.ID == nodeID {
						provenance.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
							"body": current, "author": map[string]any{"id": 8, "login": "linkdataminion"}}}})
					} else {
						t.Errorf("unexpected GraphQL query or node: %q %q", input.Query, input.Variables.ID)
						http.Error(w, "unexpected GraphQL request", http.StatusBadRequest)
					}
				case r.Method == http.MethodPost && r.URL.Path == tc.path:
					var input struct {
						Body     string `json:"body"`
						CommitID string `json:"commit_id"`
						Event    string `json:"event"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode publication: %v", err)
						return
					}
					if !strings.Contains(input.Body, "Saved complete answer") || !strings.HasSuffix(input.Body, "\n\n"+request.Marker()) ||
						tc.action == command.Review && (input.CommitID != job.Source.Head || input.Event != "COMMENT") {
						t.Errorf("unexpected publication: %+v", input)
					}
					if posts.Add(1) != 1 {
						http.Error(w, "unexpected second publication", http.StatusInternalServerError)
						return
					}
					mu.Lock()
					postedBody = input.Body
					mu.Unlock()
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, `{"id":1}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			client := testGitHub(server, "token")
			underlying := client.HTTPClient.Transport
			client.HTTPClient.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := underlying.RoundTrip(r)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != tc.path {
					return response, err
				}
				if response.StatusCode != tc.status {
					t.Errorf("publication was not accepted: status=%d", response.StatusCode)
				}
				_ = response.Body.Close() // The server accepted the POST; its response is lost to the client.
				return nil, io.ErrUnexpectedEOF
			})
			orchestrator := Orchestrator{Store: store, GitHub: client, BotLogin: "linkdataminion", BotID: 8, WorkRoot: t.TempDir(),
				ValidatePublication: permitSelectedPublication}
			if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) || !errors.Is(err, io.ErrUnexpectedEOF) || posts.Load() != 1 {
				t.Fatalf("accepted POST did not leave an issued write: err=%v posts=%d", err, posts.Load())
			}
			if write := store.Data.PublicationWrites[request.ID]; write.Kind != string(tc.action) || write.BotLogin != "linkdataminion" || write.BotID != 8 || write.Status != state.PublicationWritePending {
				t.Fatalf("lost response did not preserve issued write: %+v", write)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			orchestrator.Store, orchestrator.GitHub, orchestrator.BotLogin, orchestrator.BotID = reopened, testGitHub(server, "token"), "replacement-bot", 9
			if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || posts.Load() != 1 || provenance.Load() != 1 {
				t.Fatalf("restart did not reconcile accepted POST: err=%v posts=%d provenance=%d", err, posts.Load(), provenance.Load())
			}
			if write := reopened.Data.PublicationWrites[request.ID]; write.Kind != string(tc.action) || write.BotLogin != "linkdataminion" || write.BotID != 8 || write.Marker != request.Marker() || write.Status != state.PublicationWriteObserved {
				t.Fatalf("marker publication was not observed: %+v", write)
			}
			if got := reopened.Data.TerminalDecisions[request.ID].Outcome; got != "succeeded" {
				t.Fatalf("reconciled publication did not finish the action: %q", got)
			}
		})
	}
}

func TestIssuedTaskCommentResponseIsObservedBeforeTerminalDecision(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	request.Deadline = time.Now().Add(time.Minute)
	reserveActionRequest(t, store, &request, true)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "linkdataminion", BotID: 8}
	if err := e.publishTaskComment(t.Context(), request, "answer"); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || store.Data.PublicationWrites[request.ID].Status != state.PublicationWriteObserved ||
		len(store.Data.TerminalDecisions) != 0 {
		t.Fatalf("remote effect was not recorded before the terminal decision: posts=%d state=%+v", posts.Load(), store.Data)
	}
	if err := e.publishTaskComment(t.Context(), request, "answer"); !errors.Is(err, monitor.ErrPostModelPublication) || posts.Load() != 1 {
		t.Fatalf("issued comment was posted again: err=%v posts=%d", err, posts.Load())
	}
}

func TestAcceptedTaskCommentAfterDeadlineKeepsRemoteEffectAndFirstDecision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
		posts := 0
		client := github.New("token")
		client.BaseURL = "https://api.example.test"
		client.HTTPClient = &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusOK, `{"id":1,"full_name":"owner/repo","owner":{"id":2},"default_branch":"main","visibility":"public"}`
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/7/comments":
				posts++
				if write := store.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWritePending {
					t.Fatalf("POST preceded durable issued-write record: %+v", write)
				}
				if !time.Now().Before(request.Deadline) {
					t.Fatal("action deadline expired before GitHub accepted the POST")
				}
				time.Sleep(time.Until(request.Deadline) + time.Nanosecond)
				if err := store.WithLock(func() error {
					_, err := store.DecideTerminalLocked(request.ID, request.AttemptID, "expired", time.Now())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				status, body = http.StatusCreated, `{"id":1}`
			default:
				t.Fatalf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})}
		e := &Orchestrator{Store: store, GitHub: client, BotLogin: "linkdataminion", BotID: 8}
		if err := e.publishTaskComment(t.Context(), request, "answer"); err != nil {
			t.Fatal(err)
		}
		if posts != 1 || store.Data.PublicationWrites[request.ID].Status != state.PublicationWriteObserved {
			t.Fatalf("accepted POST was not observed after expiry: posts=%d write=%+v", posts, store.Data.PublicationWrites[request.ID])
		}
		if err := e.recordPublishedOutcome(request); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late publication changed terminal outcome: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := state.Open(path, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		e.Store = reopened
		if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) {
			t.Fatalf("recovery of late publication = %v", err)
		}
		if posts != 1 || reopened.Data.TerminalDecisions[request.ID].Outcome != "expired" ||
			reopened.Data.PublicationWrites[request.ID].Status != state.PublicationWriteObserved ||
			!reopened.Data.PublicationWrites[request.ID].ResolvedAt.After(request.Deadline) {
			t.Fatalf("recovery changed first decision or reposted: posts=%d decision=%+v write=%+v",
				posts, reopened.Data.TerminalDecisions[request.ID], reopened.Data.PublicationWrites[request.ID])
		}
	})
}

func TestIssuedReviewReconcilesUsingRecordedKindAfterCommandRemap(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.SubjectType = "PullRequest"
	request.Command = command.Command{Name: "review", Action: command.Review}
	request.Source.UpdatedAt = time.Now().UTC()
	request.AcceptedAt = time.Now().UTC()
	request.Deadline = request.AcceptedAt.Add(time.Minute)
	reserveActionRequest(t, store, &request, true)
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8, assignmentRequired: true}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "review"); err != nil {
		t.Fatal(err)
	}
	request.Command.Action = command.Comment // The configured command changed after the review was issued.
	var timelines, provenance, writes atomic.Int32
	markerBody := "Review already published\n\n" + request.Marker()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var input struct{ Query string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(input.Query, "timelineItems") {
				timelines.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": []map[string]any{{
						"id": "review-node", "fullDatabaseId": "1", "body": markerBody,
						"author": map[string]any{"id": 8, "login": "linkdataminion"}, "state": "COMMENTED",
						"submittedAt": request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339),
					}}, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
			} else {
				provenance.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
					"body": markerBody, "author": map[string]any{"id": 8, "login": "linkdataminion"}}}})
			}
		default:
			writes.Add(1)
			http.Error(w, "unexpected write", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.GitHub = testGitHub(server, "token")
	if err := orchestrator.Execute(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || !strings.Contains(err.Error(), "selected action differs from its reservation") || timelines.Load() != 0 || writes.Load() != 0 {
		t.Fatalf("remapped command reused the selected reservation: err=%v timelines=%d writes=%d", err, timelines.Load(), writes.Load())
	}
	reconciler := &monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion"}, GitHub: orchestrator.GitHub}
	if err := reconciler.ReconcilePublicationWrites(t.Context(), store); err != nil ||
		timelines.Load() != 1 || provenance.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("remapped command did not reconcile the issued review: err=%v timelines=%d provenance=%d writes=%d",
			err, timelines.Load(), provenance.Load(), writes.Load())
	}
	if write := store.Data.PublicationWrites[request.ID]; write.Kind != "review" || write.Status != state.PublicationWriteObserved {
		t.Fatalf("recorded review effect changed kind or was not observed: %+v", write)
	}
}

func TestIssuedCommentReconcilesUsingRecordedSinceAfterSourceEdit(t *testing.T) {
	store := openState(t, time.Now())
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	request.Source.UpdatedAt = time.Now().UTC()
	request.AcceptedAt = time.Now().UTC()
	request.Deadline = request.AcceptedAt.Add(time.Minute)
	reserveActionRequest(t, store, &request, true)
	orchestrator := Orchestrator{Store: store, BotLogin: "linkdataminion", BotID: 8, assignmentRequired: true}
	if err := orchestrator.beginTaskPublication(t.Context(), request, "comment"); err != nil {
		t.Fatal(err)
	}
	issuedSince := request.Source.UpdatedAt
	request.Source.UpdatedAt = issuedSince.Add(10 * time.Second) // The command was edited after its write.
	markerBody := "Comment already published\n\n" + request.Marker()
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			reads.Add(1)
			_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"id":8,"login":"linkdataminion"},"updated_at":%q}]`,
				markerBody, issuedSince.Add(time.Second).UTC().Format(time.RFC3339))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"id":8,"login":"linkdataminion"}}}}`, markerBody)
		default:
			writes.Add(1)
			http.Error(w, "unexpected write", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	orchestrator.GitHub = testGitHub(server, "token")
	if err := orchestrator.Execute(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || !strings.Contains(err.Error(), "selected action differs from its reservation") || reads.Load() != 0 || writes.Load() != 0 {
		t.Fatalf("edited source reused the selected reservation: err=%v reads=%d writes=%d", err, reads.Load(), writes.Load())
	}
	reconciler := &monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion"}, GitHub: orchestrator.GitHub}
	if err := reconciler.ReconcilePublicationWrites(t.Context(), store); err != nil || reads.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("edited source lost the original write observation: err=%v reads=%d writes=%d", err, reads.Load(), writes.Load())
	}
	if write := store.Data.PublicationWrites[request.ID]; write.Status != state.PublicationWriteObserved || !write.Since.Equal(issuedSince) {
		t.Fatalf("recorded lookup bound changed after source edit: %+v", write)
	}
}
