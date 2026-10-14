package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func TestLiveReleaseRefusesUnprotectedTags(t *testing.T) {
	if os.Getenv("MINION_LIVE_WRITE") != "1" {
		t.Skip("set MINION_LIVE_WRITE=1 to test a live release refusal")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := liveActionSetup(t, ctx)
	installActionTestSkills(t, testReleaseSkillURL)
	if fixture.cfg.IssueNumber <= 0 {
		t.Fatal("live release test requires an issue_number")
	}
	issue, err := fixture.owner.IssueDetails(ctx, liveFixRepository, fixture.cfg.IssueNumber)
	if err != nil || !strings.EqualFold(issue.User.Login, fixture.cfg.OwnerLogin) || issue.NodeID == "" {
		t.Fatalf("issue #%d is not an owner-authored fixture: %v", fixture.cfg.IssueNumber, err)
	}
	protected, err := fixture.bot.ProtectedReleaseTags(ctx, liveFixRepository)
	if err != nil {
		t.Fatal(err)
	}
	if protected {
		t.Skip("dedicated live repository has release tag protection; refusal path does not apply")
	}
	fixture.bot.HTTPClient.Transport = liveReleaseGuard{base: http.DefaultTransport}

	var randomID [32]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(randomID[:])
	tag := "minion-live-release-" + id[:12]
	if _, err := fixture.owner.TagReference(ctx, liveFixRepository, tag); !liveFixNotFound(err) {
		t.Fatalf("test tag %s already exists or cannot be checked: %v", tag, err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	job := monitor.Job{
		Repository:  config.Repository{Name: liveFixRepository, ID: fixture.repo.ID, OwnerID: fixture.repo.Owner.ID, Actions: []string{"release"}},
		SubjectType: "Issue",
		Number:      fixture.cfg.IssueNumber,
		ID:          id,
		Source: github.Item{
			Body:    "@linkdataminion release " + tag,
			Author:  fixture.ownerUser,
			HTMLURL: issue.HTMLURL,
		},
		Command: command.Command{Name: "release", Action: command.Release, Args: tag},
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		liveActionReplyCleanup(t, cleanupCtx, fixture.owner, fixture.ownerToken, fixture.cfg, job)
	})
	t.Setenv("GH_TOKEN", fixture.botToken)
	codexCalls := 0
	executor := &Executor{
		GitHub: fixture.bot, BotLogin: fixture.cfg.BotLogin, Store: store, WorkRoot: t.TempDir(),
		Commands: map[string]config.CommandMapping{"release": {Action: "release", Skills: []string{testReleaseSkillURL}}},
		RunCodex: func(context.Context, string, string, bool, string) (codexcli.Result, error) {
			codexCalls++
			return codexcli.Result{}, nil
		},
	}
	if err := executor.Execute(ctx, job); err != nil {
		t.Fatal(err)
	}
	commentID, reply, err := liveActionReply(ctx, fixture.owner, fixture.cfg, job)
	if err != nil || commentID == 0 || !strings.Contains(reply.Body, "Release drafts are disabled") {
		t.Fatalf("release refusal acknowledgement = %+v: %v", reply, err)
	}
	if _, err := fixture.owner.TagReference(ctx, liveFixRepository, tag); !liveFixNotFound(err) {
		t.Fatalf("release refusal created tag %s: %v", tag, err)
	}
	if _, err := fixture.owner.ReleaseByTag(ctx, liveFixRepository, tag); !liveFixNotFound(err) {
		t.Fatalf("release refusal created draft for tag %s: %v", tag, err)
	}
	if codexCalls != 0 || len(store.Data.PendingReleases) != 0 {
		t.Fatalf("release refusal ran Codex or recorded pending release: calls=%d, pending=%d", codexCalls, len(store.Data.PendingReleases))
	}
	if err := executor.Execute(ctx, job); err != nil {
		t.Fatal(err)
	}
	if repeatID, _, err := liveActionReply(ctx, fixture.owner, fixture.cfg, job); err != nil || repeatID != commentID || codexCalls != 0 {
		t.Fatalf("repeat release refusal changed the acknowledgement: id=%d, want=%d, Codex calls=%d: %v", repeatID, commentID, codexCalls, err)
	}
}

type liveReleaseGuard struct {
	base http.RoundTripper
}

func (guard liveReleaseGuard) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && (request.URL.Path == "/repos/"+liveFixRepository+"/git/refs" || request.URL.Path == "/repos/"+liveFixRepository+"/releases") {
		return nil, errors.New("live release test blocked tag or draft creation")
	}
	return guard.base.RoundTrip(request)
}
