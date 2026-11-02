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
	err      error
}

func (p *retentionProbe) PrunePrivateRecords(_ context.Context, _ *state.Store, at time.Time) error {
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
	if _, err := m.RunOnce(t.Context(), openState(t, now.Add(-time.Minute))); !errors.Is(err, workerUnavailable) {
		t.Fatalf("worker capacity error = %v", err)
	}
	if len(probe.calledAt) != 1 || !probe.calledAt[0].Equal(now) {
		t.Fatalf("retention calls = %v, want %s", probe.calledAt, now)
	}
	if !strings.Contains(logs.String(), "private record retention: retention unavailable") {
		t.Fatalf("retention failure was not logged: %s", logs.String())
	}
}

func TestRunOnceDefersPrivatePruneDuringRecovery(t *testing.T) {
	for _, name := range []string{"accepted action", "pending publication"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			store := openState(t, now)
			if name == "accepted action" {
				accepted := selectedAcceptance(t, now)
				accepted.RetryAt = now.Add(time.Minute)
				store.Data.Accepted = &accepted
			} else {
				store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
					AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8,
					Repository: "owner/repo", Number: 7, Marker: "<!-- linkdataminion:issued-job -->",
					Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
				}
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			probe := &retentionProbe{}
			m := selectedMonitor(t, now, &fakeGitHub{}, probe)
			_, _ = m.RunOnce(t.Context(), store)
			if len(probe.calledAt) != 0 {
				t.Fatalf("private cleanup ran during %s recovery", name)
			}
		})
	}
}
