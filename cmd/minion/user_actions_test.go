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

func actionOAuthSignIn(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("guest JaWS page: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	guest := page.Result().Cookies()[0]
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, guest, "")
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || location.Query().Get("state") == "" {
		t.Fatalf("start OAuth: status=%d location=%q err=%v", start.Code, start.Header().Get("Location"), err)
	}
	callback := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+
		url.QueryEscape(location.Query().Get("state")), nil, guest, "")
	if callback.Code != http.StatusFound || len(callback.Result().Cookies()) != 1 {
		t.Fatalf("OAuth callback: status=%d body=%q", callback.Code, callback.Body.String())
	}
	return callback.Result().Cookies()[0]
}

func TestUserActionsRoutesBindRequesterAndCurrentPrivateAccess(t *testing.T) {
	var signedID atomic.Int64
	signedID.Store(3)
	oauthClient := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d}`, signedID.Load())), nil
		default:
			return oauthJSON(http.StatusNotFound, `{}`), nil
		}
	})}
	var accessMode atomic.Int32
	var pauseRepository atomic.Bool
	repositoryEntered := make(chan struct{}, 1)
	repositoryReleased := make(chan struct{})
	bot := github.New("bot-token")
	bot.HTTPClient = &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		mode := accessMode.Load()
		for i := 1; i <= 8; i++ {
			name := fmt.Sprintf("owner/budget-%d", i)
			path := "/repos/" + name
			if r.URL.Path == path || r.URL.Path == fmt.Sprintf("/repositories/%d", 100+i) {
				return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d,"full_name":%q,"private":true,"visibility":"private","owner":{"id":2}}`, 100+i, name)), nil
			}
			if r.URL.Path == path+"/collaborators/renamed/permission" {
				return oauthJSON(http.StatusOK, `{"permission":"read","user":{"id":3,"login":"renamed"}}`), nil
			}
		}
		switch r.URL.Path {
		case "/repositories/1":
			if pauseRepository.CompareAndSwap(true, false) {
				repositoryEntered <- struct{}{}
				<-repositoryReleased
			}
			if mode == 3 {
				return oauthJSON(http.StatusBadGateway, `{}`), nil
			}
			if mode == 9 {
				return oauthJSON(http.StatusNotFound, `{}`), nil
			}
			if mode == 10 {
				return oauthJSON(http.StatusMovedPermanently, `{}`), nil
			}
			id := 1
			if mode == 2 {
				id = 99
			}
			name := "owner/repo"
			if mode == 12 {
				name = "owner/renamed"
			}
			if mode == 7 {
				return oauthJSON(http.StatusOK, `{"id":1,"full_name":"owner/repo","private":false,"owner":{"id":2}}`), nil
			}
			if mode == 8 {
				return oauthJSON(http.StatusOK, `{"id":1,"full_name":"owner/repo","private":false,"visibility":"private","owner":{"id":2}}`), nil
			}
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d,"full_name":%q,"private":true,"visibility":"private","owner":{"id":2}}`, id, name)), nil
		case "/repos/owner/repo", "/repos/owner/renamed":
			id := 1
			if mode == 11 {
				id = 99 // The name rebounds after collaborator permission was read.
			}
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d,"full_name":%q,"private":true,"visibility":"private","owner":{"id":2}}`, id, strings.TrimPrefix(r.URL.Path, "/repos/"))), nil
		case "/repositories/9":
			return oauthJSON(http.StatusNotFound, `{}`), nil
		case "/user/3":
			id := 3
			if mode == 5 {
				id = 4
			}
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d,"login":"renamed"}`, id)), nil
		case "/repos/owner/repo/collaborators/renamed/permission", "/repos/owner/renamed/collaborators/renamed/permission":
			if mode == 6 {
				return oauthJSON(http.StatusNotFound, `{}`), nil
			}
			permission, id := "read", 3
			if mode == 1 {
				permission = "none"
			}
			if mode == 4 {
				id = 4
			}
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"permission":%q,"user":{"id":%d,"login":"renamed"}}`, permission, id)), nil
		default:
			t.Errorf("unexpected bot API path %q", r.URL.Path)
			return oauthJSON(http.StatusNotFound, `{}`), nil
		}
	})}
	at := time.Now().UTC().Truncate(time.Second)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := &action.Orchestrator{Store: store, GitHub: bot, WorkRoot: t.TempDir()}
	store.Data.Accepted = dashboardTestAccepted(at)
	store.Data.Accepted.MainSettings = config.RoleModelConfig{Model: "o3", Effort: "high"}
	if _, err := store.DecideTerminalLocked("job-1", "attempt-1", "succeeded", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	detailTestRecords(t, owner.WorkRoot, "PRIVATE_DISCUSSION", true)
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	jw.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	auth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), oauthClient)
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
	signed := actionOAuthSignIn(t, handler)
	gitlab := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "original"}).key()
	githubKey := (userIdentity{Provider: "github", Host: "github.com", Subject: "3"}).key()
	if err := auth.links.link(gitlab, githubKey); err != nil {
		t.Fatal(err)
	}
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(signed))); account != gitlab {
		t.Fatalf("linked account root = %q", account)
	}
	list := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "attempt-1") ||
		!strings.Contains(list.Body.String(), "owner/repo") || strings.Contains(list.Body.String(), "PRIVATE_DISCUSSION") {
		t.Fatalf("own JaWS list: status=%d body=%q", list.Code, list.Body.String())
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/auth/actions?cursor=bad", http.StatusBadRequest}, {"/auth/actions/missing", http.StatusNotFound}} {
		notice := adminResponse(handler, http.MethodGet, tc.path, nil, signed, "")
		if notice.Code != tc.want || strings.Contains(notice.Body.String(), "PRIVATE_DISCUSSION") {
			t.Fatalf("action error %s: status=%d body=%q", tc.path, notice.Code, notice.Body.String())
		}
		key := adminJaWSKey(t, notice.Body.String())
		if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+key, nil, signed, ""); tail.Code != http.StatusOK {
			t.Fatalf("action error %s JaWS tail: status=%d body=%q", tc.path, tail.Code, tail.Body.String())
		}
	}
	for _, raw := range []string{"cursor=%ZZ", "cursor=one&cursor=two", "cursor=" + strings.Repeat("x", 513)} {
		if invalid := adminResponse(handler, http.MethodGet, "/auth/actions?"+raw, nil, signed, ""); invalid.Code != http.StatusBadRequest {
			t.Fatalf("malformed history query %q status=%d", raw, invalid.Code)
		}
	}
	detail := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "PRIVATE_DISCUSSION") ||
		!strings.Contains(detail.Body.String(), "o3") {
		t.Fatalf("renamed-login private JaWS detail: status=%d body=%q", detail.Code, detail.Body.String())
	}
	t.Run("read slot released before private access check", func(t *testing.T) {
		var once sync.Once
		release := func() { once.Do(func() { close(repositoryReleased) }) }
		defer release()
		pauseRepository.Store(true)
		response := make(chan *httptest.ResponseRecorder, 1)
		go func() { response <- adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, "") }()
		select {
		case <-repositoryEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("private repository check did not start")
		}
		if got := len(auth.actionReads); got != 0 {
			t.Fatalf("slow GitHub check holds %d durable read slots", got)
		}
		access := &userActionAccess{oauth: auth, owner: owner}
		if !access.admitRead(context.Background()) {
			t.Fatal("short identity read refused during GitHub check")
		}
		_, found, err := owner.UserActionIdentity("attempt-1", 3)
		access.finishRead()
		if err != nil || !found {
			t.Fatalf("short identity read during GitHub check: found=%t err=%v", found, err)
		}
		release()
		select {
		case got := <-response:
			if got.Code != http.StatusOK {
				t.Fatalf("detail after slow GitHub check: status=%d body=%q", got.Code, got.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("detail did not complete after GitHub check")
		}
	})
	for range cap(auth.detailSlots) {
		auth.detailSlots <- struct{}{}
	}
	if busy := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, ""); busy.Code != http.StatusServiceUnavailable || strings.Contains(busy.Body.String(), "PRIVATE_DISCUSSION") {
		t.Fatalf("busy detail reader: status=%d body=%q", busy.Code, busy.Body.String())
	}
	for range cap(auth.detailSlots) {
		<-auth.detailSlots
	}
	summary, found, err := owner.UserActionIdentity("attempt-1", 3)
	if err != nil || !found {
		t.Fatalf("private callback identity: found=%t err=%v", found, err)
	}
	projected, found, err := owner.UserActionDetail("attempt-1", at, summary)
	if err != nil || !found {
		t.Fatalf("private callback detail: found=%t err=%v", found, err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/actions/attempt-1", nil)
	request.AddCookie(signed)
	rq := jw.NewRequest(httptest.NewRecorder(), request)
	defer rq.Cancel(nil)
	view := actionDetailView{DashboardDetail: projected, AllowUser: true, access: &userActionAccess{oauth: auth, owner: owner}}
	if err := view.JawsConnect(rq); err != nil {
		t.Fatalf("authorized JaWS connect: %v", err)
	}
	history, err := owner.UserHistory(3, "")
	if err != nil {
		t.Fatal(err)
	}
	listView := userActionsView{GitHubLinked: true, Entries: history.Entries, access: view.access}
	if err := listView.JawsConnect(rq); err != nil {
		t.Fatalf("authorized list JaWS connect: %v", err)
	}
	for range cap(auth.actionReads) {
		auth.actionReads <- struct{}{}
	}
	if busy := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, ""); busy.Code != http.StatusServiceUnavailable || strings.Contains(busy.Body.String(), "attempt-1") {
		t.Fatalf("busy action list: status=%d body=%q", busy.Code, busy.Body.String())
	}
	if busy := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, ""); busy.Code != http.StatusServiceUnavailable || strings.Contains(busy.Body.String(), "PRIVATE_DISCUSSION") {
		t.Fatalf("busy action detail: status=%d body=%q", busy.Code, busy.Body.String())
	}
	if got := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+adminJaWSKey(t, list.Body.String()), nil, signed, ""); got.Code != http.StatusOK {
		t.Fatalf("list JaWS tail during busy reads: status=%d body=%q", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+adminJaWSKey(t, detail.Body.String()), nil, signed, ""); got.Code != http.StatusForbidden {
		t.Fatalf("detail JaWS tail during busy reads: status=%d body=%q", got.Code, got.Body.String())
	}
	if err := listView.JawsConnect(rq); err != nil {
		t.Fatalf("list JaWS callback during busy reads: %v", err)
	}
	if err := view.JawsConnect(rq); !errors.Is(err, errUserActionBusy) {
		t.Fatalf("busy detail JaWS callback: %v", err)
	}
	for range cap(auth.actionReads) {
		<-auth.actionReads
	}
	if err := store.WithLock(func() error {
		missing := store.Data.TerminalDecisions["job-1"]
		missing.AttemptID = "attempt-2"
		missing.Summary.Repository = "owner/hidden"
		missing.Summary.RepositoryID = 9
		missing.Summary.AcceptedAt = at.Add(time.Second)
		store.Data.TerminalDecisions["job-2"] = missing
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	mixed := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, "")
	if mixed.Code != http.StatusOK || !strings.Contains(mixed.Body.String(), "attempt-1") ||
		strings.Contains(mixed.Body.String(), "attempt-2") || strings.Contains(mixed.Body.String(), "owner/hidden") {
		t.Fatalf("mixed private history page: status=%d body=%q", mixed.Code, mixed.Body.String())
	}
	mixedKey := adminJaWSKey(t, mixed.Body.String())
	if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+mixedKey, nil, signed, ""); tail.Code != http.StatusOK {
		t.Fatalf("mixed page JaWS tail: status=%d body=%q", tail.Code, tail.Body.String())
	}
	auth.usageGate.mu.Lock()
	auth.usageGate.apiTokens, auth.usageGate.apiRefillAt = 0, time.Now()
	auth.usageGate.mu.Unlock()
	if limited := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, ""); limited.Code != http.StatusServiceUnavailable || strings.Contains(limited.Body.String(), "owner/repo") {
		t.Fatalf("exhausted access budget exposed list: status=%d body=%q", limited.Code, limited.Body.String())
	}
	if limitedTail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+mixedKey, nil, signed, ""); limitedTail.Code != http.StatusForbidden {
		t.Fatalf("exhausted access budget reached JaWS tail: status=%d", limitedTail.Code)
	}
	auth.usageGate.mu.Lock()
	auth.usageGate.apiTokens = userUsageAPIBurst
	auth.usageGate.mu.Unlock()
	if err := store.WithLock(func() error {
		for i := 3; i <= 9; i++ {
			missing := store.Data.TerminalDecisions["job-2"]
			missing.AttemptID = fmt.Sprintf("attempt-%d", i)
			missing.Summary.AcceptedAt = at.Add(time.Duration(i-1) * time.Second)
			store.Data.TerminalDecisions[fmt.Sprintf("job-%d", i)] = missing
		}
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	empty := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), "No accessible actions in this batch. Older actions may exist.") ||
		strings.Contains(empty.Body.String(), "owner/hidden") || strings.Contains(empty.Body.String(), "attempt-1") {
		t.Fatalf("revoked-only batch: status=%d body=%q", empty.Code, empty.Body.String())
	}
	_, after, ok := strings.Cut(empty.Body.String(), "/auth/actions?cursor=")
	if !ok {
		t.Fatal("revoked-only batch omitted continuation")
	}
	cursor, _, ok := strings.Cut(after, `"`)
	if !ok || cursor == "" {
		t.Fatalf("invalid continuation in %q", empty.Body.String())
	}
	continued := adminResponse(handler, http.MethodGet, "/auth/actions?cursor="+cursor, nil, signed, "")
	if continued.Code != http.StatusOK || !strings.Contains(continued.Body.String(), "attempt-1") ||
		strings.Contains(continued.Body.String(), "owner/hidden") {
		t.Fatalf("continued accessible batch: status=%d body=%q", continued.Code, continued.Body.String())
	}
	if err := store.WithLock(func() error {
		for i := 3; i <= 9; i++ {
			delete(store.Data.TerminalDecisions, fmt.Sprintf("job-%d", i))
		}
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		for i := 10; i <= 13; i++ {
			visible := store.Data.TerminalDecisions["job-1"]
			visible.AttemptID = fmt.Sprintf("attempt-%d", i)
			visible.Summary.AcceptedAt = at.Add(time.Duration(i) * time.Second)
			store.Data.TerminalDecisions[fmt.Sprintf("job-%d", i)] = visible
		}
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	full := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, "")
	if full.Code != http.StatusOK || strings.Contains(full.Body.String(), `/auth/actions/attempt-1"`) ||
		!strings.Contains(full.Body.String(), "attempt-10") || !strings.Contains(full.Body.String(), "attempt-13") {
		t.Fatalf("bounded visible page: status=%d body=%q", full.Code, full.Body.String())
	}
	_, after, ok = strings.Cut(full.Body.String(), "/auth/actions?cursor=")
	if !ok {
		t.Fatal("full visible page omitted continuation")
	}
	cursor, _, ok = strings.Cut(after, `"`)
	if !ok || cursor == "" {
		t.Fatalf("invalid full-page continuation in %q", full.Body.String())
	}
	older := adminResponse(handler, http.MethodGet, "/auth/actions?cursor="+cursor, nil, signed, "")
	if older.Code != http.StatusOK || !strings.Contains(older.Body.String(), `/auth/actions/attempt-1"`) ||
		strings.Contains(older.Body.String(), "attempt-10") {
		t.Fatalf("older visible page: status=%d body=%q", older.Code, older.Body.String())
	}
	if err := store.WithLock(func() error {
		for i := 10; i <= 13; i++ {
			delete(store.Data.TerminalDecisions, fmt.Sprintf("job-%d", i))
		}
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	accessMode.Store(1)
	if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+mixedKey, nil, signed, ""); tail.Code != http.StatusForbidden {
		t.Fatalf("revoked exposed row reached mixed JaWS tail: status=%d body=%q", tail.Code, tail.Body.String())
	}
	if err := view.JawsConnect(rq); err == nil {
		t.Fatal("revoked repository access retained JaWS connection authorization")
	}
	if err := listView.JawsConnect(rq); err == nil {
		t.Fatal("revoked repository access retained list JaWS connection authorization")
	}
	for _, body := range []string{detail.Body.String(), list.Body.String()} {
		key := adminJaWSKey(t, body)
		if got := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+key, nil, signed, ""); got.Code != http.StatusForbidden {
			t.Fatalf("revoked repository reached JaWS tail: status=%d body=%q", got.Code, got.Body.String())
		}
	}
	accessMode.Store(0)
	for _, tc := range []struct {
		name string
		mode int32
		want int
	}{
		{"permission revoked", 1, http.StatusForbidden},
		{"repository identity changed", 2, http.StatusServiceUnavailable},
		{"provider failure", 3, http.StatusServiceUnavailable},
		{"permission user mismatch", 4, http.StatusForbidden},
		{"resolved user mismatch", 5, http.StatusServiceUnavailable},
		{"permission missing", 6, http.StatusForbidden},
		{"visibility missing", 7, http.StatusForbidden},
		{"visibility contradictory", 8, http.StatusForbidden},
		{"repository missing", 9, http.StatusForbidden},
		{"repository moved", 10, http.StatusForbidden},
		{"repository name rebound", 11, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accessMode.Store(tc.mode)
			got := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, "")
			if got.Code != tc.want || strings.Contains(got.Body.String(), "PRIVATE_DISCUSSION") {
				t.Fatalf("detail after access change: status=%d body=%q", got.Code, got.Body.String())
			}
			page := adminResponse(handler, http.MethodGet, "/auth/actions?cursor=", nil, signed, "")
			wantPage := http.StatusOK
			if tc.want == http.StatusServiceUnavailable {
				wantPage = http.StatusServiceUnavailable
			}
			if page.Code != wantPage || strings.Contains(page.Body.String(), "attempt-1") ||
				strings.Contains(page.Body.String(), "owner/hidden") {
				t.Fatalf("continued page after access change: status=%d body=%q", page.Code, page.Body.String())
			}
		})
	}
	accessMode.Store(0)
	accessMode.Store(12)
	if renamed := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, signed, ""); renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), "PRIVATE_DISCUSSION") {
		t.Fatalf("renamed repository detail: status=%d body=%q", renamed.Code, renamed.Body.String())
	}
	accessMode.Store(0)
	signedID.Store(4)
	other := actionOAuthSignIn(t, handler)
	if got := adminResponse(handler, http.MethodGet, "/auth/actions/attempt-1", nil, other, ""); got.Code != http.StatusNotFound || !strings.Contains(got.Body.String(), `name="jawsKey"`) ||
		strings.Contains(got.Body.String(), "PRIVATE_DISCUSSION") {
		t.Fatalf("other requester detail JaWS error: status=%d body=%q", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodGet, "/auth/actions", nil, other, ""); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), "attempt-1") || strings.Contains(got.Body.String(), "PRIVATE_DISCUSSION") {
		t.Fatalf("other requester list status=%d body=%q", got.Code, got.Body.String())
	}
	t.Run("private repository pages and tails exhaust the account budget", func(t *testing.T) {
		if err := store.WithLock(func() error {
			sample := store.Data.TerminalDecisions["job-1"]
			store.Data.TerminalDecisions = make(map[string]state.TerminalDecision)
			for i := 1; i <= 8; i++ {
				decision := sample
				decision.AttemptID = fmt.Sprintf("budget-attempt-%d", i)
				decision.Summary.Repository = fmt.Sprintf("owner/budget-%d", i)
				decision.Summary.RepositoryID = int64(100 + i)
				decision.Summary.AcceptedAt = at.Add(time.Duration(9-i) * time.Second)
				store.Data.TerminalDecisions[fmt.Sprintf("budget-job-%d", i)] = decision
			}
			return store.Save()
		}); err != nil {
			t.Fatal(err)
		}
		auth.usageGate.mu.Lock()
		auth.usageGate.apiTokens = userUsageAPIBurst
		auth.usageGate.apiRefillAt = time.Now()
		auth.usageGate.accounts = make(map[string]userUsageAccountBudget)
		auth.usageGate.mu.Unlock()
		first := adminResponse(handler, http.MethodGet, "/auth/actions", nil, signed, "")
		if first.Code != http.StatusOK {
			t.Fatalf("first private page: status=%d body=%q", first.Code, first.Body.String())
		}
		firstKey := adminJaWSKey(t, first.Body.String())
		_, after, ok := strings.Cut(first.Body.String(), "/auth/actions?cursor=")
		if !ok {
			t.Fatal("first private page omitted continuation")
		}
		cursor, _, ok := strings.Cut(after, `"`)
		if !ok || cursor == "" {
			t.Fatalf("invalid private continuation %q", after)
		}
		second := adminResponse(handler, http.MethodGet, "/auth/actions?cursor="+cursor, nil, signed, "")
		if second.Code != http.StatusOK {
			t.Fatalf("second private page: status=%d body=%q", second.Code, second.Body.String())
		}
		for i := 1; i <= 8; i++ {
			body := second.Body.String()
			if i <= 4 {
				body = first.Body.String()
			}
			if !strings.Contains(body, fmt.Sprintf("owner/budget-%d", i)) {
				t.Fatalf("private repository %d absent from its page", i)
			}
		}
		paths := []string{"/auth/actions", "/auth/actions?cursor=" + cursor}
		var served [2]int
		exhausted := false
		for cycle := 0; cycle < 8 && !exhausted; cycle++ {
			for i, path := range paths {
				page := adminResponse(handler, http.MethodGet, path, nil, signed, "")
				if page.Code == http.StatusServiceUnavailable {
					if strings.Contains(page.Body.String(), "owner/budget-") {
						t.Fatalf("budget error exposed private history: %q", page.Body.String())
					}
					exhausted = true
					break
				}
				if page.Code != http.StatusOK {
					t.Fatalf("private page %s: status=%d body=%q", path, page.Code, page.Body.String())
				}
				served[i]++
				tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+adminJaWSKey(t, page.Body.String()), nil, signed, "")
				if tail.Code == http.StatusForbidden {
					exhausted = true
					break
				}
				if tail.Code != http.StatusOK {
					t.Fatalf("private page %s tail: status=%d body=%q", path, tail.Code, tail.Body.String())
				}
			}
		}
		auth.usageGate.mu.Lock()
		remainingGlobal := auth.usageGate.apiTokens
		remainingAccount := auth.usageGate.accounts["3"].tokens
		auth.usageGate.mu.Unlock()
		if !exhausted || served[0] < 2 || served[1] < 2 || remainingGlobal <= 0 ||
			remainingAccount >= userUsageMinPrivateCalls {
			t.Fatalf("budget exhaustion: exhausted=%t pages=%v global=%d account=%d",
				exhausted, served, remainingGlobal, remainingAccount)
		}
		if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+firstKey, nil, signed, ""); tail.Code != http.StatusForbidden ||
			strings.Contains(tail.Body.String(), "owner/budget-") {
			t.Fatalf("exhausted account reached private JaWS tail: status=%d body=%q", tail.Code, tail.Body.String())
		}
	})
	auth.links.mu.Lock()
	auth.links.bad = errors.New("link store unavailable")
	auth.links.mu.Unlock()
	for _, path := range []string{"/auth/actions", "/auth/actions/attempt-1"} {
		got := adminResponse(handler, http.MethodGet, path, nil, signed, "")
		if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), `name="jawsKey"`) ||
			strings.Contains(got.Body.String(), "PRIVATE_DISCUSSION") {
			t.Fatalf("uncertain link store %s: status=%d body=%q", path, got.Code, got.Body.String())
		}
	}
}

