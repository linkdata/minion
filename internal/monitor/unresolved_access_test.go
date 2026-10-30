package monitor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func unresolvedAcceptedFixture(t *testing.T, at time.Time) (string, *state.Store, state.AcceptedAction, *fakeGitHub, *Monitor) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	accepted.FixedRefusal = "This request cannot be completed."
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{
		items:      []github.Item{selectedItem(at)},
		permission: github.Permission{Permission: "write"},
		currentItem: func(github.Item) (github.Item, error) {
			return selectedItem(at), nil
		},
	}
	m := selectedMonitor(t, at, gh, nil)
	return path, store, accepted, gh, m
}

func TestAcceptedPrePOSTAccessFailurePersistsOriginalDisposition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stage      string
		wantStatus string
		setup      func(*fakeGitHub)
	}{
		{name: "reply marker 404", stage: "check existing bot replies", setup: func(gh *fakeGitHub) {
			gh.beforeVerify = func(context.Context, bool) error {
				return &github.StatusError{StatusCode: http.StatusNotFound}
			}
		}},
		{name: "reply marker 404 without display URL", stage: "check existing bot replies", setup: func(gh *fakeGitHub) {
			gh.beforeVerify = func(context.Context, bool) error {
				return &github.StatusError{StatusCode: http.StatusNotFound}
			}
		}},
		{name: "source 404", stage: "revalidate source", setup: func(gh *fakeGitHub) {
			gh.currentItem = func(github.Item) (github.Item, error) {
				return github.Item{}, &github.StatusError{StatusCode: http.StatusNotFound}
			}
			gh.threadItems = func(string, int, time.Time) ([]github.Item, error) {
				return nil, &github.StatusError{StatusCode: http.StatusNotFound}
			}
		}},
		{name: "source 404 then thread 503", stage: "revalidate source", wantStatus: "HTTP 503", setup: func(gh *fakeGitHub) {
			gh.currentItem = func(github.Item) (github.Item, error) {
				return github.Item{}, &github.StatusError{StatusCode: http.StatusNotFound}
			}
			gh.threadItems = func(string, int, time.Time) ([]github.Item, error) {
				return nil, &github.StatusError{StatusCode: http.StatusServiceUnavailable}
			}
		}},
		{name: "source 301 then thread 503", stage: "revalidate source", wantStatus: "HTTP 503", setup: func(gh *fakeGitHub) {
			gh.currentItem = func(github.Item) (github.Item, error) {
				return github.Item{}, &github.StatusError{StatusCode: http.StatusMovedPermanently}
			}
			gh.threadItems = func(string, int, time.Time) ([]github.Item, error) {
				return nil, &github.StatusError{StatusCode: http.StatusServiceUnavailable}
			}
		}},
		{name: "requester permission 404", stage: "revalidate source", setup: func(gh *fakeGitHub) {
			gh.permissionErr = &github.StatusError{StatusCode: http.StatusNotFound}
		}},
		{name: "repository lookup error", stage: "repository", setup: func(gh *fakeGitHub) {
			gh.beforeRepository = func() {
				if gh.repoChecks == 2 {
					gh.repositoryErr = &github.StatusError{StatusCode: http.StatusServiceUnavailable}
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC()
			path, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
			if tc.name == "reply marker 404 without display URL" {
				accepted.SourceURL = ""
				store.Data.Accepted.SourceURL = ""
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
			}
			tc.setup(gh)
			var logs bytes.Buffer
			m.Logger = log.New(&logs, "", 0)
			firstAt := at.Add(-time.Second)
			first, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "canceled", firstAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStateAt(t, path, at.Add(-time.Minute))
			if err := m.recoverAccepted(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			decision := store.Data.TerminalDecisions[accepted.JobID]
			if decision.Outcome != first.Outcome || decision.AttemptID != first.AttemptID || !decision.DecidedAt.Equal(first.DecidedAt) {
				t.Fatalf("access error replaced the first terminal decision: first=%+v final=%+v", first, decision)
			}
			access := decision.UnresolvedAccess
			if access.Repository != accepted.Repository || access.SourceURL != accepted.SourceURL || access.SourceKey != accepted.SourceKey ||
				access.ReplyState != "not_issued" || access.Reason == "" || access.OperatorAction == "" ||
				!strings.Contains(strings.ToLower(access.Reason), tc.stage) {
				t.Fatalf("missing original access disposition: accepted=%+v access=%+v", accepted, access)
			}
			if tc.wantStatus != "" && !strings.Contains(access.Reason, tc.wantStatus) {
				t.Fatalf("access reason=%q, want %s", access.Reason, tc.wantStatus)
			}
			if store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
				len(store.Data.PublicationWrites) != 0 || gh.commentCalls != 0 {
				t.Fatalf("access error issued a reply or left admission held: state=%+v comments=%d", store.Data, gh.commentCalls)
			}
			if !strings.Contains(logs.String(), accepted.SourceKey) || !strings.Contains(logs.String(), accepted.AttemptID) {
				t.Fatalf("operator log omits original request identity: %q", logs.String())
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStateAt(t, path, at.Add(-time.Minute))
			if got := store.Data.TerminalDecisions[accepted.JobID]; got != decision ||
				store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
				t.Fatalf("restart lost unresolved access or reopened admission: got=%+v state=%+v", got, store.Data)
			}
		})
	}
}

func TestAcceptedTemporaryAccessLossCanRecoverBeforeTerminal(t *testing.T) {
	at := time.Now().UTC()
	_, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
	store.Data.Accepted.FixedRefusal = ""
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh.repositoryErr = &github.StatusError{StatusCode: http.StatusNotFound}
	var executions int
	m.Handler = selectedHandler(func(Job) error { executions++; return nil })
	if err := m.recoverAccepted(t.Context(), store); err == nil ||
		store.Data.Accepted == nil || !store.Data.Processed[accepted.JobID].IsZero() || executions != 0 {
		t.Fatalf("temporary access error lost the original acceptance: err=%v state=%+v executions=%d", err, store.Data, executions)
	}
	gh.repositoryErr = nil
	if err := m.recoverAccepted(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	decision := store.Data.TerminalDecisions[accepted.JobID]
	if executions != 1 || decision.Outcome != "succeeded" || decision.UnresolvedAccess != (state.AccessDisposition{}) ||
		store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("recovered access left a false unresolved disposition: executions=%d state=%+v", executions, store.Data)
	}
}

func TestAcceptedDefiniteMoveOrDeletionHasNoUnresolvedDisposition(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeGitHub)
	}{
		{name: "repository moved 301", setup: func(gh *fakeGitHub) {
			gh.repositoryErr = &github.StatusError{StatusCode: http.StatusMovedPermanently}
		}},
		{name: "repository gone 410", setup: func(gh *fakeGitHub) {
			gh.repositoryErr = &github.StatusError{StatusCode: http.StatusGone}
		}},
		{name: "source absent from readable thread", setup: func(gh *fakeGitHub) {
			gh.currentItem = func(github.Item) (github.Item, error) {
				return github.Item{}, &github.StatusError{StatusCode: http.StatusNotFound}
			}
			gh.threadItems = func(string, int, time.Time) ([]github.Item, error) { return nil, nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC()
			_, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
			store.Data.Accepted.FixedRefusal = ""
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			tc.setup(gh)
			m.Handler = selectedHandler(func(Job) error { t.Fatal("definitely absent source dispatched"); return nil })
			if err := m.recoverAccepted(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			decision := store.Data.TerminalDecisions[accepted.JobID]
			if decision.Outcome != "lost" || decision.UnresolvedAccess != (state.AccessDisposition{}) ||
				store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || gh.commentCalls != 0 {
				t.Fatalf("definite move or deletion became unresolved access: state=%+v comments=%d", store.Data, gh.commentCalls)
			}
		})
	}
}

func TestAcceptedDefinitePrePOSTStatusHasNoUnresolvedDisposition(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			at := time.Now().UTC()
			_, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
			if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "failed", at); err != nil {
				t.Fatal(err)
			}
			gh.beforeVerify = func(context.Context, bool) error {
				return &github.StatusError{StatusCode: status}
			}
			if err := m.recoverAccepted(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			decision := store.Data.TerminalDecisions[accepted.JobID]
			if decision.Outcome != "failed" || decision.UnresolvedAccess != (state.AccessDisposition{}) ||
				store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
				len(store.Data.PublicationWrites) != 0 || gh.commentCalls != 0 {
				t.Fatalf("definite pre-POST status became unresolved access: state=%+v comments=%d", store.Data, gh.commentCalls)
			}
		})
	}
}

