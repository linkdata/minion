package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

const (
	userSessionKey       = "minion.user.identity"
	userLabelKey         = "minion.user.label"
	oauthStateCookie     = "__Host-minion_oauth_state"
	loopbackStateCookie  = "minion_oauth_state"
	oauthStateLifetime   = 10 * time.Minute
	maxPendingOAuth      = 128
	maxPendingOAuthPerIP = 8
	maxOAuthExchanges    = 4
	maxOAuthResponse     = 32 << 10
	maxOAuthAccessToken  = 4096
)

type oauthProvider struct {
	name, host, authorizeURL, tokenURL, profileURL, scope string
	clientID, clientSecret, redirectURL                   string
}

type pendingOAuth struct {
	provider, verifier, linkIdentity string
	clientIP                         string
	linkSession                      *jaws.Session
	expires                          time.Time
}

type oauthAccess struct {
	jaws      *jaws.Jaws
	links     *userLinks
	client    *http.Client
	providers map[string]oauthProvider
	mu        sync.Mutex
	pending   map[string]pendingOAuth
	inflight  int
	byIP      map[string]int
	exchanges chan struct{}
}

func newOAuthAccess(jw *jaws.Jaws, cfg config.AdminConfig, statePath string, client *http.Client) (*oauthAccess, error) {
	if cfg.GitHubOAuth == nil && cfg.GitLabOAuth == nil {
		return nil, nil
	}
	if statePath == "" {
		return nil, errors.New("OAuth sign-in requires a state path")
	}
	links, err := openUserLinks(statePath + ".user-links.json")
	if err != nil {
		return nil, fmt.Errorf("open user links: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a := &oauthAccess{jaws: jw, links: links, client: &bounded,
		providers: make(map[string]oauthProvider, 2), pending: make(map[string]pendingOAuth), byIP: make(map[string]int),
		exchanges: make(chan struct{}, maxOAuthExchanges)}
	if c := cfg.GitHubOAuth; c != nil {
		a.providers["github"] = oauthProvider{name: "github", host: "github.com",
			authorizeURL: "https://github.com/login/oauth/authorize", tokenURL: "https://github.com/login/oauth/access_token",
			profileURL: "https://api.github.com/user", scope: "read:user",
			clientID: c.ClientID, clientSecret: c.ClientSecret, redirectURL: c.RedirectURL}
	}
	if c := cfg.GitLabOAuth; c != nil {
		base := c.BaseURL
		if base == "" {
			base = "https://gitlab.com"
		}
		host := strings.TrimPrefix(base, "https://")
		a.providers["gitlab"] = oauthProvider{name: "gitlab", host: host,
			authorizeURL: base + "/oauth/authorize", tokenURL: base + "/oauth/token",
			profileURL: base + "/api/v4/user", scope: "read_user",
			clientID: c.ClientID, clientSecret: c.ClientSecret, redirectURL: c.RedirectURL}
	}
	return a, nil
}

func (a *oauthAccess) allowed(r *http.Request) bool {
	if a == nil {
		return false
	}
	switch r.URL.Path {
	case "/auth/login", "/auth/callback":
		return r.Method == http.MethodGet || r.URL.Path == "/auth/login" && r.Method == http.MethodHead
	case "/auth/github", "/auth/gitlab":
		_, ok := a.providers[strings.TrimPrefix(r.URL.Path, "/auth/")]
		return ok && r.Method == http.MethodGet
	case "/auth/link/github", "/auth/link/gitlab":
		_, ok := a.providers[strings.TrimPrefix(r.URL.Path, "/auth/link/")]
		return ok && r.Method == http.MethodPost
	case "/auth/logout":
		return r.Method == http.MethodPost
	}
	return false
}

func (a *oauthAccess) routes(mux *http.ServeMux) {
	if a == nil {
		return
	}
	mux.HandleFunc("GET /auth/login", a.loginPage)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("GET /auth/github", a.start)
	mux.HandleFunc("GET /auth/gitlab", a.start)
	mux.HandleFunc("POST /auth/link/github", a.start)
	mux.HandleFunc("POST /auth/link/gitlab", a.start)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

func (a *oauthAccess) loginPage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Minion sign-in</title></head><body><main>`)
	sess := a.jaws.GetSession(r)
	key, _ := sess.Get(userSessionKey).(string)
	if a.links.account(key) != "" {
		label, _ := sess.Get(userLabelKey).(string)
		_, _ = fmt.Fprintf(w, "<h1>Signed in as %s</h1>", html.EscapeString(label))
		for _, name := range []string{"github", "gitlab"} {
			if _, ok := a.providers[name]; ok {
				_, _ = fmt.Fprintf(w, `<form method="post" action="/auth/link/%s"><button>Link %s account</button></form>`, name, name)
			}
		}
		_, _ = io.WriteString(w, `<form method="post" action="/auth/logout"><button>Sign out</button></form>`)
	} else {
		_, _ = io.WriteString(w, "<h1>Sign in</h1>")
		for _, name := range []string{"github", "gitlab"} {
			if _, ok := a.providers[name]; ok {
				_, _ = fmt.Fprintf(w, `<p><a href="/auth/%s">Sign in with %s</a></p>`, name, name)
			}
		}
	}
	_, _ = io.WriteString(w, "</main></body></html>")
}

func randomOAuthValue() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func stateCookieName(r *http.Request) string {
	if r.TLS != nil {
		return oauthStateCookie
	}
	return loopbackStateCookie
}

func stateCookieValue(r *http.Request) (string, bool) {
	var value string
	seen := false
	for _, cookie := range r.Cookies() {
		if cookie.Name == stateCookieName(r) {
			if seen {
				return "", false
			}
			seen = true
			value = cookie.Value
		}
	}
	return value, true
}

func (a *oauthAccess) start(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	link := strings.HasPrefix(r.URL.Path, "/auth/link/")
	name := strings.TrimPrefix(r.URL.Path, "/auth/")
	name = strings.TrimPrefix(name, "link/")
	provider, ok := a.providers[name]
	if !ok || !provider.matchesRequest(r) {
		http.Error(w, "Sign-in origin is unavailable", http.StatusBadRequest)
		return
	}
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	pending := pendingOAuth{provider: name, clientIP: clientIP, expires: time.Now().Add(oauthStateLifetime)}
	if link {
		pending.linkSession = a.jaws.GetSession(r)
		pending.linkIdentity, _ = pending.linkSession.Get(userSessionKey).(string)
		if a.links.account(pending.linkIdentity) == "" {
			http.Error(w, "Sign in before linking accounts", http.StatusForbidden)
			return
		}
	}
	state, err := randomOAuthValue()
	if err == nil {
		pending.verifier, err = randomOAuthValue()
	}
	if err != nil {
		http.Error(w, "Sign-in unavailable", http.StatusServiceUnavailable)
		return
	}
	previous, unique := stateCookieValue(r)
	if !unique {
		http.Error(w, "Invalid sign-in state", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	perIP := 0
	for key, older := range a.pending {
		if !time.Now().Before(older.expires) || key == previous && older.clientIP == clientIP {
			delete(a.pending, key)
		} else if older.clientIP == clientIP {
			perIP++
		}
	}
	if len(a.pending)+a.inflight >= maxPendingOAuth || perIP+a.byIP[clientIP] >= maxPendingOAuthPerIP {
		a.mu.Unlock()
		http.Error(w, "Sign-in busy", http.StatusServiceUnavailable)
		return
	}
	a.pending[state] = pending
	a.mu.Unlock()
	challenge := sha256.Sum256([]byte(pending.verifier))
	query := url.Values{"client_id": {provider.clientID}, "redirect_uri": {provider.redirectURL},
		"response_type": {"code"}, "scope": {provider.scope}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.SetCookie(w, &http.Cookie{Name: stateCookieName(r), Value: state, Path: "/",
		MaxAge: int(oauthStateLifetime.Seconds()), HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, provider.authorizeURL+"?"+query.Encode(), http.StatusSeeOther)
}

func (p oauthProvider) matchesRequest(r *http.Request) bool {
	redirect, err := url.Parse(p.redirectURL)
	if err != nil || (redirect.Scheme == "https") != (r.TLS != nil) {
		return false
	}
	if strings.EqualFold(redirect.Host, r.Host) {
		return true
	}
	defaultPort := 80
	if r.TLS != nil {
		defaultPort = 443
	}
	port, err := strconv.Atoi(redirect.Port())
	return err == nil && port == defaultPort &&
		strings.EqualFold(strings.TrimSuffix(redirect.Host, ":"+redirect.Port()), r.Host)
}

func (a *oauthAccess) callback(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	states := query["state"]
	if len(states) != 1 || len(states[0]) != 43 {
		http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
		return
	}
	cookieValue, unique := stateCookieValue(r)
	if !unique || len(cookieValue) != len(states[0]) || subtle.ConstantTimeCompare([]byte(cookieValue), []byte(states[0])) != 1 {
		http.Error(w, "Invalid sign-in state", http.StatusBadRequest)
		return
	}
	// Leave the one-use state and cookie available when all provider slots are busy.
	select {
	case a.exchanges <- struct{}{}:
		defer func() { <-a.exchanges }()
	default:
		http.Error(w, "Sign-in busy", http.StatusServiceUnavailable)
		return
	}
	a.mu.Lock()
	pending, found := a.pending[states[0]]
	delete(a.pending, states[0])
	if found && time.Now().Before(pending.expires) {
		a.inflight++
		a.byIP[pending.clientIP]++
		defer func() {
			a.mu.Lock()
			a.inflight--
			a.byIP[pending.clientIP]--
			a.mu.Unlock()
		}()
	}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: stateCookieName(r), Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	provider, configured := a.providers[pending.provider]
	if !found || !configured || !time.Now().Before(pending.expires) || !provider.matchesRequest(r) {
		http.Error(w, "Invalid sign-in state", http.StatusBadRequest)
		return
	}
	codes := query["code"]
	if len(codes) != 1 || len(codes[0]) == 0 || len(codes[0]) > 512 || len(query["error"]) != 0 {
		http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
		return
	}
	if pending.linkIdentity != "" && !a.sameLinkSession(r, pending) {
		http.Error(w, "Linking session expired", http.StatusForbidden)
		return
	}
	identity, err := a.fetchIdentity(r.Context(), provider, codes[0], pending.verifier)
	if err != nil {
		http.Error(w, "Provider sign-in failed", http.StatusBadGateway)
		return
	}
	key := identity.key()
	if pending.linkIdentity != "" {
		if !a.sameLinkSession(r, pending) {
			http.Error(w, "Linking session expired", http.StatusForbidden)
			return
		}
		if err := a.links.link(pending.linkIdentity, key); err != nil {
			http.Error(w, "Account link unavailable", http.StatusServiceUnavailable)
			return
		}
	} else {
		if a.links.account(key) == "" {
			http.Error(w, "Sign-in unavailable", http.StatusServiceUnavailable)
			return
		}
		sess := a.jaws.NewSession(w, r)
		if sess == nil {
			http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
			return
		}
		sess.Set(userSessionKey, key)
		sess.Set(userLabelKey, identity.Label)
	}
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

func (a *oauthAccess) sameLinkSession(r *http.Request, pending pendingOAuth) bool {
	sess := a.jaws.GetSession(r)
	key, _ := sess.Get(userSessionKey).(string)
	return sess != nil && sess == pending.linkSession && key == pending.linkIdentity && a.links.account(key) != ""
}

func (a *oauthAccess) logout(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	sess := a.jaws.GetSession(r)
	key, _ := sess.Get(userSessionKey).(string)
	if !validUserKey(key) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	sess.Clear()
	if cookie := sess.Close(); cookie != nil {
		http.SetCookie(w, cookie)
	}
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

func (a *oauthAccess) fetchIdentity(ctx context.Context, provider oauthProvider, code, verifier string) (userIdentity, error) {
	form := url.Values{"client_id": {provider.clientID}, "client_secret": {provider.clientSecret},
		"code": {code}, "redirect_uri": {provider.redirectURL}, "code_verifier": {verifier}}
	if provider.name == "gitlab" {
		form.Set("grant_type", "authorization_code")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := a.providerJSON(ctx, http.MethodPost, provider.tokenURL, strings.NewReader(form.Encode()), "", &token); err != nil {
		return userIdentity{}, err
	}
	if token.AccessToken == "" || len(token.AccessToken) > maxOAuthAccessToken ||
		strings.ContainsAny(token.AccessToken, " \t\r\n\x00") || !strings.EqualFold(token.TokenType, "bearer") {
		return userIdentity{}, errors.New("provider returned an invalid access token")
	}
	var profile struct {
		ID       int64  `json:"id"`
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	if err := a.providerJSON(ctx, http.MethodGet, provider.profileURL, nil, token.AccessToken, &profile); err != nil {
		return userIdentity{}, err
	}
	label := profile.Login
	if provider.name == "gitlab" {
		label = profile.Username
	}
	if profile.ID <= 0 || label == "" || len(label) > 256 || !utf8.ValidString(label) {
		return userIdentity{}, errors.New("provider returned an invalid user identity")
	}
	return userIdentity{Provider: provider.name, Host: provider.host, ID: profile.ID, Label: label}, nil
}

func (a *oauthAccess) providerJSON(ctx context.Context, method, target string, body io.Reader, token string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxOAuthResponse || json.Unmarshal(data, result) != nil {
		return errors.New("provider returned an invalid response")
	}
	return nil
}
