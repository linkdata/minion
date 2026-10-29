package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type synchronizedCommentsGitHub struct {
	*fakeGitHub
	mu sync.Mutex
}

func (g *synchronizedCommentsGitHub) CreateIssueComment(ctx context.Context, repo string, number int, body string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.CreateIssueComment(ctx, repo, number, body)
}

func (g *synchronizedCommentsGitHub) commentsSnapshot() (int, []github.Comment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commentCalls, append([]github.Comment(nil), g.comments...)
}

func (g *synchronizedCommentsGitHub) setCommentError(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commentErr = err
}

type controlReplyLog struct {
	failed    chan struct{}
	completed chan struct{}
}

func (w controlReplyLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "terminal control in ") {
		select {
		case w.failed <- struct{}{}:
		default:
		}
	}
	if strings.Contains(string(p), "stop retrying failed ") {
		select {
		case w.completed <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func TestSelectedCancellationReplaysFetchedThreadsWithoutNotifications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		first := selectedItem(now.Add(-3 * time.Second))
		sibling := selectedItem(now.Add(-2 * time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		later := selectedItem(now.Add(-time.Second))
		later.Key, later.NodeID = "comment:3", "node:3"
		later.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-3"
		firstNotice := testNotification(first.UpdatedAt)
		laterNotice := testNotification(later.UpdatedAt)
		laterNotice.ID = "thread-2"
		laterNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"

		initial := now.Add(-time.Minute)
		path := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, path, initial)
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		fetched := map[int]int{}
		gh := &fakeGitHub{
			notifications: []github.Notification{firstNotice, laterNotice},
			permission:    github.Permission{Permission: "write"},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				fetched[number]++
				switch number {
				case 7:
					return []github.Item{first, sibling}, nil
				case 8:
					return []github.Item{later}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()

		select {
		case job := <-held.entered:
			if job.ID != jobID(first.Key, "review") {
				t.Fatalf("wrong first action selected: %+v", job)
			}
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		synctest.Wait()
		if fetched[7] != 1 || fetched[8] != 0 || held.executions.Load() != 1 {
			t.Fatalf("unaccepted thread was fetched while Execute was blocked: fetched=%v executions=%d", fetched, held.executions.Load())
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved state.Data
		if err := json.Unmarshal(bytes, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Accepted == nil || saved.Accepted.JobID != jobID(first.Key, "review") || len(saved.PendingNotifications) != 2 {
			t.Fatalf("active action and fetched threads were not durable: %+v", saved)
		}
		for _, notice := range []github.Notification{firstNotice, laterNotice} {
			pending, ok := saved.PendingNotifications[notificationKey(notice)]
			if !ok || pending.Repository != notice.Repository.FullName || pending.RepositoryID != 1 || pending.OwnerID != 2 ||
				pending.SubjectType != notice.Subject.Type || pending.SubjectURL != notice.Subject.URL ||
				!pending.Since.Equal(initial) || !pending.UpdatedAt.Equal(notice.UpdatedAt) ||
				!pending.RetryAt.IsZero() || pending.Failures != 0 {
				t.Fatalf("fetched thread pointer not durable: notice=%+v pending=%+v present=%t", notice, pending, ok)
			}
		}

		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
		if store.Data.Accepted != nil {
			t.Fatalf("canceled unassigned action kept its reservation: %+v", store.Data.Accepted)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := openStateAt(t, path, initial)
		if len(reopened.Data.PendingNotifications) != 2 {
			t.Fatalf("fetched threads lost across restart: %+v", reopened.Data.PendingNotifications)
		}

		gh.notifications = nil
		var replayed []Job
		m.Handler = selectedHandler(func(job Job) error {
			replayed = append(replayed, job)
			return nil
		})
		time.Sleep(time.Minute)
		if _, err := m.RunOnce(t.Context(), reopened); err != nil {
			t.Fatalf("replay without GitHub notifications: %v", err)
		}
		want := []string{jobID(first.Key, "review"), jobID(sibling.Key, "review"), jobID(later.Key, "review")}
		got := make([]string, 0, len(replayed))
		for _, job := range replayed {
			got = append(got, job.ID)
		}
		if !slices.Equal(got, want) || len(reopened.Data.PendingNotifications) != 0 {
			t.Fatalf("unaccepted commands did not replay once: jobs=%v want=%v pending=%+v", got, want, reopened.Data.PendingNotifications)
		}
		if fetched[8] == 0 {
			t.Fatal("later thread was not fetched on replay")
		}
		for _, id := range want {
			if reopened.Data.Processed[id].IsZero() {
				t.Fatalf("replayed command %s was not completed", id)
			}
		}
	})
}

func TestSelectedActionChecksSiblingAtJobDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		siblingID := jobID(sibling.Key, "review")
		deadline := now.Add(time.Minute)
		store := openState(t, now.Add(-time.Minute))
		store.Data.JobFailures[siblingID] = state.JobFailure{
			Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: deadline,
			NotificationKey: notificationKey(testNotification(selected.UpdatedAt)),
		}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		var repoCalls atomic.Int32
		gh := &fakeGitHub{
			notifications:    []github.Notification{testNotification(selected.UpdatedAt)},
			items:            []github.Item{selected, sibling},
			permission:       github.Permission{Permission: "write"},
			beforeRepository: func() { repoCalls.Add(1) },
			beforeComment: func() {
				if time.Now().Before(deadline) {
					t.Error("sibling received terminal reply before its deadline")
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		synctest.Wait()
		if got := repoCalls.Load(); got != 1 {
			t.Fatalf("future terminal deadline caused an early repository probe: %d", got)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+siblingID+" -->") ||
			store.Data.Processed[siblingID].IsZero() || held.executions.Load() != 1 || store.Data.Accepted == nil {
			t.Fatalf("due sibling did not finish during selected Execute: comments=%v processed=%v executions=%d accepted=%+v",
				gh.comments, store.Data.Processed[siblingID], held.executions.Load(), store.Data.Accepted)
		}
		select {
		case err := <-result:
			t.Fatalf("RunOnce returned while Execute was blocked: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedActionChecksTerminalDeadlineBeforePendingRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		terminal := selectedItem(now.Add(-time.Second))
		terminal.Key, terminal.NodeID = "comment:2", "node:2"
		terminal.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-2"
		terminalID := jobID(terminal.Key, "review")
		notice := testNotification(terminal.UpdatedAt)
		notice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		key := notificationKey(notice)
		deadline := now.Add(time.Minute)
		store := openState(t, now.Add(-time.Minute))
		store.Data.PendingNotifications[key] = state.PendingNotification{
			Repository: notice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
			SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL,
			Since: now.Add(-time.Minute), UpdatedAt: notice.UpdatedAt, RetryAt: now.Add(5 * time.Minute),
		}
		store.Data.JobFailures[terminalID] = state.JobFailure{
			Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: deadline, NotificationKey: key,
		}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			permission:    github.Permission{Permission: "write"},
			beforeComment: func() {
				if time.Now().After(deadline) {
					t.Error("terminal reply missed its deadline")
				}
			},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				switch number {
				case 7:
					return []github.Item{selected}, nil
				case 8:
					return []github.Item{terminal}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+terminalID+" -->") ||
			store.Data.Processed[terminalID].IsZero() {
			t.Fatalf("terminal deadline was masked by notification retry: comments=%v processed=%v executions=%d accepted=%+v",
				gh.comments, store.Data.Processed[terminalID], held.executions.Load(), store.Data.Accepted)
		}
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		if len(gh.comments) != 1 || held.executions.Load() != 1 || store.Data.Accepted == nil {
			t.Fatalf("selected Execute did not remain held past two minutes: comments=%v executions=%d accepted=%+v",
				gh.comments, held.executions.Load(), store.Data.Accepted)
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedActionKeepsTerminalSiblingReplyOneShotWhileBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		siblingID := jobID(sibling.Key, "review")
		store := openState(t, now.Add(-time.Minute))
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: notificationKey(testNotification(selected.UpdatedAt))}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			items:         []github.Item{selected, sibling},
			permission:    github.Permission{Permission: "write"},
			commentErr:    errors.New("temporary reply failure"),
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if gh.commentCalls != 1 || len(gh.comments) != 0 ||
			store.Data.Processed[siblingID].IsZero() || held.executions.Load() != 1 || store.Data.Accepted == nil {
			t.Fatalf("terminal sibling reply was not completed once: calls=%d comments=%v processed=%v executions=%d accepted=%+v",
				gh.commentCalls, gh.comments, store.Data.Processed[siblingID], held.executions.Load(), store.Data.Accepted)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if gh.commentCalls != 1 || len(gh.comments) != 0 {
			t.Fatalf("terminal sibling reply was duplicated: calls=%d comments=%v", gh.commentCalls, gh.comments)
		}
		select {
		case err := <-result:
			t.Fatalf("RunOnce returned while Execute was blocked: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestFailureReplyCheckpointSurvivesLaterStateSaveFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		postErr    error
		wantStatus string
		comments   int
	}{
		{name: "posted", wantStatus: state.PublicationWriteObserved, comments: 1},
		{name: "response uncertain", postErr: errors.New("response lost"), wantStatus: state.PublicationWritePending},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := testConfig(t)
			item := selectedItem(now)
			job := Job{Repository: cfg.Repositories[0], SubjectType: "PullRequest", Number: 7, Source: item,
				Command: command.Command{Name: "review", Action: command.Review}, ID: jobID(item.Key, "review")}
			path := filepath.Join(t.TempDir(), "state.json")
			backup := path + ".saved"
			store := openStateAt(t, path, now.Add(-time.Minute))
			store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: maxJobAttempts}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{items: []github.Item{item}, permission: github.Permission{Permission: "write"},
				commentErr: test.postErr, beforeComment: func() {
					if err := os.Rename(path, backup); err != nil {
						t.Error(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Error(err)
					}
				}}
			m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Now: func() time.Time { return now }}
			if err := m.finishFailedJob(t.Context(), store, job, now); err == nil ||
				gh.commentCalls != 1 || len(gh.comments) != test.comments || !store.Data.Processed[job.ID].IsZero() {
				t.Fatalf("later save failure changed one POST: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStateAt(t, path, now)
			gh.commentErr = nil
			if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil ||
				store.Data.PublicationWrites[job.ID].Status != test.wantStatus {
				t.Fatalf("checkpoint could not reconcile after restart: err=%v state=%+v", err, store.Data)
			}
			if err := m.finishFailedJob(t.Context(), store, job, now); err != nil ||
				gh.commentCalls != 1 || store.Data.Processed[job.ID].IsZero() {
				t.Fatalf("restored state repeated POST: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
			}
		})
	}
}

func TestSelectedActionDoesNotDeferOtherDueTerminalReply(t *testing.T) {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		first := selectedItem(now.Add(-3 * time.Second))
		first.Key, first.NodeID = "comment:first", "node:first"
		second := selectedItem(now.Add(-2 * time.Second))
		second.Key, second.NodeID = "comment:second", "node:second"
		selected := selectedItem(now.Add(-time.Second))
		selected.Key, selected.NodeID = "comment:selected", "node:selected"
		selected.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-selected"
		terminalNotice := testNotification(first.UpdatedAt)
		terminalKey := notificationKey(terminalNotice)
		selectedNotice := testNotification(selected.UpdatedAt)
		selectedNotice.ID = "thread-8"
		selectedNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		firstID, secondID := jobID(first.Key, "review"), jobID(second.Key, "review")
		store := openState(t, now.Add(-time.Minute))
		store.Data.PendingNotifications[terminalKey] = state.PendingNotification{
			Repository: terminalNotice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
			SubjectType: terminalNotice.Subject.Type, SubjectURL: terminalNotice.Subject.URL,
			Since: now.Add(-time.Minute), UpdatedAt: terminalNotice.UpdatedAt, RetryAt: now.Add(time.Minute),
		}
		for _, id := range []string{firstID, secondID} {
			store.Data.JobFailures[id] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: terminalKey}
		}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &synchronizedCommentsGitHub{fakeGitHub: &fakeGitHub{
			notifications: []github.Notification{selectedNotice}, permission: github.Permission{Permission: "write"},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				if number == 7 {
					return []github.Item{first, second}, nil
				}
				return []github.Item{selected}, nil
			},
		}}
		gh.beforeComment = func() {
			if gh.commentCalls == 1 {
				gh.commentErr = errors.New("first reply unavailable")
			} else {
				gh.commentErr = nil
			}
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		signals := controlReplyLog{failed: make(chan struct{}, 1), completed: make(chan struct{}, 1)}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held, Logger: log.New(signals, "", 0),
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("selected action did not start: %v", err)
		}
		synctest.Wait()
		calls, comments := gh.commentsSnapshot()
		time.Sleep(time.Minute)
		select {
		case <-signals.completed:
		case <-deadline.C:
			t.Fatal("second due terminal reply was postponed after the first failed")
		}
		var firstFailure state.JobFailure
		var firstProcessed, secondProcessed, retryAt time.Time
		if err := store.WithLock(func() error {
			firstFailure = store.Data.JobFailures[firstID]
			firstProcessed = store.Data.Processed[firstID]
			secondProcessed = store.Data.Processed[secondID]
			retryAt = store.Data.PendingNotifications[terminalKey].RetryAt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		calls, comments = gh.commentsSnapshot()
		if calls != 2 || len(comments) != 1 ||
			!strings.HasSuffix(comments[0].Body, "\n\n<!-- linkdataminion:"+secondID+" -->") ||
			firstFailure.Attempts != 0 || firstProcessed.IsZero() || secondProcessed.IsZero() {
			t.Fatalf("first reply failure deferred another due command: calls=%d comments=%v first=%+v processed=(%v, %v) retry=%v",
				calls, comments, firstFailure, firstProcessed, secondProcessed, retryAt)
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedActionChecksNewerPendingThreadBeforeRetryAt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-2"
		siblingID := jobID(sibling.Key, "review")
		fresh := testNotification(sibling.UpdatedAt)
		fresh.ID = "thread-2"
		fresh.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		oldSince := now.Add(-time.Hour)
		retryAt := now.Add(5 * time.Minute)
		store := openState(t, now.Add(-time.Minute))
		store.Data.PendingNotifications[notificationKey(fresh)] = state.PendingNotification{
			Repository: fresh.Repository.FullName, RepositoryID: 1, OwnerID: 2,
			SubjectType: fresh.Subject.Type, SubjectURL: fresh.Subject.URL,
			Since: oldSince, UpdatedAt: now.Add(-2 * time.Minute), RetryAt: retryAt, Failures: 2,
		}
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: notificationKey(fresh)}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		var siblingSince []time.Time
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt), fresh},
			permission:    github.Permission{Permission: "write"},
			threadItems: func(_ string, number int, since time.Time) ([]github.Item, error) {
				switch number {
				case 7:
					return []github.Item{selected}, nil
				case 8:
					siblingSince = append(siblingSince, since)
					return []github.Item{sibling}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		pending := store.Data.PendingNotifications[notificationKey(fresh)]
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+siblingID+" -->") ||
			store.Data.Processed[siblingID].IsZero() || held.executions.Load() != 1 || store.Data.Accepted == nil ||
			!time.Now().Before(retryAt) || len(siblingSince) == 0 || !siblingSince[0].Equal(oldSince) ||
			pending.Failures != 2 || !pending.Since.Equal(oldSince) || !pending.UpdatedAt.Equal(fresh.UpdatedAt) {
			t.Fatalf("newer pending thread was not checked before retry: comments=%v processed=%v executions=%d accepted=%+v since=%v pending=%+v",
				gh.comments, store.Data.Processed[siblingID], held.executions.Load(), store.Data.Accepted, siblingSince, pending)
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedReviewWorkerRepliesToTerminalFixWithoutSkill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		fix := selectedItem(now.Add(-time.Second))
		fix.Key, fix.NodeID = "comment:2", "node:2"
		fix.Body = "@linkdataminion fix"
		fix.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-2"
		fixID := jobID(fix.Key, "fix")
		pendingNotice := testNotification(fix.UpdatedAt)
		pendingNotice.ID = "thread-2"
		pendingNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		initial := now.Add(-time.Minute)
		store := openState(t, initial)
		store.Data.PendingNotifications[notificationKey(pendingNotice)] = state.PendingNotification{
			Repository: pendingNotice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
			SubjectType: pendingNotice.Subject.Type, SubjectURL: pendingNotice.Subject.URL,
			Since: initial, UpdatedAt: pendingNotice.UpdatedAt,
		}
		store.Data.JobFailures[fixID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: notificationKey(pendingNotice)}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".agents", "skills", "fix", "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		if installed, _ := cfg.CommandInstalled("fix"); installed {
			t.Fatal("fix skill is still installed")
		}
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			permission:    github.Permission{Permission: "write"},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				switch number {
				case 7:
					return []github.Item{selected}, nil
				case 8:
					return []github.Item{fix}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+fixID+" -->") ||
			store.Data.Processed[fixID].IsZero() || held.executions.Load() != 1 || store.Data.Accepted == nil ||
			len(gh.reactions) != 1 || gh.reactions[0] != selected.NodeID {
			t.Fatalf("terminal fix was dispatched or missed: comments=%v processed=%v executions=%d accepted=%+v reactions=%v",
				gh.comments, store.Data.Processed[fixID], held.executions.Load(), store.Data.Accepted, gh.reactions)
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedControlRetiresMissingTerminalSourceWithoutLosingFix(t *testing.T) {
	for _, test := range []struct {
		name                string
		removedInFetch      bool
		removedInCurrent    bool
		transientCurrent    bool
		ambiguousFetch      bool
		denyActor           bool
		denyPermission      bool
		transientPermission bool
	}{
		{name: "deleted source"},
		{name: "command removed from fetched source", removedInFetch: true},
		{name: "fetched fix edited before reply", removedInCurrent: true},
		{name: "transient source lookup", transientCurrent: true},
		{name: "ambiguous edited source", ambiguousFetch: true},
		{name: "ambiguous source with transient lookup", ambiguousFetch: true, transientCurrent: true},
		{name: "actor denied", denyActor: true},
		{name: "permission denied", denyPermission: true},
		{name: "transient permission lookup", transientPermission: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				missing := selectedItem(now.Add(-3 * time.Minute))
				missing.Key, missing.NodeID = "comment:missing", "node:missing"
				missing.HTMLURL = "https://github.com/example-owner/example-repo/pull/6#issuecomment-missing"
				missingCommand := "review"
				if test.removedInCurrent || test.transientCurrent || test.ambiguousFetch {
					missing.Body, missingCommand = "@linkdataminion fix", "fix"
				}
				if test.denyActor {
					missing.Author = github.User{Login: "outsider", ID: 8}
				}
				fix := selectedItem(now.Add(-2 * time.Minute))
				fix.Key, fix.NodeID, fix.Body = "comment:fix", "node:fix", "@linkdataminion fix"
				selected := selectedItem(now.Add(-time.Second))
				selected.Key, selected.NodeID = "comment:selected", "node:selected"
				selected.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-selected"

				missingNotice := testNotification(missing.UpdatedAt)
				missingNotice.ID = "thread-6"
				missingNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/6"
				fixNotice := testNotification(fix.UpdatedAt)
				fixNotice.ID = "thread-7"
				selectedNotice := testNotification(selected.UpdatedAt)
				selectedNotice.ID = "thread-8"
				selectedNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
				missingKey, fixKey := notificationKey(missingNotice), notificationKey(fixNotice)
				missingID, fixID := jobID(missing.Key, missingCommand), jobID(fix.Key, "fix")
				store := openState(t, now.Add(-5*time.Minute))
				for _, notice := range []github.Notification{missingNotice, fixNotice} {
					key := notificationKey(notice)
					pending := state.PendingNotification{
						Repository: notice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
						SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL,
						Since: now.Add(-4 * time.Minute), UpdatedAt: notice.UpdatedAt,
					}
					if key == missingKey {
						pending.RetryAt = now.Add(time.Second)
					}
					store.Data.PendingNotifications[key] = pending
				}
				store.Data.JobFailures[missingID] = state.JobFailure{
					Attempts: 1, AcceptedAt: now.Add(-3 * time.Minute), Deadline: now.Add(time.Second), NotificationKey: missingKey,
				}
				store.Data.JobFailures[fixID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: fixKey}
				store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}

				var missingFetches, fixFetches atomic.Int32
				permissionLookupErr := errors.New("temporary permission lookup failure")
				var gh *fakeGitHub
				gh = &fakeGitHub{
					notifications: []github.Notification{selectedNotice},
					permission:    github.Permission{Permission: "write"},
					threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
						switch number {
						case 6:
							missingFetches.Add(1)
							if test.ambiguousFetch {
								missing.Body, missing.Edited = "@linkdataminion fix\n@linkdataminion review", true
								return []github.Item{missing}, nil
							}
							if test.removedInFetch {
								missing.Body, missing.Edited = "ordinary comment", true
								return []github.Item{missing}, nil
							}
							if test.removedInCurrent || test.transientCurrent || test.denyActor ||
								test.denyPermission || test.transientPermission {
								return []github.Item{missing}, nil
							}
							return nil, nil
						case 7:
							fixFetches.Add(1)
							return []github.Item{fix}, nil
						case 8:
							if missingFetches.Load() != 0 || !store.Data.Processed[missingID].IsZero() {
								t.Errorf("missing source was handled before selected Execute")
							}
							return []github.Item{selected}, nil
						default:
							return nil, fmt.Errorf("unexpected thread #%d", number)
						}
					},
					currentItem: func(item github.Item) (github.Item, error) {
						if test.transientCurrent && item.Key == missing.Key {
							return github.Item{}, errors.New("temporary missing source lookup failure")
						}
						if test.removedInCurrent && item.Key == missing.Key {
							item.Body, item.Edited = "ordinary comment", true
							return item, nil
						}
						if item.Key == missing.Key {
							if test.denyPermission {
								gh.permission.Permission = "read"
							}
							if test.transientPermission {
								gh.permissionErr = permissionLookupErr
							}
						}
						if item.Key == fix.Key {
							return github.Item{}, errors.New("temporary fix source lookup failure")
						}
						return item, nil
					},
				}
				held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
				cfg := testConfig(t)
				cfg.MaxRuntimeMinutes = 7
				if test.denyActor {
					cfg.Repositories[0].AllowedActors = []string{"writer"}
					cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
				}
				m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
					SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := m.RunOnce(ctx, store); result <- err }()
				select {
				case job := <-held.entered:
					if job.ID != jobID(selected.Key, "review") {
						t.Fatalf("unexpected selected action: %+v", job)
					}
				case err := <-result:
					t.Fatalf("selected review did not start: %v", err)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if missingFetches.Load() == 0 || fixFetches.Load() < 2 {
					t.Fatalf("terminal sources were not checked while review ran: missing=%d fix=%d",
						missingFetches.Load(), fixFetches.Load())
				}
				_, missingFailure := store.Data.JobFailures[missingID]
				transient := test.transientCurrent || test.transientPermission
				if transient {
					if !store.Data.Processed[missingID].IsZero() || !missingFailure ||
						store.Data.PendingNotifications[missingKey].Repository == "" {
						t.Fatalf("transient lookup retired the failed job: processed=%v failures=%+v pending=%+v",
							store.Data.Processed, store.Data.JobFailures, store.Data.PendingNotifications)
					}
				} else if store.Data.Processed[missingID].IsZero() || missingFailure {
					t.Fatalf("missing source was not retired: processed=%v failures=%+v",
						store.Data.Processed, store.Data.JobFailures)
				}
				if !store.Data.Processed[fixID].IsZero() || store.Data.JobFailures[fixID].Attempts != maxJobAttempts ||
					store.Data.PendingNotifications[fixKey].Repository == "" || gh.commentCalls != 0 || held.executions.Load() != 1 {
					t.Fatalf("valid fix was lost during review: processed=%v failures=%+v pending=%+v comments=%d executions=%d",
						store.Data.Processed, store.Data.JobFailures, store.Data.PendingNotifications, gh.commentCalls, held.executions.Load())
				}
				close(held.release)
				synctest.Wait()
				if err := <-result; err != nil {
					t.Fatalf("RunOnce after selected review: %v", err)
				}
				gh.notifications = nil
				time.Sleep(time.Minute)
				if _, err := m.RunOnce(t.Context(), store); err != nil &&
					(!test.transientPermission || !errors.Is(err, permissionLookupErr)) {
					t.Fatalf("follow-up poll: %v", err)
				}
				if _, pending := store.Data.PendingNotifications[missingKey]; pending != transient {
					t.Fatalf("missing source pending state after follow-up poll: pending=%t want=%t entries=%+v",
						pending, transient, store.Data.PendingNotifications)
				}
				if store.Data.PendingNotifications[fixKey].Repository == "" || store.Data.JobFailures[fixID].Attempts != maxJobAttempts {
					t.Fatalf("valid unsupported fix was lost after follow-up poll: failures=%+v pending=%+v",
						store.Data.JobFailures, store.Data.PendingNotifications)
				}
			})
		})
	}
}

func TestSelectedReactionRecoveryServicesTerminalSibling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		siblingID := jobID(sibling.Key, "review")
		initial := now.Add(-time.Minute)
		path := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, path, initial)
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: notificationKey(testNotification(selected.UpdatedAt))}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		reactionErr := errors.New("reaction unavailable")
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			items:         []github.Item{selected, sibling},
			permission:    github.Permission{Permission: "write"},
			reactionErr:   reactionErr,
			currentItem: func(seed github.Item) (github.Item, error) {
				switch seed.Key {
				case selected.Key:
					return selected, nil
				case sibling.Key:
					return sibling, nil
				default:
					return github.Item{}, fmt.Errorf("unexpected source %q", seed.Key)
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, reactionErr) {
			t.Fatalf("first run did not fail after acceptance: %v", err)
		}
		accepted := store.Data.Accepted
		if accepted == nil || accepted.JobID != jobID(selected.Key, "review") || held.executions.Load() != 0 ||
			len(store.Data.PendingNotifications) != 1 || store.Data.PendingNotifications[notificationKey(gh.notifications[0])].SubjectURL != gh.notifications[0].Subject.URL {
			t.Fatalf("reaction failure lost accepted action or sibling thread: accepted=%+v pending=%+v executions=%d",
				accepted, store.Data.PendingNotifications, held.executions.Load())
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := openStateAt(t, path, initial)
		gh.reactionErr = nil
		gh.notifications = nil
		time.Sleep(time.Minute + time.Second)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, reopened); result <- err }()
		select {
		case job := <-held.entered:
			if job.ID != accepted.JobID || job.AttemptID != accepted.AttemptID {
				t.Fatalf("recovery replaced accepted action: job=%+v accepted=%+v", job, accepted)
			}
		case err := <-result:
			t.Fatalf("recovery returned before Execute: %v", err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+siblingID+" -->") ||
			reopened.Data.Processed[siblingID].IsZero() || held.executions.Load() != 1 ||
			reopened.Data.Accepted == nil || reopened.Data.Accepted.AttemptID != accepted.AttemptID || len(gh.notificationCalls) != 1 {
			t.Fatalf("recovered Execute missed terminal sibling: comments=%v processed=%v accepted=%+v executions=%d polls=%d",
				gh.comments, reopened.Data.Processed[siblingID], reopened.Data.Accepted, held.executions.Load(), len(gh.notificationCalls))
		}
		select {
		case err := <-result:
			t.Fatalf("recovery returned while Execute was blocked: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("recovery after cancellation: %v", err)
		}
	})
}

func TestSelectedActionSkipsIrrelevantPendingBacklogWhileCheckingInvitations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		store := openState(t, now.Add(-time.Minute))
		for number := 100; number < 164; number++ {
			notice := testNotification(now.Add(-time.Second))
			notice.Subject.URL = fmt.Sprintf("https://api.github.com/repos/example-owner/example-repo/pulls/%d", number)
			store.Data.PendingNotifications[notificationKey(notice)] = state.PendingNotification{
				Repository: notice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
				SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL,
				Since: now.Add(-time.Minute), UpdatedAt: notice.UpdatedAt,
			}
		}
		store.Data.NextInviteCheckAt = now.Add(time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			items:         []github.Item{selected},
			permission:    github.Permission{Permission: "write"},
			invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
				FullName: "example-owner/example-repo", ID: 1,
				Owner: github.User{Login: "example-owner", ID: 2},
			}, Permissions: "write"}},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if gh.repoChecks != 1 || gh.threadCalls != 1 || !slices.Equal(gh.accepted, []int64{41}) ||
			held.executions.Load() != 1 || store.Data.Accepted == nil || len(store.Data.PendingNotifications) != 65 {
			t.Fatalf("irrelevant backlog caused sweeps or blocked invitation: repository calls=%d thread calls=%d invitations=%v executions=%d accepted=%+v pending=%d",
				gh.repoChecks, gh.threadCalls, gh.accepted, held.executions.Load(), store.Data.Accepted, len(store.Data.PendingNotifications))
		}
		select {
		case err := <-result:
			t.Fatalf("RunOnce returned while Execute was blocked: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedActionChecksOnlyTerminalThreadInPendingBacklog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		terminal := selectedItem(now.Add(-time.Second))
		terminal.Key, terminal.NodeID = "comment:2", "node:2"
		terminal.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-2"
		terminalID := jobID(terminal.Key, "review")
		target := testNotification(terminal.UpdatedAt)
		target.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		targetKey := notificationKey(target)
		initial := now.Add(-time.Minute)
		store := openState(t, initial)
		for number := 8; number < 73; number++ {
			notice := testNotification(terminal.UpdatedAt)
			notice.Subject.URL = fmt.Sprintf("https://api.github.com/repos/example-owner/example-repo/pulls/%d", number)
			store.Data.PendingNotifications[notificationKey(notice)] = state.PendingNotification{
				Repository: notice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
				SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL,
				Since: initial, UpdatedAt: notice.UpdatedAt,
			}
		}
		store.Data.JobFailures[terminalID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: targetKey}
		store.Data.NextInviteCheckAt = now.Add(time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			items:         []github.Item{selected},
			permission:    github.Permission{Permission: "write"},
			invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
				FullName: "example-owner/example-repo", ID: 1,
				Owner: github.User{Login: "example-owner", ID: 2},
			}, Permissions: "write"}},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				switch number {
				case 7:
					return []github.Item{selected}, nil
				case 8:
					return []github.Item{terminal}, nil
				default:
					return nil, nil
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if gh.repoChecks != 4 || gh.threadCalls != 2 || len(gh.comments) != 1 ||
			!strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+terminalID+" -->") ||
			store.Data.Processed[terminalID].IsZero() || !slices.Equal(gh.accepted, []int64{41}) ||
			held.executions.Load() != 1 || store.Data.Accepted == nil {
			t.Fatalf("pending backlog swept or terminal control missed: repository calls=%d thread calls=%d comments=%v processed=%v invitations=%v executions=%d accepted=%+v",
				gh.repoChecks, gh.threadCalls, gh.comments, store.Data.Processed[terminalID], gh.accepted, held.executions.Load(), store.Data.Accepted)
		}
		select {
		case err := <-result:
			t.Fatalf("RunOnce returned while Execute was blocked: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedRetryKeepsOneShotReplyAndServicesDueInvitations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		terminal := selectedItem(now.Add(-time.Second))
		terminal.Key, terminal.NodeID = "comment:2", "node:2"
		terminal.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-2"
		terminalID := jobID(terminal.Key, "review")
		notice := testNotification(terminal.UpdatedAt)
		notice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		key := notificationKey(notice)
		store := openState(t, now.Add(-time.Minute))
		store.Data.PendingNotifications[key] = state.PendingNotification{
			Repository: notice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
			SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL,
			Since: now.Add(-time.Minute), UpdatedAt: notice.UpdatedAt, RetryAt: now.Add(time.Minute),
		}
		store.Data.JobFailures[terminalID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: key}
		store.Data.NextInviteCheckAt = now.Add(time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			permission:    github.Permission{Permission: "write"},
			invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
				FullName: "example-owner/example-repo", ID: 1,
				Owner: github.User{Login: "example-owner", ID: 2},
			}, Permissions: "write"}},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				switch number {
				case 7:
					return []github.Item{selected}, nil
				case 8:
					return []github.Item{terminal}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		handler := selectedHandler(func(Job) error {
			close(entered)
			<-release
			return ErrAcceptedRetry
		})
		clock := now
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: handler, Now: func() time.Time { return clock },
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); result <- err }()
		select {
		case <-entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		synctest.Wait()
		if len(gh.comments) != 1 || store.Data.Processed[terminalID].IsZero() {
			t.Fatalf("terminal reply did not finish on its first pass: %v", gh.comments)
		}
		clock = now.Add(time.Minute)
		close(release)
		released = true
		synctest.Wait()
		if err := <-result; !errors.Is(err, ErrAcceptedRetry) {
			t.Fatalf("RunOnce did not retain deferred assignment: %v", err)
		}
		if !slices.Equal(gh.accepted, []int64{41}) || len(gh.comments) != 1 ||
			!strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+terminalID+" -->") ||
			store.Data.Processed[terminalID].IsZero() || store.Data.Accepted == nil {
			t.Fatalf("due controls were skipped as Execute returned: invitations=%v comments=%v processed=%v accepted=%+v",
				gh.accepted, gh.comments, store.Data.Processed[terminalID], store.Data.Accepted)
		}
	})
}

