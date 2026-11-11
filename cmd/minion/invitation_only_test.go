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
}

func (*firstInvitationGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	return []github.Invitation{{ID: 7, Repository: github.Repository{ID: 50, FullName: "acme/repo", Owner: github.User{Login: "acme", ID: 42}}, Invitee: github.User{Login: "bot"}, Permissions: "write"}}, nil
}

func (g *firstInvitationGitHub) AcceptRepositoryInvitation(_ context.Context, id int64) error {
	g.accepted = id
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
}