func TestGitLabOnlyActionsPageDoesNotOfferUnavailableGitHubLink(t *testing.T) {
	issuer, nonce := testGitLabIssuer(t)
	cfg := config.AdminConfig{GitLabOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/gitlab/callback", BaseURL: issuer.URL}}
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), issuer.Client())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, &action.Orchestrator{}, auth)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("GitLab sign-in page: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	guest := page.Result().Cookies()[0]
	start := adminResponse(handler, http.MethodGet, "/auth/gitlab/login", nil, guest, "")
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || location.Query().Get("state") == "" || location.Query().Get("nonce") == "" {
		t.Fatalf("GitLab sign-in start: status=%d location=%q err=%v", start.Code, start.Header().Get("Location"), err)
	}
	nonce.Store(location.Query().Get("nonce"))
	callback := adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=gitlab&state="+
		url.QueryEscape(location.Query().Get("state")), nil, guest, "")
	if callback.Code != http.StatusFound || len(callback.Result().Cookies()) != 1 {
		t.Fatalf("GitLab sign-in callback: status=%d body=%q", callback.Code, callback.Body.String())
	}
	view := adminResponse(handler, http.MethodGet, "/auth/actions", nil, callback.Result().Cookies()[0], "")
	if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), "GitHub action history is unavailable on this server.") ||
		strings.Contains(view.Body.String(), "Link a GitHub account") {
		t.Fatalf("GitLab-only action page: status=%d body=%q", view.Code, view.Body.String())
	}
}

