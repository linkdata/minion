package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

type firstInvitationGitHub struct {
	monitor.GitHub
	accepted int64
	declined int64
	onList   func()
}

type invitationCleanupProbe struct{ calls int }

func (*invitationCleanupProbe) Execute(context.Context, monitor.Job) error { return nil }

func (p *invitationCleanupProbe) PrunePrivateRecords(context.Context, *state.Store, time.Time) error {
	p.calls++
	return nil
}

func (g *firstInvitationGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	if g.onList != nil {
		onList := g.onList
		g.onList = nil
		onList()
	}
	return []github.Invitation{{ID: 7, Repository: github.Repository{ID: 50, FullName: "acme/repo", Owner: github.User{Login: "acme", ID: 42}}, Invitee: github.User{Login: "bot"}, Permissions: "write"}}, nil
}

func (g *firstInvitationGitHub) AcceptRepositoryInvitation(_ context.Context, id int64) error {
	g.accepted = id
	return nil
}

func (g *firstInvitationGitHub) DeclineRepositoryInvitation(_ context.Context, id int64) error {
	g.declined = id
	return nil
}

func TestInvitationOnlyAcceptsFirstRepositoryWithoutWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"bot_login":"bot","github":{"token":"test-token"},"invitation_owners":["acme"],"invitation_owner_ids":{"acme":42},"repositories":[]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gh := &firstInvitationGitHub{}
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: path, GitHub: gh}
	if code := runOrchestrateMode(t.Context(), nil, watcher, store, false, log.New(os.Stderr, "", 0), "", 0); code != 0 {
		t.Fatalf("invitation-only pass returned %d", code)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, ok := saved.Repository("acme/repo")
	if gh.accepted != 7 || !ok || !repo.DenyAllActors || len(watcher.Config.Repositories) != 0 || store.Data.RepositoryCursors["acme/repo"].IsZero() {
		t.Fatalf("first invitation: accepted=%d repo=%+v exists=%t active=%v cursors=%v", gh.accepted, repo, ok, watcher.Config.Repositories, store.Data.RepositoryCursors)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if code := runInvitationOnly(t.Context(), watcher, store, false, log.New(os.Stderr, "", 0), live); code != 0 || !live.snapshot().RestartRequired {
		t.Fatalf("saved first repository needs a restart: code=%d snapshot=%+v", code, live.snapshot())
	}
}

func TestInvitationOnlyAppliesOwnerRemovalBeforeInvitationAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"bot_login":"bot","github":{"token":"test-token"},"invitation_owners":["acme"],"invitation_owner_ids":{"acme":42},"repositories":[]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gh := &firstInvitationGitHub{}
	gh.onList = func() {
		if err := live.save(func(next *config.Config) error {
			next.InvitationOwners = []string{"other"}
			next.InvitationOwnerIDs = map[string]int64{"other": 99}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: path, GitHub: gh, BeforeAccept: live.admit}
	if code := runInvitationOnly(t.Context(), watcher, store, false, log.New(os.Stderr, "", 0), live); code != 0 {
		t.Fatalf("invitation-only pass returned %d", code)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if gh.accepted != 0 || gh.declined != 7 || len(saved.Repositories) != 0 || len(live.current().InvitationOwners) != 1 || live.current().InvitationOwners[0] != "other" {
		t.Fatalf("owner removal did not fence invitation: accepted=%d declined=%d repos=%v active=%v", gh.accepted, gh.declined, saved.Repositories, live.current().InvitationOwners)
	}
}

func TestInvitationOnlyRunsMonitorCleanup(t *testing.T) {
	now := time.Now().UTC()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	handler := &invitationCleanupProbe{}
	watcher := &monitor.Monitor{Handler: handler}
	if code := runInvitationOnly(t.Context(), watcher, store, false, log.New(os.Stderr, "", 0), nil); code != 0 || handler.calls != 1 {
		t.Fatalf("invitation-only monitor cleanup: code=%d calls=%d", code, handler.calls)
	}
}
