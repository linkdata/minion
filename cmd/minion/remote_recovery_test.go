package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

type remoteStartupGitHub struct {
	*terminalRecoveryGitHub
	invitationChecks  int
	invitationChecked chan struct{}
}

func (g *remoteStartupGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	g.invitationChecks++
	if g.invitationChecked != nil {
		select {
		case g.invitationChecked <- struct{}{}:
		default:
		}
	}
	return nil, nil
}

func TestRemoteStartupRecoversWithoutWorkerPull(t *testing.T) {
	for _, tc := range []struct {
		name              string
		assigned          bool
		expiresDuringWait bool
		dueInvitation     bool
		futureRetry       bool
		endpointStops     bool
		deadlineWait      time.Duration
		readyTimeout      time.Duration
		fixedRefusal      string
		outcome           string
	}{
		{name: "before delivery expires during readiness wait", expiresDuringWait: true, outcome: "expired"},
		{name: "before delivery services invitation without a pull", expiresDuringWait: true, dueInvitation: true, deadlineWait: 3 * time.Second, readyTimeout: 100 * time.Millisecond, outcome: "expired"},
		{name: "before delivery waits through saved retry in one pass", expiresDuringWait: true, futureRetry: true, deadlineWait: 3 * time.Second, readyTimeout: 100 * time.Millisecond, outcome: "expired"},
		{name: "endpoint loss ends accepted wait", endpointStops: true},
		{name: "after delivery is lost on restart", assigned: true, outcome: "lost"},
		{name: "saved fixed refusal finishes on restart", fixedRefusal: "Cannot complete this request.", outcome: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certFile, keyFile, _ := remoteTestTLSFiles(t)
			cfg := selectedServiceTestConfig(t)
			cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = 1, 2
			cfg.Repositories[0].AllowedActors = []string{"writer"}
			cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
			cfg.RemoteExecutor = &config.RemoteExecutorConfig{
				ExecutorID: "remote", Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
				Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
			}
			now := time.Now().UTC()
			accepted := state.AcceptedAction{
				JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "remote",
				Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
				SubjectType: "Issue", SubjectNumber: 7,
				SourceKey: "comment:1", SourceNodeID: "source-node",
				SourceURL: "https://github.com/owner/repo/issues/7#issuecomment-1", SourceUpdatedAt: now,
				Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
				AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour), Assigned: tc.assigned,
				FixedRefusal: tc.fixedRefusal,
			}
			statePath := filepath.Join(t.TempDir(), "state.json")
			store, err := state.Open(statePath, now)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			store.Data = state.Data{
				Cursor:   now.Add(-2 * time.Minute),
				Accepted: &accepted,
				JobFailures: map[string]state.JobFailure{accepted.JobID: {
					Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
				}},
				Processed: map[string]time.Time{}, TerminalDecisions: map[string]state.TerminalDecision{},
				PublicationWrites: map[string]state.PublicationWrite{},
				NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour),
			}
			gh := &remoteStartupGitHub{terminalRecoveryGitHub: &terminalRecoveryGitHub{item: github.Item{
				Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
				UpdatedAt: now, Body: "@bot comment", Author: github.User{Login: "writer", ID: 7},
			}}}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			owner := newOrchestrator(cfg, nil, store, logger, nil)
			watcher := &monitor.Monitor{Config: cfg, GitHub: gh, BotID: 8, Logger: logger}
			endpoint, err := attachSelectedRemoteWorker(owner, watcher)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = endpoint.Close() })
			if tc.expiresDuringWait {
				deadlineWait := tc.deadlineWait
				if deadlineWait == 0 {
					deadlineWait = 5 * time.Second
				}
				accepted.Deadline = time.Now().Add(deadlineWait)
				failure := store.Data.JobFailures[accepted.JobID]
				failure.Deadline = accepted.Deadline
				store.Data.JobFailures[accepted.JobID] = failure
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = state.Open(statePath, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			owner.Store = store
			if tc.expiresDuringWait && terminalRecoveryPending(store) {
				t.Fatal("acceptance expired before remote readiness wait")
			}
			if tc.dueInvitation {
				dueAt := time.Now().Add(time.Second)
				store.Data.NextInviteCheckAt = dueAt
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				if !time.Now().Before(dueAt) {
					t.Fatal("invitation became due before remote readiness wait")
				}
			}
			if tc.futureRetry {
				retryAt := time.Now().Add(time.Second)
				store.Data.Accepted.RetryAt = retryAt
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				if !time.Now().Before(retryAt) {
					t.Fatal("saved retry became due before remote recovery began")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var endpointStopped <-chan struct{}
			if tc.endpointStops {
				gh.invitationChecked = make(chan struct{}, 1)
				store.Data.NextInviteCheckAt = time.Now().Add(-time.Second)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				stopped := make(chan struct{})
				endpointStopped = stopped
				go func() {
					select {
					case <-gh.invitationChecked:
						_ = endpoint.Close()
					case <-ctx.Done():
					}
					close(stopped)
				}()
			}
			readyTimeout := tc.readyTimeout
			if readyTimeout == 0 {
				readyTimeout = 7 * time.Second
			}
			code := runSelectedRemoteService(ctx, owner, watcher, store, tc.endpointStops, logger, endpoint, readyTimeout)
			if tc.endpointStops {
				<-endpointStopped
				if code != 1 || store.Data.Accepted == nil || !store.Data.Processed[accepted.JobID].IsZero() {
					t.Fatalf("endpoint loss did not stop accepted wait: code=%d state=%+v logs=%q", code, store.Data, logs.String())
				}
				return
			}
			if code != 0 {
				t.Fatalf("remote recovery returned %d: %s", code, logs.String())
			}
			if store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
				store.Data.TerminalDecisions[accepted.JobID].Outcome != tc.outcome || gh.reply == "" {
				t.Fatalf("remote reservation survived without a worker: state=%+v reply=%q logs=%q", store.Data, gh.reply, logs.String())
			}
			if tc.dueInvitation && gh.invitationChecks == 0 {
				t.Fatal("due invitation was not serviced during the no-pull accepted wait")
			}
			if tc.fixedRefusal != "" && !strings.Contains(gh.reply, tc.fixedRefusal) {
				t.Fatalf("saved fixed refusal was not published: %q", gh.reply)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(statePath, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() ||
				reopened.Data.TerminalDecisions[accepted.JobID].Outcome != tc.outcome {
				t.Fatalf("remote reservation was not durably released: %+v", reopened.Data)
			}
		})
	}
}

func TestRemoteStartupClearsOrphanResultBeforeReadiness(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "without worker"
		if held {
			name = "with precleanup held pull"
		}
		t.Run(name, func(t *testing.T) {
			certFile, keyFile, leaf := remoteTestTLSFiles(t)
			cfg := selectedServiceTestConfig(t)
			cfg.RemoteExecutor = &config.RemoteExecutorConfig{
				ExecutorID: "remote", Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
				Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
			}
			statePath := filepath.Join(t.TempDir(), "state.json")
			store, err := state.Open(statePath, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			store.Data.Processed["old-job"] = time.Now().UTC()
			store.Data.NextInviteCheckAt, store.Data.NextPollAt = time.Now().Add(time.Hour), time.Now().Add(time.Hour)
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := store.WriteAcceptedResult([]byte("orphan result")); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = state.Open(statePath, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			owner := newOrchestrator(cfg, nil, store, logger, nil)
			watcher := &monitor.Monitor{Config: cfg, GitHub: &terminalRecoveryGitHub{}, BotID: 8, Logger: logger}
			endpoint, err := attachSelectedRemoteWorker(owner, watcher)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = endpoint.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type pullResult struct {
				status int
				err    error
			}
			var pulled <-chan pullResult
			if held {
				roots := x509.NewCertPool()
				roots.AddCert(leaf)
				client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
				t.Cleanup(client.CloseIdleConnections)
				finished := make(chan pullResult, 1)
				pulled = finished
				go func() {
					body := fmt.Sprintf(`{"version":%d,"executor_id":"remote"}`, action.ProtocolVersion)
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint.Addr().String()+"/next", strings.NewReader(body))
					if err != nil {
						finished <- pullResult{err: err}
						return
					}
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Authorization", "Bearer "+cfg.RemoteExecutor.Token)
					response, err := client.Do(request)
					if err != nil {
						finished <- pullResult{err: err}
						return
					}
					_, err = io.Copy(io.Discard, response.Body)
					if closeErr := response.Body.Close(); err == nil {
						err = closeErr
					}
					finished <- pullResult{status: response.StatusCode, err: err}
				}()
				if err := owner.WaitCapacity(ctx); err != nil {
					t.Fatalf("precleanup authenticated pull was unavailable: %v", err)
				}
			}
			if code := runSelectedRemoteService(ctx, owner, watcher, store, false, logger, endpoint, 100*time.Millisecond); code != 1 {
				t.Fatalf("remote startup did not await a fresh pull: code=%d logs=%q", code, logs.String())
			}
			if present, err := store.HasAcceptedResult(); err != nil || present {
				t.Fatalf("orphan result remained after startup without a fresh worker: present=%t error=%v", present, err)
			}
			if held {
				select {
				case result := <-pulled:
					if result.err != nil || result.status != http.StatusTooEarly {
						t.Fatalf("precleanup pull was not retired: %+v", result)
					}
				case <-ctx.Done():
					t.Fatalf("precleanup pull did not retire: %v", ctx.Err())
				}
			}
		})
	}
}

func TestOrchestrateRemoteRestartFinishesFixedRefusalBeforeWorkerAttach(t *testing.T) {
	cfg := selectedServiceTestConfig(t)
	missingTLS := t.TempDir()
	cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = 1, 2
	cfg.Repositories[0].AllowedActors = []string{"writer"}
	cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	cfg.RemoteExecutor = &config.RemoteExecutorConfig{
		ExecutorID: "remote", Listen: "127.0.0.1:0",
		TLSCertFile: filepath.Join(missingTLS, "cert.pem"), TLSKeyFile: filepath.Join(missingTLS, "key.pem"),
		Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
	}
	now := time.Now().UTC()
	accepted := state.AcceptedAction{
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "remote",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "comment:1", SourceNodeID: "source-node",
		SourceURL: "https://github.com/owner/repo/issues/7#issuecomment-1", SourceUpdatedAt: now,
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour), FixedRefusal: "Cannot complete this request.",
	}
	store := serviceStore(t, state.Data{
		Cursor: now.Add(-2 * time.Minute), Accepted: &accepted,
		JobFailures: map[string]state.JobFailure{accepted.JobID: {
			Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
		}},
		Processed: map[string]time.Time{}, TerminalDecisions: map[string]state.TerminalDecision{},
		PublicationWrites: map[string]state.PublicationWrite{},
		NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour),
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &terminalRecoveryGitHub{item: github.Item{
		Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
		UpdatedAt: now, Body: "@bot comment", Author: github.User{Login: "writer", ID: 7},
	}}
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, nil, store, logger, nil)
	watcher := &monitor.Monitor{Config: cfg, GitHub: gh, BotID: 8, Logger: logger}
	if code := runOrchestrateMode(t.Context(), owner, watcher, store, false, logger, "", time.Second); code != 0 ||
		store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
		!strings.Contains(gh.reply, accepted.FixedRefusal) || owner.Executor != nil {
		t.Fatalf("remote restart waited for worker before fixed refusal: code=%d reply=%q state=%+v logs=%q", code, gh.reply, store.Data, logs.String())
	}
}

type remoteRecoveryLoopGitHub struct {
	*terminalRecoveryGitHub
	resumed      chan struct{}
	replyEntered chan struct{}
	releaseReply <-chan struct{}
}

func (g *remoteRecoveryLoopGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	return nil, nil
}

func (g *remoteRecoveryLoopGitHub) ListRepositoryNotifications(context.Context, string, time.Time) ([]github.Notification, time.Duration, error) {
	select {
	case g.resumed <- struct{}{}:
	default:
	}
	return nil, time.Minute, nil
}

func (g *remoteRecoveryLoopGitHub) CreateIssueComment(ctx context.Context, repo string, number int, body string) (github.Comment, error) {
	select {
	case g.replyEntered <- struct{}{}:
	default:
	}
	select {
	case <-g.releaseReply:
	case <-ctx.Done():
		return github.Comment{}, ctx.Err()
	}
	return g.terminalRecoveryGitHub.CreateIssueComment(ctx, repo, number, body)
}

func TestRemoteLoopContinuesAfterRecoveryWithFreshPull(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service bool
	}{
		{name: "startup capacity wait", service: true},
		{name: "normal monitor loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRemoteLoopContinuesAfterRecoveryWithFreshPull(t, tc.service)
		})
	}
}

