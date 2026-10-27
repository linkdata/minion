package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

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
		if fetched[7] == 0 || fetched[8] == 0 || held.executions.Load() != 1 {
			t.Fatalf("threads were not fetched while Execute was blocked: fetched=%v executions=%d", fetched, held.executions.Load())
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
		}
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
			beforeComment: func() {
				if time.Now().Before(deadline) {
					t.Error("sibling received terminal reply before its deadline")
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
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
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

func TestSelectedActionRetriesTerminalSiblingReplyWhileBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		siblingID := jobID(sibling.Key, "review")
		store := openState(t, now.Add(-time.Minute))
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts}
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		var gh *fakeGitHub
		gh = &fakeGitHub{
			notifications: []github.Notification{testNotification(selected.UpdatedAt)},
			items:         []github.Item{selected, sibling},
			permission:    github.Permission{Permission: "write"},
			commentErr:    errors.New("temporary reply failure"),
			beforeComment: func() {
				if gh.commentCalls == 2 {
					gh.commentErr = nil
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
		case <-held.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if gh.commentCalls != 2 || len(gh.comments) != 1 ||
			!strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+siblingID+" -->") ||
			store.Data.Processed[siblingID].IsZero() || held.executions.Load() != 1 || store.Data.Accepted == nil {
			t.Fatalf("terminal sibling reply was not retried once: calls=%d comments=%v processed=%v executions=%d accepted=%+v",
				gh.commentCalls, gh.comments, store.Data.Processed[siblingID], held.executions.Load(), store.Data.Accepted)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if gh.commentCalls != 2 || len(gh.comments) != 1 {
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
