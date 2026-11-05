package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestUserAbortLinkedGitLabOwnActiveAttempt(t *testing.T) {
	issuer, nonce := testGitLabIssuer(t)
	var githubID atomic.Int64
	githubID.Store(4)
	providerClient := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d}`, githubID.Load())), nil
		default:
			if r.URL.Host == strings.TrimPrefix(issuer.URL, "https://") {
				return issuer.Client().Transport.RoundTrip(r)
			}
			return nil, fmt.Errorf("unexpected provider request %s", r.URL)
		}
	})}
	var accessMode atomic.Int32
	var pauseRepository atomic.Bool
	repositoryEntered := make(chan struct{}, 1)
	repositoryReleased := make(chan struct{})
	bot := github.New("bot-token")
	bot.HTTPClient = &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/repositories/1", "/repos/owner/repo":
			if pauseRepository.CompareAndSwap(true, false) {
				repositoryEntered <- struct{}{}
				<-repositoryReleased
			}
			switch accessMode.Load() {
			case 1:
				return oauthJSON(http.StatusOK, `{"id":99,"full_name":"owner/repo","private":true,"visibility":"private","owner":{"id":2}}`), nil
			case 2:
				return oauthJSON(http.StatusBadGateway, `{}`), nil
			}
			return oauthJSON(http.StatusOK, `{"id":1,"full_name":"owner/repo","private":true,"visibility":"private","owner":{"id":2}}`), nil
		case "/user/3":
			return oauthJSON(http.StatusOK, `{"id":3,"login":"renamed"}`), nil
		case "/repos/owner/repo/collaborators/renamed/permission":
			if accessMode.Load() == 3 {
				return oauthJSON(http.StatusOK, `{"permission":"none","user":{"id":3,"login":"renamed"}}`), nil
			}
			return oauthJSON(http.StatusOK, `{"permission":"read","user":{"id":3,"login":"renamed"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected bot request %s", r.URL)
		}
	})}
	at := time.Now().UTC().Truncate(time.Second)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accepted := dashboardTestAccepted(at)
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	owner := &action.Orchestrator{Store: store, GitHub: bot, WorkRoot: t.TempDir()}
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	jw.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret", RedirectURL: "https://admin.example/auth/github/callback"},
		GitLabOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret", RedirectURL: "https://admin.example/auth/gitlab/callback", BaseURL: issuer.URL}}
	auth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), providerClient)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, owner, auth)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	guest := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if guest.Code != http.StatusOK || len(guest.Result().Cookies()) != 1 {
		t.Fatalf("guest sign-in status=%d cookies=%v", guest.Code, guest.Result().Cookies())
	}
	start := adminResponse(handler, http.MethodGet, "/auth/gitlab/login", nil, guest.Result().Cookies()[0], "")
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || location.Query().Get("state") == "" || location.Query().Get("nonce") == "" {
		t.Fatalf("GitLab sign-in start: status=%d location=%q err=%v", start.Code, start.Header().Get("Location"), err)
	}
	nonce.Store(location.Query().Get("nonce"))
	login := adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil, guest.Result().Cookies()[0], "")
	if login.Code != http.StatusFound || len(login.Result().Cookies()) != 1 {
		t.Fatalf("GitLab sign-in: status=%d body=%q", login.Code, login.Body.String())
	}
	signed := login.Result().Cookies()[0]
	path := "/auth/actions/attempt-1/abort"
	if got := adminResponse(handler, http.MethodPost, path, nil, signed, "https://admin.example"); got.Code != http.StatusForbidden {
		t.Fatalf("unlinked GitLab abort status=%d", got.Code)
	}
	gitlabKey := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "team:42/member"}).key()
	githubKey := (userIdentity{Provider: "github", Host: "github.com", Subject: "3"}).key()
	if err := auth.links.link(gitlabKey, githubKey); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	if got := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), `action="/auth/actions/attempt-1/abort"`) {
		t.Fatalf("own JaWS detail omitted abort form: status=%d body=%q", got.Code, got.Body.String())
	}
	admin := dashboardTestCookie(t, handler)
	other := actionOAuthSignIn(t, handler)
	for _, tc := range []struct {
		name, target, origin string
		cookie               *http.Cookie
		mode                 int32
		want                 int
	}{
		{"anonymous", path, "https://admin.example", nil, 0, http.StatusSeeOther},
		{"administrator", path, "https://admin.example", admin, 0, http.StatusSeeOther},
		{"other requester", path, "https://admin.example", other, 0, http.StatusNotFound},
		{"wrong attempt", "/auth/actions/other-attempt/abort", "https://admin.example", signed, 0, http.StatusNotFound},
		{"foreign origin", path, "https://other.example", signed, 0, http.StatusForbidden},
		{"wrong scheme", path, "http://admin.example", signed, 0, http.StatusForbidden},
		{"changed repository", path, "https://admin.example", signed, 1, http.StatusServiceUnavailable},
		{"repository unavailable", path, "https://admin.example", signed, 2, http.StatusServiceUnavailable},
		{"permission revoked", path, "https://admin.example", signed, 3, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accessMode.Store(tc.mode)
			got := adminResponse(handler, http.MethodPost, tc.target, nil, tc.cookie, tc.origin)
			if got.Code != tc.want {
				t.Fatalf("abort status=%d want=%d body=%q", got.Code, tc.want, got.Body.String())
			}
			if tc.want == http.StatusSeeOther && (got.Header().Get("Location") != "/auth/login?error=request" || got.Body.Len() != 0) {
				t.Fatalf("denied abort did not hand off to JaWS login: status=%d location=%q body=%q", got.Code, got.Header().Get("Location"), got.Body.String())
			}
			if tc.name == "other requester" && (!strings.Contains(got.Body.String(), `name="jawsKey"`) ||
				strings.Contains(got.Body.String(), "PRIVATE_DISCUSSION")) {
				t.Fatalf("denied abort did not render a safe JaWS notice: status=%d body=%q", got.Code, got.Body.String())
			}
			if tc.name == "other requester" {
				key := adminJaWSKey(t, got.Body.String())
				if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+key, nil, tc.cookie, ""); tail.Code != http.StatusOK {
					t.Fatalf("denied abort JaWS connection: status=%d", tail.Code)
				}
			}
			if ctx.Err() != nil {
				t.Fatal("denied abort canceled selected action")
			}
		})
	}
	accessMode.Store(0)
	auth.links.mu.Lock()
	auth.links.bad = errors.New("link store unavailable")
	auth.links.mu.Unlock()
	uncertain := adminResponse(handler, http.MethodPost, path, nil, signed, "https://admin.example")
	auth.links.mu.Lock()
	auth.links.bad = nil
	auth.links.mu.Unlock()
	if uncertain.Code != http.StatusServiceUnavailable || !strings.Contains(uncertain.Body.String(), `name="jawsKey"`) || ctx.Err() != nil {
		t.Fatalf("uncertain account links ran abort: status=%d cancellation=%v body=%q", uncertain.Code, ctx.Err(), uncertain.Body.String())
	}
	for range cap(auth.actionReads) {
		auth.actionReads <- struct{}{}
	}
	busyAbort := adminResponse(handler, http.MethodPost, path, nil, signed, "https://admin.example")
	for range cap(auth.actionReads) {
		<-auth.actionReads
	}
	if busyAbort.Code != http.StatusServiceUnavailable || ctx.Err() != nil {
		t.Fatalf("busy identity reads: status=%d cancellation=%v", busyAbort.Code, ctx.Err())
	}
	t.Run("abort while detail checks and projections are busy", func(t *testing.T) {
		for range cap(auth.detailSlots) {
			auth.detailSlots <- struct{}{}
		}
		defer func() {
			for range cap(auth.detailSlots) {
				<-auth.detailSlots
			}
		}()
		auth.actionReads <- struct{}{}
		defer func() { <-auth.actionReads }()
		var once sync.Once
		release := func() { once.Do(func() { close(repositoryReleased) }) }
		defer release()
		pauseRepository.Store(true)
		detailResponse := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			detailResponse <- adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, "")
		}()
		select {
		case <-repositoryEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("detail repository check did not start")
		}
		got := adminResponse(handler, http.MethodPost, path, nil, signed, "")
		if got.Code != http.StatusSeeOther || got.Header().Get("Location") != "/auth/actions/attempt-1" {
			t.Fatalf("own abort during slow detail check: status=%d location=%q body=%q", got.Code, got.Header().Get("Location"), got.Body.String())
		}
		release()
		select {
		case <-detailResponse:
		case <-time.After(5 * time.Second):
			t.Fatal("blocked detail did not finish")
		}
	})
	if ctx.Err() != context.Canceled {
		t.Fatal("own abort did not cancel selected action")
	}
	saved, err := store.ReadSavedDurable()
	if err != nil || saved.Accepted == nil || !saved.Accepted.Terminal ||
		saved.Accepted.StopRequest.RequestedBy != "github-user:3" || saved.Accepted.StopRequest.Status != "requested" {
		t.Fatalf("abort did not save stable linked account attribution: accepted=%+v err=%v", saved.Accepted, err)
	}
	if got := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "Termination requested; waiting for stop verification.") ||
		strings.Contains(got.Body.String(), "Abort this action") {
		t.Fatalf("requested status/form: status=%d body=%q", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodPost, path, nil, signed, "https://admin.example"); got.Code != http.StatusSeeOther {
		t.Fatalf("repeat abort status=%d body=%q", got.Code, got.Body.String())
	}
	if err := store.ResolveStop(accepted.JobID, accepted.AttemptID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "canceled", time.Now().UTC()); err != nil {
			return err
		}
		store.Data.Accepted = nil
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	unregister()
	if got := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "Termination confirmed.") || strings.Contains(got.Body.String(), "Abort this action") {
		t.Fatalf("confirmed status/form: status=%d body=%q", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodPost, path, nil, signed, "https://admin.example"); got.Code != http.StatusSeeOther {
		t.Fatalf("completed repeat status=%d body=%q", got.Code, got.Body.String())
	}
	if saved, err := store.ReadSavedDurable(); err != nil || saved.TerminalDecisions[accepted.JobID].Outcome != "canceled" ||
		saved.TerminalDecisions[accepted.JobID].StopRequest.RequestedBy != "github-user:3" ||
		saved.TerminalDecisions[accepted.JobID].StopRequest.Status != "confirmed" {
		t.Fatalf("completed abort changed terminal decision: decision=%+v err=%v", saved.TerminalDecisions[accepted.JobID], err)
	}
	finished := dashboardTestAccepted(at)
	finished.JobID, finished.AttemptID = "job-2", "attempt-2"
	if err := store.WithLock(func() error {
		store.Data.Accepted = finished
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	finishedCtx, finishedCancel := context.WithCancel(t.Context())
	defer finishedCancel()
	finishedUnregister := store.RegisterSelectedStop(finished.JobID, finished.AttemptID, finishedCancel)
	defer finishedUnregister()
	if got := adminResponse(handler, http.MethodPost, path, nil, signed, "https://admin.example"); got.Code != http.StatusSeeOther || finishedCtx.Err() != nil {
		t.Fatalf("old attempt stopped a newer action: status=%d cancellation=%v", got.Code, finishedCtx.Err())
	}
	if err := store.WithLock(func() error {
		if _, err := store.DecideTerminalLocked(finished.JobID, finished.AttemptID, "succeeded", time.Now().UTC()); err != nil {
			return err
		}
		store.Data.Accepted = nil
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	if got := adminResponse(handler, http.MethodPost, "/auth/actions/attempt-2/abort", nil, signed, "https://admin.example"); got.Code != http.StatusConflict {
		t.Fatalf("completion-winning abort status=%d body=%q", got.Code, got.Body.String())
	}
	if finishedCtx.Err() != nil {
		t.Fatal("completion-winning abort canceled the finished action")
	}
	finishedUnregister()
	if saved, err := store.ReadSavedDurable(); err != nil || saved.TerminalDecisions[finished.JobID].Outcome != "succeeded" ||
		saved.TerminalDecisions[finished.JobID].StopRequest != (state.StopRequest{}) {
		t.Fatalf("completion-winning abort altered outcome: decision=%+v err=%v", saved.TerminalDecisions[finished.JobID], err)
	}
	unclean := dashboardTestAccepted(at)
	unclean.JobID, unclean.AttemptID = "job-3", "attempt-3"
	if err := store.WithLock(func() error {
		store.Data.Accepted = unclean
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	uncleanCtx, uncleanCancel := context.WithCancel(t.Context())
	defer uncleanCancel()
	uncleanUnregister := store.RegisterSelectedStop(unclean.JobID, unclean.AttemptID, uncleanCancel)
	defer uncleanUnregister()
	if got := adminResponse(handler, http.MethodPost, "/auth/actions/attempt-3/abort", nil, signed, "https://admin.example"); got.Code != http.StatusSeeOther || uncleanCtx.Err() != context.Canceled {
		t.Fatalf("unclean abort: status=%d cancellation=%v", got.Code, uncleanCtx.Err())
	}
	if err := store.ResolveStop(unclean.JobID, unclean.AttemptID, "unverified"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		if _, err := store.DecideTerminalLocked(unclean.JobID, unclean.AttemptID, "canceled", time.Now().UTC()); err != nil {
			return err
		}
		store.Data.Accepted = nil
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	uncleanUnregister()
	if got := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-3", nil, signed, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "Termination could not be verified; contact an operator to check executor readiness before reuse.") ||
		strings.Contains(got.Body.String(), "Abort this action") {
		t.Fatalf("unverified status/form: status=%d body=%q", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-3", nil, admin, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "Termination could not be verified; check executor readiness before reuse.") ||
		strings.Contains(got.Body.String(), "contact an operator") {
		t.Fatalf("admin older attempt unverified status: status=%d body=%q", got.Code, got.Body.String())
	}
}
