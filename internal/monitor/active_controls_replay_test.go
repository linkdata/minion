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
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts}
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
		store.Data.JobFailures[fixID] = state.JobFailure{Attempts: maxJobAttempts}
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
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts}
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
		m := &Monitor{Config: cfg, GitHub: gh, Handler: held,
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
		if gh.repoChecks != 3 || gh.threadCalls != 2 || len(gh.comments) != 1 ||
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
