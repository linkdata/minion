package monitor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func uncertainAcceptedReplyFixture(t *testing.T, at time.Time, withDisplayURL bool) (string, *state.Store, state.AcceptedAction, state.TerminalDecision, *fakeGitHub, *Monitor) {
	t.Helper()
	path, store, accepted, gh, m := unresolvedAcceptedFixture(t, at)
	if !withDisplayURL {
		accepted.SourceURL = ""
		store.Data.Accepted.SourceURL = ""
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "canceled", at)
	if err != nil {
		t.Fatal(err)
	}
	gh.commentErr = errors.New("POST response lost")
	gh.currentItem = func(item github.Item) (github.Item, error) {
		if item.Key == accepted.SourceKey {
			return selectedItem(at), nil
		}
		return item, nil
	}
	m.Now = time.Now
	if err := m.recoverAccepted(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	write := store.Data.PublicationWrites[accepted.JobID]
	if gh.commentCalls != 1 || write.Status != state.PublicationWritePending ||
		write.SourceURL != accepted.SourceURL || write.SourceKey != accepted.SourceKey || write.Repository != accepted.Repository ||
		write.AttemptID != accepted.AttemptID || store.Data.Accepted != nil ||
		store.Data.Processed[accepted.JobID].IsZero() ||
		store.Data.TerminalDecisions[accepted.JobID] != first {
		t.Fatalf("uncertain POST lost original provenance or released an uncheckpointed reply: write=%+v state=%+v calls=%d",
			write, store.Data, gh.commentCalls)
	}
	return path, store, accepted, first, gh, m
}

func TestUnresolvedIssuedReplyRepeated404AfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		at := time.Now().UTC()
		path, store, accepted, first, gh, m := uncertainAcceptedReplyFixture(t, at, true)
		var logs bytes.Buffer
		m.Logger = log.New(&logs, "", 0)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, at.Add(-time.Minute))
		write := store.Data.PublicationWrites[accepted.JobID]
		if write.SourceURL != accepted.SourceURL || write.SourceKey != accepted.SourceKey || store.Data.Accepted != nil ||
			store.Data.TerminalDecisions[accepted.JobID] != first {
			t.Fatalf("restart lost issued reply provenance: state=%+v", store.Data)
		}
		gh.beforeVerify = func(context.Context, bool) error {
			return &github.StatusError{StatusCode: http.StatusNotFound}
		}
		for i := 0; i < 2; i++ {
			if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWritePending ||
				store.Data.TerminalDecisions[accepted.JobID] != first || gh.commentCalls != 1 {
				t.Fatalf("pre-grace 404 changed decision or retried POST: state=%+v calls=%d", store.Data, gh.commentCalls)
			}
			time.Sleep(publicationRecheckInterval)
		}
		time.Sleep(time.Until(write.Deadline) + time.Nanosecond)
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		decision := store.Data.TerminalDecisions[accepted.JobID]
		access := decision.UnresolvedAccess
		resolvedWrite := store.Data.PublicationWrites[accepted.JobID]
		if decision.AttemptID != first.AttemptID || decision.Outcome != first.Outcome ||
			!decision.DecidedAt.Equal(first.DecidedAt) || access.Repository != accepted.Repository ||
			access.SourceURL != accepted.SourceURL || access.SourceKey != accepted.SourceKey || access.ReplyState != "unknown" ||
			access.Reason == "" || !strings.Contains(access.Reason, "HTTP 404") || access.OperatorAction == "" ||
			resolvedWrite.Status != state.PublicationWriteUncertain || resolvedWrite.ResolvedAt.IsZero() ||
			gh.commentCalls != 1 {
			t.Fatalf("grace expiry lost unresolved reply disposition: decision=%+v write=%+v calls=%d",
				decision, resolvedWrite, gh.commentCalls)
		}
		if !strings.Contains(logs.String(), accepted.SourceURL) || !strings.Contains(logs.String(), accepted.AttemptID) {
			t.Fatalf("unresolved reply log omits original request: %q", logs.String())
		}
		lookups := len(gh.verifiedCalls)
		if lookups < 3 {
			t.Fatalf("grace did not recheck repeated 404: lookups=%d", lookups)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, at.Add(-time.Minute))
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil ||
			store.Data.TerminalDecisions[accepted.JobID] != decision ||
			store.Data.PublicationWrites[accepted.JobID] != resolvedWrite ||
			len(gh.verifiedCalls) != lookups || gh.commentCalls != 1 {
			t.Fatalf("restart lost final disposition or repeated the POST: err=%v state=%+v lookups=%d calls=%d",
				err, store.Data, len(gh.verifiedCalls), gh.commentCalls)
		}
	})
}

