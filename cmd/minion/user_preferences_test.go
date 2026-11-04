package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

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
		!strings.Contains(page.Body.String(), `<form method="post" action="/auth/preferences">`) ||
		!strings.Contains(page.Body.String(), "approved-main") {
		t.Fatalf("preference JaWS page: status=%d body=%q", page.Code, page.Body.String())
	}
	account := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	form := url.Values{"main": {preferenceChoiceValue(choice)}, "identity": {preferenceFormIdentity(account)}}
	if saved := adminResponse(handler, http.MethodPost, "/auth/preferences", form, signed, "https://admin.example"); saved.Code != http.StatusSeeOther {
		t.Fatalf("save: status=%d body=%q", saved.Code, saved.Body.String())
	}
	if got, err := auth.links.preferencesForGitHubID(42); err != nil || got.Main != choice {
		t.Fatalf("saved preference: %+v, %v", got, err)
	}
	form.Set("identity", "stale account")
	if stale := adminResponse(handler, http.MethodPost, "/auth/preferences", form, signed, "https://admin.example"); stale.Code != http.StatusConflict {
		t.Fatalf("stale form: status=%d", stale.Code)
	}
	auth.choices.Main = nil
	withdrawn := adminResponse(handler, http.MethodGet, "/auth/preferences", nil, signed, "")
	if withdrawn.Code != http.StatusOK || !strings.Contains(withdrawn.Body.String(), "Saved main choice is unavailable") ||
		!strings.Contains(withdrawn.Body.String(), `<select name="main">`) ||
		!strings.Contains(withdrawn.Body.String(), `<option value="default" selected>Service default</option>`) {
		t.Fatalf("withdrawn choice page: status=%d body=%q", withdrawn.Code, withdrawn.Body.String())
	}
}