func TestAcceptedDispositionSaveFailureThenAccessRecovery(t *testing.T) {
	at := time.Now().UTC()
	path, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
	if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "failed", at); err != nil {
		t.Fatal(err)
	}
	backup := path + ".before-final-save"
	blocked := false
	gh.beforeVerify = func(context.Context, bool) error {
		if !blocked {
			blocked = true
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		return &github.StatusError{StatusCode: http.StatusNotFound}
	}
	err := m.recoverAccepted(t.Context(), store)
	if !errors.Is(err, ErrAcceptanceState) || !blocked || store.Data.Accepted == nil ||
		store.Data.Processed[accepted.JobID] != (time.Time{}) || gh.commentCalls != 0 {
		t.Fatalf("failed final save released admission: err=%v state=%+v comments=%d", err, store.Data, gh.commentCalls)
	}
	if store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess != (state.AccessDisposition{}) {
		t.Fatal("failed final save left an unresolved marker in memory")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	gh.beforeVerify = nil
	if err := m.recoverAccepted(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
		store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess != (state.AccessDisposition{}) || gh.commentCalls != 1 {
		t.Fatalf("recovered access left a false unresolved marker or skipped the reply: state=%+v comments=%d", store.Data, gh.commentCalls)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, at.Add(-time.Minute))
	if store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess != (state.AccessDisposition{}) ||
		store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("restart restored a false access marker: state=%+v", store.Data)
	}
}

func TestAcceptedUnresolvedDispositionGatesOtherRepositoryAdmission(t *testing.T) {
	at := time.Now().UTC()
	path, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
	if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "failed", at); err != nil {
		t.Fatal(err)
	}
	other := config.Repository{Name: "other/repo", ID: 1, OwnerID: 4, Actions: []string{"review"}}
	m.Config.Repositories = append(m.Config.Repositories, other)
	notice := testNotification(at)
	notice.ID = "other-thread"
	notice.Repository.FullName = other.Name
	notice.Subject.URL = "https://api.github.com/repos/other/repo/pulls/8"
	gh.notifications = []github.Notification{notice}
	otherItem := selectedItem(at)
	otherItem.Key, otherItem.NodeID = "other-comment", "other-node"
	otherItem.HTMLURL = "https://github.com/other/repo/pull/8#issuecomment-9"
	gh.items = []github.Item{otherItem}
	gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
	handler := &fakeHandler{}
	m.Handler = handler
	backup := path + ".before-final-save"
	blocked := false
	gh.beforeVerify = func(context.Context, bool) error {
		if !blocked {
			blocked = true
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		return &github.StatusError{StatusCode: http.StatusNotFound}
	}
	if err := m.recoverAccepted(t.Context(), store); !errors.Is(err, ErrAcceptanceState) || !blocked ||
		store.Data.Accepted == nil || !store.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("failed disposition save released accepted action: err=%v state=%+v", err, store.Data)
	}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrAcceptanceState) ||
		len(gh.notificationCalls) != 0 || len(handler.jobs) != 0 {
		t.Fatalf("RunOnce discovered another repository before disposition save: err=%v calls=%v jobs=%v",
			err, gh.notificationCalls, handler.jobs)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted != nil ||
		store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess == (state.AccessDisposition{}) ||
		len(gh.notificationCalls) != 0 || len(handler.jobs) != 0 {
		t.Fatalf("terminal recovery polled before durable access disposition: err=%v state=%+v calls=%v jobs=%v",
			err, store.Data, gh.notificationCalls, handler.jobs)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, at.Add(-time.Minute))
	if store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess == (state.AccessDisposition{}) {
		t.Fatal("unresolved access disposition was not durable before admission")
	}
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if len(handler.jobs) != 1 || handler.jobs[0].Repository.Name != other.Name ||
		store.Data.Processed[accepted.JobID].IsZero() || store.Data.Accepted != nil {
		t.Fatalf("other repository did not proceed after disposition save: jobs=%v state=%+v", handler.jobs, store.Data)
	}
}