func TestUserActionAccessSharesRequesterProofAndCapsBotCalls(t *testing.T) {
	var calls, userLookups atomic.Int32
	bot := github.New("bot-token")
	bot.HTTPClient = &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch r.URL.Path {
		case "/user/3":
			userLookups.Add(1)
			return oauthJSON(http.StatusOK, `{"id":3,"login":"renamed"}`), nil
		case "/repos/owner/one", "/repos/owner/two", "/repos/owner/three", "/repositories/1", "/repositories/2", "/repositories/3":
			name := map[string]string{
				"/repos/owner/one": "one", "/repositories/1": "one",
				"/repos/owner/two": "two", "/repositories/2": "two",
				"/repos/owner/three": "three", "/repositories/3": "three",
			}[r.URL.Path]
			id := map[string]int{"one": 1, "two": 2, "three": 3}[name]
			name = "owner/" + name
			return oauthJSON(http.StatusOK, fmt.Sprintf(`{"id":%d,"full_name":%q,"private":true,"visibility":"private","owner":{"id":2}}`, id, name)), nil
		case "/repos/owner/one/collaborators/renamed/permission", "/repos/owner/two/collaborators/renamed/permission":
			return oauthJSON(http.StatusOK, `{"permission":"read","user":{"id":3,"login":"renamed"}}`), nil
		default:
			t.Errorf("unexpected bot API path %q", r.URL.Path)
			return oauthJSON(http.StatusNotFound, `{}`), nil
		}
	})}
	gate := newUserUsageGate()
	gate.apiTokens, gate.accountBurst = 7, 7
	access := &userActionAccess{oauth: &oauthAccess{usageGate: gate}, owner: &action.Orchestrator{GitHub: bot}}
	summary := dashboardTestAccepted(time.Now().UTC()).HistorySummary()
	cache := make(map[repositoryProofKey]error)
	proof := &requesterProof{}
	for i, name := range []string{"one", "two", "three"} {
		summary.Repository, summary.RepositoryID = "owner/"+name, int64(i+1)
		err := access.checkRepositoryOnce(context.Background(), summary, cache, proof)
		if i < 2 && err != nil || i == 2 && !errors.Is(err, errUserActionBusy) {
			t.Fatalf("repository %s: %v", name, err)
		}
	}
	if got := calls.Load(); got != 7 || userLookups.Load() != 1 {
		t.Fatalf("bot calls=%d user lookups=%d; want 7 and 1", got, userLookups.Load())
	}
}

func TestSharedUserAPIGateBoundsGlobalAccountAndConcurrentChecks(t *testing.T) {
	gate := newUserUsageGate()
	gate.apiTokens, gate.accountBurst = 3, 2
	ctx := context.Background()
	if !gate.take(ctx, 3) || !gate.takeGitHubCall(ctx, "3") || gate.take(ctx, 3) ||
		!gate.takeGitHubCall(ctx, "4") || gate.take(ctx, 4) {
		t.Fatal("global or requester call budget was not enforced")
	}
	for id := int64(5); id < 1005; id++ {
		if gate.take(ctx, id) {
			t.Fatal("exhausted global budget admitted another account")
		}
	}
	if len(gate.accounts) != 2 {
		t.Fatalf("exhausted calls grew account map to %d", len(gate.accounts))
	}
	for range cap(gate.checks) {
		if !gate.enter(ctx) {
			t.Fatal("check slot unavailable below cap")
		}
	}
	if gate.enter(ctx) {
		t.Fatal("check cap allowed another concurrent probe")
	}
	for range cap(gate.checks) {
		gate.leave()
	}
}