func TestUnresolvedIssuedReplyWithoutDisplayURL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		at := time.Now().UTC()
		path, store, accepted, _, gh, m := uncertainAcceptedReplyFixture(t, at, false)
		write := store.Data.PublicationWrites[accepted.JobID]
		if write.SourceURL != "" || write.SourceKey != accepted.SourceKey {
			t.Fatalf("issued reply lost source key: %+v", write)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, at.Add(-time.Minute))
		gh.beforeVerify = func(context.Context, bool) error {
			return &github.StatusError{StatusCode: http.StatusNotFound}
		}
		time.Sleep(time.Until(write.Deadline) + time.Nanosecond)
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		access := store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess
		if access.SourceURL != "" || access.SourceKey != accepted.SourceKey ||
			access.ReplyState != "unknown" || store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteUncertain || gh.commentCalls != 1 {
			t.Fatalf("404 after issued reply lost source key or repeated POST: access=%+v state=%+v calls=%d",
				access, store.Data, gh.commentCalls)
		}
	})
}

func TestUnresolvedIssuedReplyTemporary404ThenReadable(t *testing.T) {
	for _, markerPresent := range []bool{false, true} {
		name := "no marker"
		if markerPresent {
			name = "verified marker"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				at := time.Now().UTC()
				_, store, accepted, first, gh, m := uncertainAcceptedReplyFixture(t, at, true)
				write := store.Data.PublicationWrites[accepted.JobID]
				gh.beforeVerify = func(context.Context, bool) error {
					return &github.StatusError{StatusCode: http.StatusNotFound}
				}
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil ||
					store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWritePending ||
					store.Data.TerminalDecisions[accepted.JobID] != first {
					t.Fatalf("temporary 404 finalized access before grace: err=%v state=%+v", err, store.Data)
				}
				gh.beforeVerify = nil
				if markerPresent {
					gh.items = append(gh.items, github.Item{
						Key: "reply:1", Body: accepted.FixedRefusal + "\n\n" + write.Marker,
						Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: time.Now().UTC(),
					})
				}
				time.Sleep(publicationRecheckInterval)
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				if markerPresent {
					if store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteObserved {
						t.Fatalf("verified marker was not observed: %+v", store.Data)
					}
				} else {
					if store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWritePending {
						t.Fatalf("readable no-marker lookup ended grace early: %+v", store.Data)
					}
					time.Sleep(time.Until(write.Deadline) + time.Nanosecond)
					if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
						t.Fatal(err)
					}
					if store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteUncertain {
						t.Fatalf("no-marker lookup did not close issued write: %+v", store.Data)
					}
				}
				if store.Data.TerminalDecisions[accepted.JobID] != first || gh.commentCalls != 1 {
					t.Fatalf("recovered access created false disposition or reposted: state=%+v calls=%d", store.Data, gh.commentCalls)
				}
			})
		})
	}
}

func TestUnresolvedIssuedReplyDefiniteMoveOrGoneHasNoDisposition(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				at := time.Now().UTC()
				_, store, accepted, first, gh, m := uncertainAcceptedReplyFixture(t, at, true)
				write := store.Data.PublicationWrites[accepted.JobID]
				gh.beforeVerify = func(context.Context, bool) error {
					return &github.StatusError{StatusCode: status}
				}
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Until(write.Deadline) + time.Nanosecond)
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				if store.Data.TerminalDecisions[accepted.JobID] != first ||
					store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteUncertain ||
					gh.commentCalls != 1 {
					t.Fatalf("definite move or deletion became unresolved access: state=%+v calls=%d", store.Data, gh.commentCalls)
				}
			})
		})
	}
}

func TestUnresolvedIssuedReplyFailedGraceSaveThenMarkerIsObserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		at := time.Now().UTC()
		path, store, accepted, first, gh, m := uncertainAcceptedReplyFixture(t, at, true)
		write := store.Data.PublicationWrites[accepted.JobID]
		time.Sleep(time.Until(write.Deadline) + time.Nanosecond)
		backup := path + ".before-grace-save"
		gh.beforeVerify = func(context.Context, bool) error {
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			return &github.StatusError{StatusCode: http.StatusNotFound}
		}
		if err := m.ReconcilePublicationWrites(t.Context(), store); !errors.Is(err, ErrAcceptanceState) ||
			store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWritePending ||
			store.Data.TerminalDecisions[accepted.JobID].UnresolvedAccess != (state.AccessDisposition{}) || gh.commentCalls != 1 {
			t.Fatalf("failed grace save lost pending write or left an unresolved marker: err=%v state=%+v calls=%d",
				err, store.Data, gh.commentCalls)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
		gh.beforeVerify = nil
		gh.items = append(gh.items, github.Item{
			Key: "reply:1", Body: accepted.FixedRefusal + "\n\n" + write.Marker,
			Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: time.Now().UTC(),
		})
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		observed := store.Data.PublicationWrites[accepted.JobID]
		if observed.Status != state.PublicationWriteObserved || observed.ResolvedAt.IsZero() ||
			store.Data.TerminalDecisions[accepted.JobID] != first || gh.commentCalls != 1 {
			t.Fatalf("readable marker left stale unresolved access or retried POST: write=%+v decision=%+v calls=%d",
				observed, store.Data.TerminalDecisions[accepted.JobID], gh.commentCalls)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, at.Add(-time.Minute))
		if store.Data.PublicationWrites[accepted.JobID] != observed ||
			store.Data.TerminalDecisions[accepted.JobID] != first || gh.commentCalls != 1 {
			t.Fatalf("restart restored stale unresolved access: state=%+v calls=%d", store.Data, gh.commentCalls)
		}
	})
}
