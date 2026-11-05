package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestOAuthPreferencesUseJaWSAndBoundSavedChoice(t *testing.T) {
	choice := config.RoleModelConfig{Model: "approved-main", Effort: "low"}
	cfg := config.AdminConfig{
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
		UserModelChoices: &config.UserModelChoices{Main: []config.RoleModelConfig{choice}},
	}
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":42}`), nil
		default:
			t.Errorf("unexpected provider request: %s", r.URL)
			return oauthJSON(http.StatusNotFound, `{}`), nil
		}
	})}
	handler, _, auth := testOAuthAdminHandler(t, cfg, client)
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, nil, "")
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || len(start.Result().Cookies()) != 1 {
		t.Fatalf("start: status=%d error=%v", start.Code, err)
	}
	guest := start.Result().Cookies()[0]
	callback := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil, guest, "")
	if callback.Code != http.StatusFound || len(callback.Result().Cookies()) != 1 {
		t.Fatalf("callback: status=%d cookies=%v", callback.Code, callback.Result().Cookies())
	}
	signed := callback.Result().Cookies()[0]
	page := adminResponse(handler, http.MethodGet, "/auth/preferences", nil, signed, "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="jawsKey"`) ||
		!strings.Contains(page.Body.String(), `<form class="d-grid gap-3" method="post" action="/auth/preferences">`) ||
		!strings.Contains(page.Body.String(), `name="viewport"`) ||
		!strings.Contains(page.Body.String(), "approved-main") {
		t.Fatalf("preference JaWS page: status=%d body=%q", page.Code, page.Body.String())
	}
	account := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	form := url.Values{"main": {preferenceChoiceValue(choice)}, "identity": {preferenceFormIdentity(account)}}
	if saved := adminResponse(handler, http.MethodPost, "/auth/preferences", form, signed, "https://admin.example"); saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/auth/preferences" {
		t.Fatalf("save: status=%d location=%q body=%q", saved.Code, saved.Header().Get("Location"), saved.Body.String())
	}
	if got, err := auth.links.preferencesForGitHubID(42); err != nil || got.Main != choice {
		t.Fatalf("saved preference: %+v, %v", got, err)
	}
	form.Set("main", "default")
	limited := adminResponse(handler, http.MethodPost, "/auth/preferences", form, signed, "https://admin.example")
	retry := limited.Header().Get("Retry-After")
	if limited.Code != http.StatusTooManyRequests || retry == "" ||
		!strings.Contains(limited.Body.String(), `name="jawsKey"`) ||
		!strings.Contains(limited.Body.String(), "Wait "+retry+" seconds before retrying") {
		t.Fatalf("rate limit notice: status=%d retry=%q body=%q", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
	}
	forged := adminResponse(handler, http.MethodGet, "/auth/preferences?saved=1", nil, signed, "")
	if forged.Code != http.StatusOK || strings.Contains(forged.Body.String(), "Preferences saved for future actions") {
		t.Fatalf("query flag falsely claims a save: status=%d body=%q", forged.Code, forged.Body.String())
	}
	form.Set("identity", "stale account")
	if stale := adminResponse(handler, http.MethodPost, "/auth/preferences", form, signed, "https://admin.example"); stale.Code != http.StatusConflict {
		t.Fatalf("stale form: status=%d", stale.Code)
	}
	auth.choices.Main = nil
	withdrawn := adminResponse(handler, http.MethodGet, "/auth/preferences", nil, signed, "")
	if withdrawn.Code != http.StatusOK || !strings.Contains(withdrawn.Body.String(), "Saved main choice is unavailable") ||
		!strings.Contains(withdrawn.Body.String(), `<select class="form-select" name="main">`) ||
		!strings.Contains(withdrawn.Body.String(), `<option value="default" selected>Service default</option>`) {
		t.Fatalf("withdrawn choice page: status=%d body=%q", withdrawn.Code, withdrawn.Body.String())
	}
}

func TestOAuthPreferencesSaveReadsDelayedHTTPBody(t *testing.T) {
	choice := config.RoleModelConfig{Model: "approved-main", Effort: "low"}
	cfg := config.AdminConfig{
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
		UserModelChoices: &config.UserModelChoices{Main: []config.RoleModelConfig{choice}},
	}
	provider := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":42}`), nil
		default:
			t.Errorf("unexpected provider request: %s", r.URL)
			return oauthJSON(http.StatusNotFound, `{}`), nil
		}
	})}
	handler, _, auth := testOAuthAdminHandler(t, cfg, provider)
	server := httptest.NewServer(handler)
	defer server.Close()
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := browser.Get(server.URL + "/auth/github/login")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	location, err := url.Parse(start.Header.Get("Location"))
	if err != nil || start.StatusCode != http.StatusFound || len(start.Cookies()) != 1 {
		t.Fatalf("start: status=%d error=%v", start.StatusCode, err)
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(start.Cookies()[0])
	callback, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	callback.Body.Close()
	if callback.StatusCode != http.StatusFound || len(callback.Cookies()) != 1 {
		t.Fatalf("callback: status=%d cookies=%v", callback.StatusCode, callback.Cookies())
	}
	signed := callback.Cookies()[0]
	account := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	body := url.Values{"main": {preferenceChoiceValue(choice)}, "identity": {preferenceFormIdentity(account)}}.Encode()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /auth/preferences HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nCookie: %s=%s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\n\r\n",
		server.Listener.Addr(), server.URL, signed.Name, signed.Value, len(body)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // Send the form after the handler receives its headers.
	if _, err := io.WriteString(conn, body); err != nil {
		t.Fatal(err)
	}
	saved, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Body.Close()
	if saved.StatusCode != http.StatusSeeOther || saved.Header.Get("Location") != "/auth/preferences" {
		t.Fatalf("delayed-body save: status=%d location=%q", saved.StatusCode, saved.Header.Get("Location"))
	}
	if got, err := auth.links.preferencesForGitHubID(42); err != nil || got.Main != choice {
		t.Fatalf("delayed-body preference: %+v, %v", got, err)
	}
}
