package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type recoveredResultHandler struct {
	executions   int
	publications int
	execute      func(Job) error
	publish      func(Job) error
}

// fakeGitHub counts these reads without locking; recovery and terminal controls run concurrently.
type synchronizedRecoveryGitHub struct {
	*fakeGitHub
	mu sync.Mutex
}

func (g *synchronizedRecoveryGitHub) Repository(ctx context.Context, name string) (github.Repository, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.Repository(ctx, name)
}

func (g *synchronizedRecoveryGitHub) CurrentItem(ctx context.Context, item github.Item) (github.Item, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.CurrentItem(ctx, item)
}

func (g *synchronizedRecoveryGitHub) Permission(ctx context.Context, repo, login string) (github.Permission, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.Permission(ctx, repo, login)
}

func (h *recoveredResultHandler) Execute(_ context.Context, job Job) error {
	h.executions++
	if h.execute != nil {
		return h.execute(job)
	}
	return errors.New("stored result was executed again")
}

func (h *recoveredResultHandler) PublishRecovered(_ context.Context, job Job) error {
	h.publications++
	if h.publish != nil {
		return h.publish(job)
	}
	return nil
}

func (*recoveredResultHandler) WaitCapacity(context.Context) error { return nil }

func storedRecoveryFixture(t *testing.T) (*state.Store, string, state.AcceptedAction, *fakeGitHub, *Monitor) {
	t.Helper()
	return storedRecoveryFixtureAt(t, time.Now().UTC())
}

func storedRecoveryFixtureAt(t *testing.T, at time.Time) (*state.Store, string, state.AcceptedAction, *fakeGitHub, *Monitor) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	store.Data.NextPollAt = at.Add(time.Hour)
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult([]byte(`{"result":"opaque-to-monitor"}`)); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{
		permission:  github.Permission{Permission: "write"},
		currentItem: func(github.Item) (github.Item, error) { return selectedItem(at), nil },
	}
	return store, path, accepted, gh, selectedMonitor(t, at, gh, nil)
}

func TestSelectedExecutionKeepsSavedResultAfterPublicationError(t *testing.T) {
	at := time.Now().UTC()
	store := openState(t, at.Add(-time.Minute))
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(at)},
		items:         []github.Item{selectedItem(at)},
		permission:    github.Permission{Permission: "write"},
		currentItem:   func(github.Item) (github.Item, error) { return selectedItem(at), nil },
	}
	publishErr := errors.New("publication unavailable")
	handler := &recoveredResultHandler{execute: func(job Job) error {
		if err := store.WithLock(func() error {
			accepted := store.Data.Accepted
			if accepted == nil || accepted.JobID != job.ID || accepted.AttemptID != job.AttemptID {
				return errors.New("execution lost accepted reservation")
			}
			accepted.Assigned = true
			if err := store.Save(); err != nil {
				return err
			}
			return store.WriteAcceptedResult([]byte(`{"result":"saved-before-error"}`))
		}); err != nil {
			return err
		}
		return errors.Join(ErrPostModelPublication, publishErr)
	}}
	monitor := selectedMonitor(t, at, gh, handler)
	next, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, publishErr) || !errors.Is(err, ErrPostModelPublication) ||
		store.Data.Accepted == nil || !store.Data.Accepted.Assigned ||
		!next.Equal(store.Data.Accepted.RetryAt) || !next.After(time.Now()) ||
		store.Data.JobFailures[store.Data.Accepted.JobID].Attempts != 1 ||
		len(store.Data.Processed) != 0 || len(gh.comments) != 0 ||
		handler.executions != 1 || handler.publications != 0 {
		t.Fatalf("post-model publication error lost saved result: next=%s err=%v state=%+v handler=%+v", next, err, store.Data, handler)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("post-model publication error lost sidecar: present=%t err=%v", present, err)
	}
}