func TestSelectedReviewKeepsOlderTerminalFixForControlReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		fix := selectedItem(now.Add(-2 * time.Second))
		fix.Key, fix.NodeID = "comment:old-fix", "node:old-fix"
		fix.Body = "@linkdataminion fix"
		fix.HTMLURL = "https://github.com/example-owner/example-repo/pull/6#issuecomment-old-fix"
		review := selectedItem(now.Add(-time.Second))
		fixNotice := testNotification(fix.UpdatedAt)
		fixNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/6"
		fixKey := notificationKey(fixNotice)
		fixID := jobID(fix.Key, "fix")
		store := openState(t, now.Add(-time.Minute))
		store.Data.PendingNotifications[fixKey] = state.PendingNotification{
			Repository: fixNotice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
			SubjectType: fixNotice.Subject.Type, SubjectURL: fixNotice.Subject.URL,
			Since: now.Add(-time.Minute), UpdatedAt: fix.UpdatedAt,
		}
		store.Data.JobFailures[fixID] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: fixKey}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(review.UpdatedAt)},
			permission:    github.Permission{Permission: "write"},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				switch number {
				case 6:
					return []github.Item{fix}, nil
				case 7:
					return []github.Item{review}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
		}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("review did not start: %v", err)
		}
		synctest.Wait()
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+fixID+" -->") ||
			store.Data.Processed[fixID].IsZero() || store.Data.Accepted == nil || held.executions.Load() != 1 {
			t.Fatalf("older terminal fix was lost while review ran: comments=%v processed=%v accepted=%+v executions=%d",
				gh.comments, store.Data.Processed[fixID], store.Data.Accepted, held.executions.Load())
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestTerminalReplyRemainsOneShotAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		item := selectedItem(now)
		path := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, path, now.Add(-time.Minute))
		cfg := testConfig(t)
		job := Job{Repository: cfg.Repositories[0], SubjectType: "PullRequest", Number: 7, Source: item,
			Command: command.Command{Name: "review", Action: command.Review}, ID: jobID(item.Key, "review")}
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: maxJobAttempts}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{items: []github.Item{item}, permission: github.Permission{Permission: "write"},
			commentErr: errors.New("response lost")}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Now: time.Now}
		if err := m.finishFailedJob(t.Context(), store, job, now); err != nil {
			t.Fatal(err)
		}
		write := store.Data.PublicationWrites[job.ID]
		if gh.commentCalls != 1 || store.Data.Processed[job.ID].IsZero() ||
			write.Status != state.PublicationWritePending {
			t.Fatalf("uncertain POST was not checkpointed: calls=%d state=%+v", gh.commentCalls, store.Data)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, now)
		gh.commentErr = nil
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if err := m.finishFailedJob(t.Context(), store, job, now); err != nil ||
			gh.commentCalls != 1 || store.Data.PublicationWrites[job.ID].Status != state.PublicationWritePending {
			t.Fatalf("restart repeated uncertain POST: err=%v calls=%d write=%+v", err, gh.commentCalls, store.Data.PublicationWrites[job.ID])
		}
		time.Sleep(write.Deadline.Sub(time.Now()) + time.Second)
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil ||
			gh.commentCalls != 1 || store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteUncertain {
			t.Fatalf("absent marker authorized a second POST: err=%v calls=%d write=%+v", err, gh.commentCalls, store.Data.PublicationWrites[job.ID])
		}
	})
}