func testRemoteLoopContinuesAfterRecoveryWithFreshPull(t *testing.T, service bool) {
	t.Helper()
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	cfg := selectedServiceTestConfig(t)
	cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = 1, 2
	cfg.Repositories[0].AllowedActors = []string{"writer"}
	cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	cfg.RemoteExecutor = &config.RemoteExecutorConfig{
		ExecutorID: "remote", Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
		Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
	}
	now := time.Now().UTC()
	accepted := state.AcceptedAction{
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "remote",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "comment:1", SourceNodeID: "source-node",
		SourceURL: "https://github.com/owner/repo/issues/7#issuecomment-1", SourceUpdatedAt: now,
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour), Assigned: true,
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(statePath, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	store.Data.NextPollAt = now.Add(-time.Second)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	releaseReply := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseReply) })
	defer release()
	gh := &remoteRecoveryLoopGitHub{terminalRecoveryGitHub: &terminalRecoveryGitHub{item: github.Item{
		Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
		UpdatedAt: now, Body: "@bot comment", Author: github.User{Login: "writer", ID: 7},
	}}, resumed: make(chan struct{}, 1), replyEntered: make(chan struct{}, 1), releaseReply: releaseReply}
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, nil, store, logger, nil)
	watcher := &monitor.Monitor{Config: cfg, GitHub: gh, BotID: 8, Logger: logger}
	endpoint, err := attachSelectedRemoteWorker(owner, watcher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
	t.Cleanup(client.CloseIdleConnections)
	url := "https://" + endpoint.Addr().String()
	pullBody := fmt.Sprintf(`{"version":%d,"executor_id":"remote"}`, action.ProtocolVersion)
	post := func(ctx context.Context, path, token, body string) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url+path, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		return client.Do(request)
	}
	pull := func(ctx context.Context, want int) <-chan error {
		done := make(chan error, 1)
		go func() {
			response, err := post(ctx, "/next", cfg.RemoteExecutor.Token, pullBody)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				if closeErr := response.Body.Close(); err == nil {
					err = closeErr
				}
				if err == nil && response.StatusCode != want {
					err = fmt.Errorf("held pull status %d, want %d", response.StatusCode, want)
				}
			}
			done <- err
		}()
		return done
	}
	serviceCtx, stopService := context.WithTimeout(t.Context(), 15*time.Second)
	defer stopService()
	serviceDone := make(chan int, 1)
	go func() {
		if service {
			serviceDone <- runSelectedRemoteService(serviceCtx, owner, watcher, store, true, logger, endpoint, 5*time.Second)
		} else {
			serviceDone <- runMonitorLoop(serviceCtx, watcher, store, true, endpoint.Done(), logger, false)
		}
	}()
	select {
	case <-gh.replyEntered:
	case code := <-serviceDone:
		t.Fatalf("remote service exited before terminal reply: code=%d logs=%q", code, logs.String())
	case <-serviceCtx.Done():
		t.Fatalf("remote recovery did not reach terminal reply: %v logs=%q", serviceCtx.Err(), logs.String())
	}
	prePullCtx, stopPrePull := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopPrePull()
	prePullDone := pull(prePullCtx, http.StatusTooEarly)
	preReady, stopPreReady := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopPreReady()
	if err := owner.WaitCapacity(preReady); err != nil {
		t.Fatalf("authenticated pull did not arrive during recovery: %v", err)
	}
	release()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var saved struct {
			Accepted  *state.AcceptedAction `json:"accepted"`
			Processed map[string]time.Time  `json:"processed"`
		}
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Accepted == nil && !saved.Processed[accepted.JobID].IsZero() {
			break
		}
		select {
		case code := <-serviceDone:
			t.Fatalf("remote service exited before a fresh pull: code=%d logs=%q", code, logs.String())
		case <-serviceCtx.Done():
			t.Fatalf("remote recovery did not release the repository: %v logs=%q", serviceCtx.Err(), logs.String())
		case <-ticker.C:
		}
	}
	if err := <-prePullDone; err != nil {
		t.Fatalf("pull held before cleanup was not retired: %v", err)
	}
	statusBody, err := json.Marshal(struct {
		Status action.Status `json:"status"`
	}{Status: action.Status{
		Version: action.ProtocolVersion,
		Identity: action.Identity{
			JobID: accepted.JobID, AttemptID: accepted.AttemptID, ExecutorID: accepted.ExecutorID,
			Repository: accepted.Repository, RepositoryID: accepted.RepositoryID, OwnerID: accepted.OwnerID,
			Requester: accepted.Requester, RequesterID: accepted.RequesterID,
			Number: accepted.SubjectNumber, SubjectType: accepted.SubjectType,
		},
		Sequence: 1, WorkerState: "ready", Phase: "cleanup",
	}})
	if err != nil {
		t.Fatal(err)
	}
	postStatus := func(path, token, body string) (int, error) {
		probeCtx, stopProbe := context.WithTimeout(t.Context(), time.Second)
		defer stopProbe()
		response, err := post(probeCtx, path, token, body)
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(io.Discard, response.Body)
		if closeErr := response.Body.Close(); err == nil {
			err = closeErr
		}
		return response.StatusCode, err
	}
	if status, err := postStatus("/next", "old-token", pullBody); err != nil || status != http.StatusUnauthorized {
		t.Fatalf("stale credential advertised capacity: status=%d error=%v", status, err)
	}
	if status, err := postStatus("/status", cfg.RemoteExecutor.Token, string(statusBody)); err != nil || status != http.StatusConflict {
		t.Fatalf("stale Status advertised capacity: status=%d error=%v", status, err)
	}
	noPull, stopNoPull := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stopNoPull()
	if err := owner.WaitCapacity(noPull); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale pull or Status changed readiness: %v", err)
	}
	pullCtx, stopPull := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopPull()
	pullDone := pull(pullCtx, http.StatusConflict)
	select {
	case <-gh.resumed:
	case code := <-serviceDone:
		t.Fatalf("remote service exited before resuming with a fresh pull: code=%d logs=%q", code, logs.String())
	case <-serviceCtx.Done():
		t.Fatalf("remote service did not resume after a fresh pull: %v logs=%q", serviceCtx.Err(), logs.String())
	}
	stopService()
	if code := <-serviceDone; code != 0 {
		t.Fatalf("remote service stopped after fresh pull: code=%d logs=%q", code, logs.String())
	}
	if !service {
		if err := endpoint.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-pullDone; err != nil {
		t.Fatal(err)
	}
}