func TestSelectedExecutionEndsPermanentSavedResult(t *testing.T) {
	at := time.Now().UTC()
	store := openState(t, at.Add(-time.Minute))
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(at)},
		items:         []github.Item{selectedItem(at)},
		permission:    github.Permission{Permission: "write"},
		currentItem:   func(github.Item) (github.Item, error) { return selectedItem(at), nil },
	}
	handler := &recoveredResultHandler{execute: func(job Job) error {
		if err := store.WithLock(func() error {
			store.Data.Accepted.Assigned = true
			if err := store.Save(); err != nil {
				return err
			}
			return store.WriteAcceptedResult([]byte(`{"result":"saved-before-error"}`))
		}); err != nil {
			return err
		}
		return errors.Join(ErrTerminalJob, ErrPostModelPublication, errors.New("reviewed fix has no changes"))
	}}
	monitor := selectedMonitor(t, at, gh, handler)
	if _, err := monitor.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 1 || handler.publications != 0 || store.Data.Accepted != nil ||
		len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || len(gh.comments) != 1 {
		t.Fatalf("permanent result kept retrying: handler=%+v state=%+v comments=%v", handler, store.Data, gh.comments)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("permanent result kept receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryPublishesAfterRestartWithoutWorkerSkill(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".agents", "skills", "review", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	monitor.SelectedCommands = map[string]bool{"fix": true}
	handler := &recoveredResultHandler{publish: func(job Job) error {
		if job.ID != accepted.JobID || job.AttemptID != accepted.AttemptID ||
			job.Source.Key != accepted.SourceKey || job.Source.Author.ID != accepted.RequesterID ||
			!job.Source.UpdatedAt.Equal(accepted.SourceUpdatedAt) ||
			!job.Deadline.Equal(accepted.Deadline) {
			t.Fatalf("publication lost accepted identity: %+v", job)
		}
		present, err := reopened.HasAcceptedResult()
		if err != nil || !present {
			t.Fatalf("publication started without saved result: present=%t err=%v", present, err)
		}
		return nil
	}}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 1 || gh.currentChecks != 1 || gh.checks != 1 ||
		len(gh.comments) != 0 || len(gh.reactions) != 0 {
		t.Fatalf("saved result was not published exactly once: handler=%+v github=%+v", handler, gh)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	finished := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	present, err := finished.HasAcceptedResult()
	if err != nil || present || finished.Data.Accepted != nil ||
		finished.Data.Processed[accepted.JobID].IsZero() || len(finished.Data.JobFailures) != 0 {
		t.Fatalf("publication was not durably completed: present=%t err=%v state=%+v", present, err, finished.Data)
	}
	if _, err := monitor.RunOnce(t.Context(), finished); err != nil || handler.publications != 1 {
		t.Fatalf("completed result published again: err=%v publications=%d", err, handler.publications)
	}
}

func TestStoredResultRecoveryPublishesAfterAcceptedDeadline(t *testing.T) {
	at := time.Now().UTC().Add(-8 * time.Minute)
	store, path, accepted, gh, monitor := storedRecoveryFixtureAt(t, at)
	if !accepted.Deadline.Before(time.Now()) {
		t.Fatalf("accepted deadline did not expire: %s", accepted.Deadline)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	monitor.Now = time.Now
	handler := &recoveredResultHandler{publish: func(job Job) error {
		if job.AttemptID != accepted.AttemptID || !job.Deadline.Equal(accepted.Deadline) {
			t.Fatalf("publication lost expired accepted attempt: %+v", job)
		}
		return nil
	}}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 1 || gh.currentChecks != 1 || gh.checks != 1 ||
		len(gh.comments) != 0 || reopened.Data.Accepted != nil ||
		reopened.Data.Processed[accepted.JobID].IsZero() || len(reopened.Data.JobFailures) != 0 {
		t.Fatalf("expired accepted result did not take publication path: handler=%+v github=%+v state=%+v", handler, gh, reopened.Data)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("completed result kept receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryServicesInvitationWhileRecoveryIsHeld(t *testing.T) {
	for _, phase := range []string{"validation", "publication"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, monitor := storedRecoveryFixtureAt(t, now)
				monitor.Now = time.Now
				store.Data.NextInviteCheckAt = now.Add(time.Minute)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				checked := make(chan struct{}, 1)
				gh.beforeList = func() { checked <- struct{}{} }
				entered, release := make(chan struct{}, 1), make(chan struct{})
				released := false
				defer func() {
					if !released {
						close(release)
					}
				}()
				handler := &recoveredResultHandler{}
				if phase == "validation" {
					gh.beforeRepository = func() {
						entered <- struct{}{}
						<-release
					}
				} else {
					handler.publish = func(Job) error {
						entered <- struct{}{}
						<-release
						return nil
					}
				}
				monitor.Handler = handler
				finished := make(chan error, 1)
				go func() { _, err := monitor.RunOnce(t.Context(), store); finished <- err }()
				select {
				case <-entered:
				case err := <-finished:
					t.Fatalf("%s did not start: %v", phase, err)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				select {
				case <-checked:
				default:
					t.Fatalf("due invitation was not checked during held %s", phase)
				}
				select {
				case err := <-finished:
					t.Fatalf("held %s returned before release: %v", phase, err)
				default:
				}
				close(release)
				released = true
				if err := <-finished; err != nil || handler.executions != 0 || handler.publications != 1 ||
					store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
					t.Fatalf("held %s did not finish: err=%v handler=%+v state=%+v", phase, err, handler, store.Data)
				}
			})
		})
	}
}

func TestStoredResultRecoveryServicesTerminalReplyWhilePublicationIsHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, monitor := storedRecoveryFixtureAt(t, now)
		monitor.Now = time.Now
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		notice := testNotification(sibling.UpdatedAt)
		key := notificationKey(notice)
		store.Data.PendingNotifications[key] = state.PendingNotification{
			Repository: notice.Repository.FullName, RepositoryID: accepted.RepositoryID, OwnerID: accepted.OwnerID,
			SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL, Since: now.Add(-time.Minute), UpdatedAt: notice.UpdatedAt,
		}
		store.Data.JobFailures[jobID(sibling.Key, "review")] = state.JobFailure{
			Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(time.Minute), NotificationKey: key,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.items = []github.Item{sibling}
		gh.currentItem = func(item github.Item) (github.Item, error) {
			if item.Key == sibling.Key {
				return sibling, nil
			}
			return selectedItem(now), nil
		}
		monitor.GitHub = &synchronizedRecoveryGitHub{fakeGitHub: gh}
		commented := make(chan struct{}, 1)
		gh.beforeComment = func() { commented <- struct{}{} }
		entered, release := make(chan struct{}, 1), make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		handler := &recoveredResultHandler{publish: func(Job) error {
			entered <- struct{}{}
			<-release
			return nil
		}}
		monitor.Handler = handler
		finished := make(chan error, 1)
		go func() { _, err := monitor.RunOnce(t.Context(), store); finished <- err }()
		select {
		case <-entered:
		case err := <-finished:
			t.Fatalf("publication did not start: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case <-commented:
		default:
			t.Fatal("due terminal reply was not posted during held publication")
		}
		select {
		case err := <-finished:
			t.Fatalf("held publication returned before release: %v", err)
		default:
		}
		close(release)
		released = true
		if err := <-finished; err != nil || handler.executions != 0 || handler.publications != 1 ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(gh.comments) != 1 {
			t.Fatalf("terminal reply or publication failed: err=%v handler=%+v state=%+v comments=%v", err, handler, store.Data, gh.comments)
		}
	})
}

