package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

func TestHTTPInactivityOutcomePersistsThroughSelectedMonitor(t *testing.T) {
	testHTTPWorkerLimitOutcome(t, false)
}

func TestHTTPEarlyRuntimeCapOutcomePersistsThroughSelectedMonitor(t *testing.T) {
	testHTTPWorkerLimitOutcome(t, true)
}

func testHTTPWorkerLimitOutcome(t *testing.T, earlyRuntimeCap bool) {
	t.Helper()
	gitFixtureRemote(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion comment",
		HTMLURL: "https://github.com/o/r/issues/7#issuecomment-1", UpdatedAt: now,
		Author: github.User{Login: "writer", ID: 7}}
	gh := &actionMonitorGitHub{items: []github.Item{item}, notification: github.Notification{
		ID: "thread-1", Repository: github.Repository{FullName: "o/r"},
		Subject: github.Subject{Type: "Issue", URL: "https://api.github.com/repos/o/r/issues/7"}, UpdatedAt: now,
	}}
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveIssueFixture(t, w, r) {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(githubServer.Close)
	commands := map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	owner := &Orchestrator{WorkRoot: t.TempDir(), Store: store, GitHub: testGitHub(githubServer, "fixture"),
		BotLogin: "linkdataminion", BotID: 8, Commands: commands, InactivityTimeout: time.Minute}
	m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60,
		MaxRuntimeMinutes: 7, InactivityTimeoutMinutes: 1, Commands: commands,
		Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"comment"},
			AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}},
		GitHub: gh, Handler: owner, BotID: 8}
	bindSelectedRunner(&m, store, owner)

	remote, err := NewHTTPExecutor(LocalExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if earlyRuntimeCap && r.URL.Path == "/next" {
			remote.serveNextEncoded(w, r, func(assignment httpAssignment) ([]byte, error) {
				assignment.Job.RuntimeBudget = time.Nanosecond // Worker stops before the parent deadline.
				return httpEncode(assignment, httpJobBytes)
			})
			return
		}
		remote.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { _ = remote.Close(); workerServer.Close() })
	selected := make(chan Identity, 1)
	parentOutcome := make(chan error, 1)
	owner.Executor = readyExecutor{executorFunc(func(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
		selected <- job.Identity
		if err := sink.(assignmentCheckpoint).BeforeAssignment(job); err != nil {
			return Result{}, err
		}
		result, err := remote.Execute(ctx, job, sink)
		parentOutcome <- err
		return result, err
	})}
	modelStarted := make(chan struct{})
	workerDone := make(chan error, 1)
	workerRoot := t.TempDir()
	go func() {
		workerDone <- RunHTTP(ctx, workerServer.Client(), workerServer.URL, LocalExecutorID,
			&Runner{WorkRoot: workerRoot, runModel: func(_ context.Context, _ modelRun) (aicli.Result, error) {
				close(modelStarted)
				if earlyRuntimeCap {
					return aicli.Result{}, errors.New("model ran after worker runtime cap")
				}
				return aicli.Result{}, errors.Join(monitor.ErrTerminalJob, errExecutorInactivity)
			}})
	}()
	if _, err := m.RunOnce(ctx, store); err != nil {
		t.Fatal(err)
	}
	if !earlyRuntimeCap {
		select {
		case <-modelStarted:
		default:
			t.Fatal("monitor did not reach the model call")
		}
	}
	select {
	case err := <-workerDone:
		if earlyRuntimeCap {
			if !errors.Is(err, errExecutorRuntimeCap) || !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("HTTP worker did not report its runtime cap: %v", err)
			}
		} else if !errors.Is(err, errExecutorInactivity) || !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("HTTP worker did not report inactivity: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("HTTP worker did not finish: %v", ctx.Err())
	}
	if earlyRuntimeCap {
		select {
		case <-modelStarted:
			t.Fatal("worker ran a model after its runtime cap")
		default:
		}
	}
	var reported *workerFailureError
	select {
	case err := <-parentOutcome:
		if !errors.As(err, &reported) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(err, monitor.ErrTerminalJob) ||
			(earlyRuntimeCap && !reported.failure.RuntimeCap ||
				!earlyRuntimeCap && !reported.ExecutorInactivity()) {
			t.Fatalf("parent did not retain a checkpointed worker-limit outcome: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("parent did not finish HTTP execution: %v", ctx.Err())
	}
	var identity Identity
	select {
	case identity = <-selected:
	case <-ctx.Done():
		t.Fatalf("monitor did not select an action: %v", ctx.Err())
	}
	decision := store.Data.TerminalDecisions[identity.JobID]
	wantOutcome := "inactivity"
	if earlyRuntimeCap {
		wantOutcome = "expired"
	}
	if decision.Outcome != wantOutcome || decision.AttemptID != identity.AttemptID || store.Data.Processed[identity.JobID].IsZero() {
		t.Fatalf("monitor did not finish with %s: decision=%+v state=%+v", wantOutcome, decision, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.Data.TerminalDecisions[identity.JobID]; got != decision {
		t.Fatalf("reopened state lost %s decision: got=%+v want=%+v", wantOutcome, got, decision)
	}
}
