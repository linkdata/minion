package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type heldSelectedAction struct {
	entered    chan Job
	release    chan struct{}
	executions atomic.Int32
}

func (*heldSelectedAction) WaitCapacity(context.Context) error { return nil }

func (h *heldSelectedAction) Execute(ctx context.Context, job Job) error {
	h.executions.Add(1)
	h.entered <- job
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRunOnceServicesControlsWhileSelectedActionExecutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		selected := selectedItem(now.Add(-2 * time.Second))
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/8#issuecomment-2"
		siblingID := jobID(sibling.Key, "review")
		firstNotice := testNotification(selected.UpdatedAt)
		secondNotice := testNotification(sibling.UpdatedAt)
		secondNotice.ID = "thread-2"
		secondNotice.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"

		statePath := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, statePath, now.Add(-time.Minute))
		store.Data.JobFailures[siblingID] = state.JobFailure{Attempts: maxJobAttempts}
		store.Data.NextInviteCheckAt = now.Add(time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}

		cfg := testConfig(t)
		cfg.MaxRuntimeMinutes = 7
		cfg.InvitationOwners = []string{"example-owner"}
		cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
		configPath := filepath.Join(t.TempDir(), "config.json")
		configBytes, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
			t.Fatal(err)
		}

		handler := &heldSelectedAction{entered: make(chan Job, 2), release: make(chan struct{})}
		gh := &fakeGitHub{
			notifications: []github.Notification{firstNotice, secondNotice},
			invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
				FullName: "example-owner/new-repo", ID: 41, Owner: github.User{Login: "example-owner", ID: 2},
			}, Permissions: "write"}},
			permission: github.Permission{Permission: "write"},
			threadItems: func(_ string, number int, _ time.Time) ([]github.Item, error) {
				switch number {
				case 7:
					return []github.Item{selected}, nil
				case 8:
					return []github.Item{sibling}, nil
				default:
					return nil, fmt.Errorf("unexpected thread #%d", number)
				}
			},
			beforeAccept: func() {
				if handler.executions.Load() == 0 {
					t.Error("invitation accepted before selected Execute entered")
				}
			},
		}
		m := &Monitor{Config: cfg, ConfigPath: configPath, GitHub: gh, Handler: handler,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		released := false
		defer func() {
			if !released {
				close(handler.release)
			}
		}()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()

		var job Job
		select {
		case job = <-handler.entered:
		case err := <-result:
			t.Fatalf("RunOnce returned before selected Execute: %v", err)
		}
		if job.Source.Key != selected.Key || store.Data.Accepted == nil ||
			store.Data.Accepted.JobID != job.ID || store.Data.Accepted.AttemptID != job.AttemptID {
			t.Fatalf("wrong action accepted: job=%+v accepted=%+v", job, store.Data.Accepted)
		}
		accepted := *store.Data.Accepted
		time.Sleep(time.Minute)
		synctest.Wait()
		var runErr error
		returned := false
		select {
		case err := <-result:
			runErr, returned = err, true
			t.Errorf("RunOnce returned while Execute was blocked: %v", err)
		default:
		}
		if !slices.Equal(gh.accepted, []int64{41}) {
			t.Errorf("due whitelisted invitation was not accepted: %v", gh.accepted)
		}
		_, siblingPending := store.Data.JobFailures[siblingID]
		if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+siblingID+" -->") ||
			store.Data.Processed[siblingID].IsZero() || siblingPending {
			t.Errorf("terminal sibling was not replied to once: comments=%v processed=%v pending=%t",
				gh.comments, store.Data.Processed[siblingID], siblingPending)
		}
		if store.Data.Accepted == nil || *store.Data.Accepted != accepted || handler.executions.Load() != 1 {
			t.Errorf("active action changed during control work: accepted=%+v want=%+v executions=%d",
				store.Data.Accepted, accepted, handler.executions.Load())
		}
		stateBytes, err := os.ReadFile(statePath)
		if err != nil {
			t.Errorf("read durable state while Execute is blocked: %v", err)
		} else {
			var saved state.Data
			if err := json.Unmarshal(stateBytes, &saved); err != nil {
				t.Errorf("decode durable state while Execute is blocked: %v", err)
			} else if saved.Accepted == nil || *saved.Accepted != accepted || saved.Processed[siblingID].IsZero() {
				t.Errorf("control work was not durable before Execute returned: accepted=%+v sibling=%v",
					saved.Accepted, saved.Processed[siblingID])
			}
		}
		savedConfig, err := config.Load(configPath)
		if err != nil {
			t.Errorf("load invitation config: %v", err)
		} else if _, ok := savedConfig.Repository("example-owner/new-repo"); !ok {
			t.Error("invited repository was not persisted before Execute returned")
		}

		close(handler.release)
		released = true
		synctest.Wait()
		if !returned {
			runErr = <-result
		}
		if runErr != nil {
			t.Errorf("RunOnce after Execute release: %v", runErr)
		}
		if store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() ||
			len(gh.comments) != 1 || handler.executions.Load() != 1 {
			t.Errorf("action or sibling repeated after release: accepted=%+v processed=%v replies=%d executions=%d",
				store.Data.Accepted, store.Data.Processed[job.ID], len(gh.comments), handler.executions.Load())
		}
	})
}
