package main

import (
	"context"
	"errors"
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
	accepted   int64
	declined   int64
	onList     func()
	listErr    error
	empty      bool
	emptyFirst bool
	listCalls  int
}

type invitationCleanupProbe struct{ calls int }

func (*invitationCleanupProbe) Execute(context.Context, monitor.Job) error { return nil }

func (p *invitationCleanupProbe) PrunePrivateRecords(context.Context, *state.Store, time.Time) error {
	p.calls++
	return nil
}

func (g *firstInvitationGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	g.listCalls++
	if g.onList != nil {
		onList := g.onList
		g.onList = nil
		onList()
	}
	if g.listErr != nil {
		return nil, g.listErr
	}
	if g.empty || g.emptyFirst && g.listCalls == 1 {
		return nil, nil
	}
	return []github.Invitation{{ID: 7, Repository: github.Repository{ID: 50, FullName: "acme/repo", Owner: github.User{Login: "acme", ID: 42}}, Invitee: github.User{Login: "bot"}, Permissions: "write"}}, nil
}

func TestInvitationOnlyOneshotReportsInvitationFailure(t *testing.T) {
	now := time.Now().UTC()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	watcher := &monitor.Monitor{GitHub: &firstInvitationGitHub{listErr: errors.New("invitation list failed")}}
	if code := runInvitationOnly(t.Context(), watcher, store, false, log.New(os.Stderr, "", 0), nil); code != 1 {
		t.Fatalf("invitation-only failure exited %d, want 1", code)
	}
}

func (g *firstInvitationGitHub) AcceptRepositoryInvitation(_ context.Context, id int64) error {
	g.accepted = id
	return nil
}

func (g *firstInvitationGitHub) DeclineRepositoryInvitation(_ context.Context, id int64) error {
	g.declined = id
	return nil
}

func TestInvitationOnlyOneshotIgnoresUserPreferenceFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(statePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := os.WriteFile(statePath+".link", []byte("invalid JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	gh := &firstInvitationGitHub{empty: true}
	watcher := &monitor.Monitor{Config: config.Config{
		StatePath: statePath,
		Admin:     &config.AdminConfig{UserModelChoices: &config.UserModelChoices{}},
	}, GitHub: gh}
	if code := runOrchestrateMode(t.Context(), nil, watcher, store, false, log.New(os.Stderr, "", 0), "", 0); code != 0 || gh.listCalls != 1 {
		t.Fatalf("owner-only pass used unrelated preferences: code=%d invitation_checks=%d", code, gh.listCalls)
	}
}

func TestInvitationOnlyAcceptsFirstRepositoryWithoutWorker(t *testing.T) {
	cfg, path := writeModeConfig(t, config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"},
		InvitationOwners: []string{"acme"}, InvitationOwnerIDs: map[string]int64{"acme": 42},
	})
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
	cfg, path := writeModeConfig(t, config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"},
		InvitationOwners: []string{"acme"}, InvitationOwnerIDs: map[string]int64{"acme": 42},
	})
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

func TestInvitationOnlyFencesFirstRepositorySavedDuringListing(t *testing.T) {
	cfg, path := writeModeConfig(t, config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"},
		InvitationOwners: []string{"acme"}, InvitationOwnerIDs: map[string]int64{"acme": 42},
	})
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gh := &firstInvitationGitHub{onList: func() {
		if err := live.save(func(next *config.Config) error {
			next.Repositories = []config.Repository{{Name: "other/first", ID: 90, OwnerID: 91, Actions: []string{"review"}}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}}
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: path, GitHub: gh, BeforeAccept: live.admit}
	if code := runInvitationOnly(t.Context(), watcher, store, false, log.New(os.Stderr, "", 0), live); code != 0 {
		t.Fatalf("invitation-only pass returned %d", code)
	}
	if gh.accepted != 0 || gh.declined != 0 || !live.snapshot().RestartRequired {
		t.Fatalf("first-repository save did not fence invitation: accepted=%d declined=%d restart=%t", gh.accepted, gh.declined, live.snapshot().RestartRequired)
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

func TestInvitationOnlyLoopAppliesOwnerUpdateWithoutWaitingForNextPoll(t *testing.T) {
	cfg, path := writeModeConfig(t, config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"},
		InvitationOwners: []string{"acme"}, InvitationOwnerIDs: map[string]int64{"acme": 42},
	})
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	saved := make(chan struct{})
	gh := &firstInvitationGitHub{empty: true, onList: func() {
		if err := live.save(func(next *config.Config) error {
			next.InvitationOwners = []string{"other"}
			next.InvitationOwnerIDs = map[string]int64{"other": 99}
			return nil
		}); err != nil {
			t.Error(err)
		}
		close(saved)
	}}
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: path, GitHub: gh, BeforeAccept: live.admit}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- runInvitationOnly(ctx, watcher, store, true, log.New(os.Stderr, "", 0), live) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	firstPass := time.NewTimer(5 * time.Second)
	defer firstPass.Stop()
	select {
	case <-saved:
	case <-firstPass.C:
		t.Fatal("first invitation pass did not run")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for live.snapshot().Pending {
		select {
		case <-deadline.C:
			t.Fatal("owner update waited for the next invitation poll")
		case <-ticker.C:
		}
	}
	cancel()
	code := <-done
	joined = true
	if code != 0 || live.current().InvitationOwners[0] != "other" {
		t.Fatalf("owner update was not applied: code=%d owners=%v", code, live.current().InvitationOwners)
	}
}

func TestInvitationOnlyStopsPollingWhenAdminAddsFirstRepository(t *testing.T) {
	cfg, path := writeModeConfig(t, config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"},
		InvitationOwners: []string{"acme"}, InvitationOwnerIDs: map[string]int64{"acme": 42},
	})
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	listed := make(chan struct{})
	gh := &firstInvitationGitHub{emptyFirst: true, onList: func() { close(listed) }}
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: path, GitHub: gh, BeforeAccept: live.admit}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- runInvitationOnly(ctx, watcher, store, true, log.New(os.Stderr, "", 0), live) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	select {
	case <-listed:
	case <-deadline.C:
		t.Fatal("initial invitation check did not run")
	}
	for {
		var checked bool
		_ = store.WithLock(func() error {
			checked = store.Data.NextInviteCheckAt.After(time.Now())
			return nil
		})
		if checked {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("initial invitation check did not finish")
		case <-ticker.C:
		}
	}
	if err := store.WithLock(func() error {
		store.Data.NextInviteCheckAt = time.Time{}
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(next *config.Config) error {
		next.Repositories = []config.Repository{{Name: "other/first", ID: 90, OwnerID: 91, Actions: []string{"review"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	probe := time.NewTimer(500 * time.Millisecond)
	defer probe.Stop()
	<-probe.C
	cancel()
	code := <-done
	joined = true
	if code != 0 || gh.listCalls != 1 || gh.accepted != 0 || !live.snapshot().RestartRequired {
		t.Fatalf("invitation checks continued before restart: code=%d lists=%d accepted=%d", code, gh.listCalls, gh.accepted)
	}
}