func TestSelectedControlClearsResolvedScanReplyError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		terminal := selectedItem(now.Add(-2 * time.Second))
		terminal.Key, terminal.NodeID = "comment:terminal", "node:terminal"
		review := selectedItem(now.Add(-time.Second))
		id := jobID(terminal.Key, "review")
		store := openState(t, now.Add(-time.Minute))
		store.Data.JobFailures[id] = state.JobFailure{Attempts: maxJobAttempts, NotificationKey: notificationKey(testNotification(review.UpdatedAt))}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		notice := testNotification(review.UpdatedAt)
		base := &fakeGitHub{
			notifications: []github.Notification{notice},
			items:         []github.Item{terminal, review},
			permission:    github.Permission{Permission: "write"},
			commentErr:    errors.New("temporary comment failure"),
		}
		gh := &synchronizedCommentsGitHub{fakeGitHub: base}
		held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Handler: held,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); result <- err }()
		select {
		case <-held.entered:
		case err := <-result:
			t.Fatalf("review did not start: %v", err)
		}
		synctest.Wait()
		var processed time.Time
		if err := store.WithLock(func() error {
			processed = store.Data.Processed[id]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		calls, _ := gh.commentsSnapshot()
		if calls != 1 || processed.IsZero() || store.Data.PublicationWrites[id].Status != state.PublicationWritePending {
			t.Fatalf("first reply failure did not retain an issued write: calls=%d processed=%v pending=%+v",
				calls, processed, store.Data.PendingNotifications)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if err := store.WithLock(func() error {
			processed = store.Data.Processed[id]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		calls, _ = gh.commentsSnapshot()
		if calls != 1 || processed.IsZero() {
			t.Fatalf("terminal reply repeated after uncertain POST: calls=%d processed=%v",
				calls, processed)
		}
		synctest.Wait()
		close(held.release)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatalf("resolved terminal reply still returned scan error: %v", err)
		}
		if gh.commentCalls != 1 || len(gh.comments) != 0 ||
			store.Data.Processed[id].IsZero() || len(store.Data.PendingNotifications) != 0 {
			t.Fatalf("uncertain terminal reply was repeated or re-deferred: calls=%d comments=%v processed=%v pending=%+v",
				gh.commentCalls, gh.comments, store.Data.Processed[id], store.Data.PendingNotifications)
		}
	})
}