func TestStoredResultRecoveryRetriesPublicationWithoutExecution(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	publishErr := errors.New("publication unavailable")
	publishAttempts := 0
	handler := &recoveredResultHandler{publish: func(Job) error {
		publishAttempts++
		if publishAttempts == 1 {
			return publishErr
		}
		return nil
	}}
	monitor.Handler = handler
	next, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, publishErr) || !errors.Is(err, ErrPostModelPublication) ||
		store.Data.Accepted == nil || store.Data.Accepted.AttemptID != accepted.AttemptID ||
		!next.Equal(store.Data.Accepted.RetryAt) || !next.After(time.Now()) ||
		store.Data.JobFailures[accepted.JobID].Attempts != 1 || handler.executions != 0 || handler.publications != 1 ||
		len(gh.comments) != 0 {
		t.Fatalf("publication failure did not retain a scheduled result: next=%s err=%v state=%+v handler=%+v", next, err, store.Data, handler)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("failed publication lost sidecar: present=%t err=%v", present, err)
	}
	if _, err := monitor.RunOnce(t.Context(), store); err != nil || handler.publications != 1 {
		t.Fatalf("publication retried before due time: err=%v publications=%d", err, handler.publications)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.RetryAt.Equal(next) {
		t.Fatalf("publication retry was not durable: %+v", reopened.Data)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("restart lost saved result: present=%t err=%v", present, err)
	}
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil || handler.executions != 0 || handler.publications != 2 ||
		reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() || len(gh.comments) != 0 {
		t.Fatalf("due publication did not finish saved result: err=%v handler=%+v state=%+v", err, handler, reopened.Data)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("successful retry kept sidecar: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryEndsPermanentPublicationFailure(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	handler := &recoveredResultHandler{publish: func(Job) error {
		return errors.Join(ErrTerminalJob, ErrPostModelPublication, errors.New("reviewed fix has no changes"))
	}}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 1 || reopened.Data.Accepted != nil ||
		reopened.Data.Processed[accepted.JobID].IsZero() || len(reopened.Data.JobFailures) != 0 || len(gh.comments) != 1 {
		t.Fatalf("permanent saved result kept retrying: handler=%+v state=%+v comments=%v", handler, reopened.Data, gh.comments)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("permanent saved result kept receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryRetainsReceiptOnPublicationDeadline(t *testing.T) {
	at := time.Now().UTC().Add(-8 * time.Minute)
	store, _, accepted, _, monitor := storedRecoveryFixtureAt(t, at)
	handler := &recoveredResultHandler{publish: func(Job) error {
		return errors.Join(ErrTerminalJob, ErrPostModelPublication, context.DeadlineExceeded)
	}}
	monitor.Handler = handler
	next, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrPostModelPublication) ||
		store.Data.Accepted == nil || !next.Equal(store.Data.Accepted.RetryAt) ||
		store.Data.JobFailures[accepted.JobID].Attempts != 1 || handler.executions != 0 || handler.publications != 1 {
		t.Fatalf("publication timeout lost receipt: next=%s err=%v state=%+v handler=%+v", next, err, store.Data, handler)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("publication timeout lost receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryDiscardsInvalidSourceOrRequester(t *testing.T) {
	for _, test := range []struct {
		name             string
		change           func(*fakeGitHub, time.Time)
		permissionChecks int
	}{
		{"edited source", func(gh *fakeGitHub, at time.Time) {
			gh.currentItem = func(github.Item) (github.Item, error) {
				item := selectedItem(at)
				item.Edited = true
				return item, nil
			}
		}, 0},
		{"revoked requester", func(gh *fakeGitHub, _ time.Time) {
			gh.permission.Permission = "read"
		}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path, accepted, gh, monitor := storedRecoveryFixture(t)
			test.change(gh, accepted.AcceptedAt)
			handler := &recoveredResultHandler{}
			monitor.Handler = handler
			if _, err := monitor.RunOnce(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if handler.executions != 0 || handler.publications != 0 || gh.currentChecks != 1 ||
				gh.checks != test.permissionChecks || len(gh.comments) != 0 {
				t.Fatalf("invalid command reached publisher: handler=%+v github=%+v", handler, gh)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
			present, err := reopened.HasAcceptedResult()
			if err != nil || present || reopened.Data.Accepted != nil ||
				reopened.Data.Processed[accepted.JobID].IsZero() || len(reopened.Data.JobFailures) != 0 {
				t.Fatalf("invalid command was not durably discarded: present=%t err=%v state=%+v", present, err, reopened.Data)
			}
		})
	}
}

func TestStoredResultRecoveryStopsOnUnusableSidecarOrPublisher(t *testing.T) {
	for _, test := range []struct {
		name          string
		change        func(*testing.T, string, *Monitor)
		currentChecks int
	}{
		{"nonregular sidecar", func(t *testing.T, path string, monitor *Monitor) {
			if err := os.Remove(path + ".result"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path+".result", 0700); err != nil {
				t.Fatal(err)
			}
			monitor.Handler = &recoveredResultHandler{}
		}, 0},
		{"missing publisher", func(_ *testing.T, _ string, monitor *Monitor) {
			monitor.Handler = selectedHandler(func(Job) error { return errors.New("stored result was executed again") })
		}, 0},
		{"publisher rejects corrupt receipt", func(_ *testing.T, _ string, monitor *Monitor) {
			monitor.Handler = &recoveredResultHandler{publish: func(Job) error {
				return errors.Join(ErrAcceptanceState, errors.New("corrupt saved result"))
			}}
		}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path, accepted, gh, monitor := storedRecoveryFixture(t)
			test.change(t, path, monitor)
			if _, err := monitor.RunOnce(t.Context(), store); !errors.Is(err, ErrAcceptanceState) {
				t.Fatalf("unsafe result state did not stop recovery: %v", err)
			}
			if store.Data.Accepted == nil || store.Data.Accepted.AttemptID != accepted.AttemptID ||
				!store.Data.Accepted.RetryAt.IsZero() || len(store.Data.Processed) != 0 ||
				gh.currentChecks != test.currentChecks || len(gh.comments) != 0 {
				t.Fatalf("unsafe result state changed reservation or published: %+v", store.Data)
			}
			if _, err := os.Lstat(path + ".result"); err != nil {
				t.Fatalf("unsafe result sidecar was removed: %v", err)
			}
		})
	}
}
