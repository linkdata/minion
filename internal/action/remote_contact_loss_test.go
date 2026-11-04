package action

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

type remoteContactLossHandler struct {
	worker     *remoteWorker
	store      *state.Store
	executions int
}

func (h *remoteContactLossHandler) WaitCapacity(ctx context.Context) error {
	return h.worker.WaitCapacity(ctx)
}

func (h *remoteContactLossHandler) Execute(ctx context.Context, request monitor.Job) error {
	h.executions++
	job := testJob(command.Comment)
	job.Identity.JobID, job.Identity.AttemptID, job.Identity.ExecutorID = request.ID, request.AttemptID, "remote"
	job.Identity.Repository, job.Identity.RepositoryID, job.Identity.OwnerID = request.Repository.Name, request.Repository.ID, request.Repository.OwnerID
	job.Identity.Requester, job.Identity.RequesterID = request.Source.Author.Login, request.Source.Author.ID
	job.Identity.Number, job.Identity.SubjectType = request.Number, request.SubjectType
	job.Command, job.Action = request.Command.Name, request.Command.Action
	job.Main.Model, job.Main.Effort = request.MainSettings.Model, request.MainSettings.Effort
	job.RuntimeBudget = time.Second
	probe := &remoteAssignmentProbe{before: func(Job) error { return markSelectedAssignment(h.store, request) }}
	_, err := h.worker.Execute(ctx, job, probe)
	return err
}

func TestRemoteDeliveredPullLossSettlesWithoutReconnect(t *testing.T) {
	certificate, leaf := testRemoteCertificate(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	executor.outcomeGrace = 20 * time.Millisecond
	endpoint, err := ListenRemoteExecution(executor, "127.0.0.1:0", certificate, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	origin := "https://" + endpoint.Addr().String()
	client, err := NewRemoteExecutionClient(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client.Transport.(*authenticatedExecutionTransport).TLSClientConfig.RootCAs = roots
	base, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	transport := &httpExecutionClient{client: client, base: base}

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
	gh := &httpFailureReplyGitHub{actionMonitorGitHub: &actionMonitorGitHub{
		items: []github.Item{item}, notification: github.Notification{
			ID: "thread-1", Repository: github.Repository{FullName: "o/r"},
			Subject: github.Subject{Type: "Issue", URL: "https://api.github.com/repos/o/r/issues/7"}, UpdatedAt: now,
		},
	}}
	handler := &remoteContactLossHandler{worker: &remoteWorker{executor: executor, endpoint: endpoint}, store: store}
	watcher := &monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60,
		MainSettings: config.RoleModelConfig{Model: "test-main", Effort: "medium"},
		Commands:     map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"comment"},
			AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}},
		GitHub: gh, Handler: handler, BotID: 8, SelectedExecutorID: "remote", SelectedCommands: map[string]bool{"comment": true}}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	type pullResult struct {
		assignment httpAssignment
		err        error
	}
	pulled := make(chan pullResult, 1)
	go func() {
		var assignment httpAssignment
		err := transport.post(ctx, "next", pull, httpJobBytes, &assignment, httpBinding{}, 0)
		pulled <- pullResult{assignment, err}
	}()
	if err := handler.WaitCapacity(ctx); err != nil {
		t.Fatalf("authenticated held pull was unavailable: %v", err)
	}
	finished := make(chan error, 1)
	go func() { _, err := watcher.RunOnce(ctx, store); finished <- err }()
	select {
	case got := <-pulled:
		if got.err != nil || got.assignment.Job.Identity.ExecutorID != "remote" || got.assignment.Job.Identity.JobID == "" {
			t.Fatalf("remote assignment was not delivered: %+v", got)
		}
	case <-ctx.Done():
		t.Fatalf("remote Job was not delivered: %v", ctx.Err())
	}
	// The authenticated worker drops contact after receiving Job and never sends an outcome.
	select {
	case err := <-finished:
		if err != nil && !errors.Is(err, ErrRemoteWorkerUnavailable) {
			t.Fatalf("lost remote Job did not settle: %v state=%+v", err, store.Data)
		}
	case <-ctx.Done():
		t.Fatalf("lost remote Job did not settle without reconnect: %v", ctx.Err())
	}
	if handler.executions != 1 || store.Data.Accepted != nil || len(store.Data.Processed) != 1 || len(gh.replies) != 1 {
		t.Fatalf("remote contact loss retained exclusive work or repeated it: executions=%d state=%+v replies=%q", handler.executions, store.Data, gh.replies)
	}
	for jobID := range store.Data.Processed {
		if decision := store.Data.TerminalDecisions[jobID]; decision.Outcome != "lost" {
			t.Fatalf("post-delivery contact loss outcome = %+v", decision)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.Accepted != nil || len(reopened.Data.Processed) != 1 {
		t.Fatalf("contact loss did not durably release repository: %+v", reopened.Data)
	}
	watcher.Handler = httpNetworkAction(func(context.Context, monitor.Job) error {
		t.Fatal("lost remote work was dispatched again")
		return nil
	})
	reopened.Data.NextPollAt = time.Now().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := watcher.RunOnce(ctx, reopened); err != nil {
		t.Fatalf("restart retried processed remote work: %v", err)
	}
	if err := handler.WaitCapacity(ctx); !errors.Is(err, ErrRemoteWorkerUnavailable) {
		t.Fatalf("lost worker advertised readiness: %v", err)
	}
}
