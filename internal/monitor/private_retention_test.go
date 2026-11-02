package monitor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/state"
)

type retentionProbe struct {
	capacityHandler
	calledAt []time.Time
	onPrune  func()
	err      error
}

func (p *retentionProbe) PrunePrivateRecords(_ context.Context, _ *state.Store, at time.Time) error {
	if p.onPrune != nil {
		p.onPrune()
	}
	p.calledAt = append(p.calledAt, at)
	return p.err
}

func TestRunOncePrunesPrivateRecordsWithoutWorkerCapacity(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	workerUnavailable := errors.New("worker unavailable")
	probe := &retentionProbe{capacityHandler: capacityHandler{failAt: 1, err: workerUnavailable}, err: errors.New("retention unavailable")}
	var logs bytes.Buffer
	m := &Monitor{
		Config: testConfig(t), GitHub: &fakeGitHub{}, Handler: probe,
		Now: func() time.Time { return now }, Logger: log.New(&logs, "", 0), SelectedExecutorID: "local",
	}
	store := openState(t, now.Add(-time.Minute))
	store.Data.NextInviteCheckAt = now.Add(time.Minute)
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, workerUnavailable) {
		t.Fatalf("worker capacity error = %v", err)
	}
	if len(probe.calledAt) != 1 || !probe.calledAt[0].Equal(now) {
		t.Fatalf("retention calls = %v, want %s", probe.calledAt, now)
	}
	if !strings.Contains(logs.String(), "private record retention: retention unavailable") {
		t.Fatalf("retention failure was not logged: %s", logs.String())
	}
}

func TestRunOnceDefersPrivatePruneWhileAccepted(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	accepted := selectedAcceptance(t, now)
	accepted.RetryAt = now.Add(time.Minute)
	store.Data.Accepted = &accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	probe := &retentionProbe{}
	m := selectedMonitor(t, now, &fakeGitHub{}, probe)
	_, _ = m.RunOnce(t.Context(), store)
	if len(probe.calledAt) != 0 {
		t.Fatalf("private cleanup ran before accepted recovery: %v", probe.calledAt)
	}
}

func TestRunOnceReconcilesPendingWriteBeforePrivatePrune(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	store.Data.NextInviteCheckAt = now.Add(time.Minute)
	store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
		AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8,
		Repository: "owner/repo", Number: 7, Marker: "<!-- linkdataminion:issued-job -->",
		Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{}
	probe := &retentionProbe{onPrune: func() {
		if len(gh.verifiedCalls) == 0 {
			t.Error("private cleanup ran before publication reconciliation")
		}
	}}
	m := selectedMonitor(t, now, gh, probe)
	_, _ = m.RunOnce(t.Context(), store)
	if len(probe.calledAt) != 1 || store.Data.PublicationWrites["issued-job"].Status != state.PublicationWritePending {
		t.Fatalf("pending write starved unrelated retention: calls=%v write=%+v", probe.calledAt, store.Data.PublicationWrites["issued-job"])
	}
}

func TestRunOnceServicesDueControlsBeforePrivatePrune(t *testing.T) {
	for _, name := range []string{"invitation", "terminal reply"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			store := openState(t, now)
			store.Data.NextInviteCheckAt = now.Add(time.Minute)
			if name == "invitation" {
				store.Data.NextInviteCheckAt = now
			} else {
				queueTerminalControl(store, now, 8, state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
			}
			gh := &fakeGitHub{}
			if name == "terminal reply" {
				gh.repositoryErr = errors.New("terminal lookup unavailable")
			}
			invitationChecked := false
			probe := &retentionProbe{onPrune: func() {
				if name == "invitation" && !invitationChecked || name == "terminal reply" && gh.repoChecks == 0 {
					t.Errorf("private cleanup ran before due %s", name)
				}
			}}
			m := selectedMonitor(t, now, gh, probe)
			m.afterInvitationCheck = func() { invitationChecked = true }
			_, _ = m.RunOnce(t.Context(), store)
			wantCalls := 1
			if name == "terminal reply" {
				_, _ = m.RunOnce(t.Context(), store)
				wantCalls = 2 // A repeatedly due failed reply cannot starve retention.
			}
			if len(probe.calledAt) != wantCalls {
				t.Fatalf("due %s starved private cleanup: %v", name, probe.calledAt)
			}
		})
	}
}
