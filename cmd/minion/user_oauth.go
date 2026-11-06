package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/jawsauth"
	"github.com/linkdata/minion/internal/config"
)

var userLoginHTML = template.Must(template.New("user login").Parse(headHTML + `<!doctype html>
<html lang="en" data-bs-theme="dark"><head>{{template "head.html" $}}<title>Minion account</title></head>
<body><main class="container py-4">
{{if .Dot.Error}}<p role="alert" class="alert alert-danger">Request could not be completed. Try again.</p>{{end}}
{{if .Dot.SignedIn}}
<h1>Signed in with {{.Dot.Provider}}</h1>
<p><a href="/user/usage">Your usage</a></p>
{{if .Dot.GitHub}}<form method="post" action="/auth/link/github"><button class="btn btn-primary">Link GitHub account</button></form>{{end}}
{{if .Dot.GitLab}}<form method="post" action="/auth/link/gitlab"><button class="btn btn-primary">Link GitLab account</button></form>{{end}}
{{if .Dot.Preferences}}<p><a href="/auth/preferences">Model preferences</a></p>{{end}}
<p><a href="/auth/actions">My actions</a></p>
<form method="post" action="/auth/logout"><button class="btn btn-outline-light">Sign out</button></form>
{{else}}
<h1>Sign in</h1>
{{if .Dot.GitHub}}<p><a class="btn btn-primary" href="/auth/github/login{{.Dot.SignInSuffix}}">Sign in with GitHub</a></p>{{end}}
{{if .Dot.GitLab}}<p><a class="btn btn-primary" href="/auth/gitlab/login{{.Dot.SignInSuffix}}">Sign in with GitLab</a></p>{{end}}
{{end}}
</main>{{$.TailHTML}}</body></html>`))

var userErrorHTML = template.Must(template.New("user error").Parse(headHTML + `<!doctype html>
<html lang="en" data-bs-theme="dark"><head>{{template "head.html" $}}<title>Minion request failed</title></head>
<body><main class="container py-4"><h1>Request failed</h1><p role="alert" class="alert alert-danger">Request could not be completed. Try again.</p>
{{if .Dot.RetryAfter}}<p role="status" class="alert alert-info">Wait {{.Dot.RetryAfter}} seconds before retrying.</p>{{end}}
<a href="/auth/login">Return to account</a></main>{{$.TailHTML}}</body></html>`))

type userErrorView struct{ RetryAfter string }

type userLoginView struct {
	SignedIn       bool
	Error          bool
	Provider       string
	GitHub, GitLab bool
	Preferences    bool
	SignInSuffix   string
}

type userErrorPageKey struct{}

type oauthProvider struct {
	name, issuer, host string
	server             *jawsauth.Server
}

func (p *oauthProvider) identity(claims map[string]any) (id userIdentity, ok bool) {
	if p == nil || claims == nil {
		return
	}
	issuer, _ := claims["iss"].(string)
	subject, _ := claims["sub"].(string)
	id = userIdentity{Provider: p.name, Host: p.host, Subject: subject}
	return id, issuer == p.issuer && validUserKey(id.key())
}

type oauthAccess struct {
	jaws        *jaws.Jaws
	links       *userLinks
	mux         *http.ServeMux
	providers   []*oauthProvider
	choices     *config.UserModelChoices
	detailSlots chan struct{}
	actionReads chan struct{}
	linkMu      sync.Mutex
	usageGate   *userUsageGate
}

// Keep 32 slots for admin login pages and session rotation while OAuth flows wait at providers.
const maxOAuthAdmissionSessions = maxAdminSessions - 32

func githubProfileSubject(claims map[string]any) (string, error) {
	raw, ok := claims["id"].(json.Number)
	if !ok {
		return "", jawsauth.ErrOAuth2InvalidProfile
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != string(raw) {
		return "", jawsauth.ErrOAuth2InvalidProfile
	}
	return string(raw), nil
}

func newOAuthAccess(jw *jaws.Jaws, cfg config.AdminConfig, statePath string, client *http.Client) (*oauthAccess, error) {
	if cfg.GitHubOAuth == nil && cfg.GitLabOAuth == nil {
		return nil, nil
	}
	if statePath == "" {
		return nil, errors.New("OAuth sign-in requires a state path")
	}
	links, err := openUserLinks(statePath + ".link")
	if err != nil {
		return nil, fmt.Errorf("open user links: %w", err)
	}
	a := &oauthAccess{jaws: jw, links: links, mux: http.NewServeMux(), choices: cfg.UserModelChoices,
		usageGate: newUserUsageGate(), detailSlots: make(chan struct{}, 2), actionReads: make(chan struct{}, 2)}
	if err := jw.AddTemplateLookuper(userLoginHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(userErrorHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(userUsageHTML); err != nil {
		return nil, err
	}
	page := jw.SessionMiddleware(http.HandlerFunc(a.loginPage))
	a.mux.Handle("GET /auth/login", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		if jw.GetSession(r) == nil && jw.SessionCount() >= maxOAuthAdmissionSessions-1 {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		page.ServeHTTP(&oauthErrorWriter{ResponseWriter: w}, r)
	}))
	a.mux.HandleFunc("HEAD /auth/login", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	})
	if a.choices != nil {
		if err := jw.AddTemplateLookuper(userPreferencesHTML); err != nil {
			return nil, err
		}
		a.mux.Handle("GET /auth/preferences", a.oauthErrors(http.HandlerFunc(a.preferencePage)))
		// The preference form must be read before its body can be closed.
		a.mux.Handle("POST /auth/preferences", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a.savePreferences(&oauthErrorWriter{ResponseWriter: w, oauth: a, request: r}, r)
		}))
	}
	if c := cfg.GitHubOAuth; c != nil {
		if err := a.addProvider("github", "https://github.com", "github.com", &jawsauth.Config{
			Namespace: "github", RedirectURL: c.RedirectURL, Issuer: "https://github.com",
			AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token",
			UserInfoURL: "https://api.github.com/user", Scopes: []string{"read:user"},
			ClientID: c.ClientID, ClientSecret: c.ClientSecret, HTTPClient: client,
			ProfileSubject: githubProfileSubject,
		}); err != nil {
			return nil, err
		}
	}
	if c := cfg.GitLabOAuth; c != nil {
		base := c.BaseURL
		if base == "" {
			base = "https://gitlab.com"
		}
		if err := a.addProvider("gitlab", base, strings.TrimPrefix(base, "https://"), &jawsauth.Config{
			Namespace: "gitlab", RedirectURL: c.RedirectURL, Issuer: base,
			ClientID: c.ClientID, ClientSecret: c.ClientSecret, HTTPClient: client,
		}); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *oauthAccess) addProvider(name, issuer, host string, cfg *jawsauth.Config) error {
	p := &oauthProvider{name: name, issuer: issuer, host: host}
	srv, err := jawsauth.New(a.jaws, cfg, func(path string, handler http.Handler) {
		if !strings.HasSuffix(path, "/logout") {
			handler = a.oauthErrors(handler)
			if strings.HasSuffix(path, "/login") {
				login := handler
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if a.jaws.SessionCount() >= maxOAuthAdmissionSessions {
						w.Header().Set("Cache-Control", "no-store")
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					returnTo := userUsageReturnTarget(r.URL.Query().Get("next"))
					if returnTo == "" {
						returnTo = "/auth/login"
					}
					r.Header.Set("Referer", returnTo)
					login.ServeHTTP(w, r)
				})
			}
			a.mux.Handle("GET "+path, handler)
		}
	})
	if err != nil {
		return fmt.Errorf("configure %s sign-in: %w", name, err)
	}
	p.server = srv
	srv.SessionRotationLock = &a.linkMu
	srv.LinkEvent = func(sess *jaws.Session, _ *http.Request, claims map[string]any) (int, error) {
		current, source := a.currentUser(sess)
		proved, ok := p.identity(claims)
		if current == "" || source == p || !ok {
			return http.StatusForbidden, errors.New("account proof ended")
		}
		if err := a.links.link(current, proved.key()); err != nil {
			if errors.Is(err, errUserLinkConflict) {
				return http.StatusConflict, err
			}
			return http.StatusServiceUnavailable, err
		}
		return 0, nil
	}
	a.mux.Handle("POST /auth/link/"+name, a.oauthErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current, source := a.currentUser(a.jaws.GetSession(r)); current == "" || source == p {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		r.Header.Set("Referer", "/auth/login")
		srv.HandleLink(w, r)
	})))
	a.providers = append(a.providers, p)
	return nil
}

func (a *oauthAccess) currentUser(sess *jaws.Session) (account string, provider *oauthProvider) {
	if a == nil || sess == nil {
		return
	}
	for _, candidate := range a.providers {
		if id, ok := candidate.identity(candidate.server.CurrentClaims(sess)); ok {
			if account = a.links.account(id.key()); account != "" {
				provider = candidate
				return
			}
		}
	}
	return
}

func (a *oauthAccess) allowed(r *http.Request) bool {
	if a == nil {
		return false
	}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/user/usage":
			return true // The handler redirects anonymous readers to sign-in.
		}
	}
	_, pattern := a.mux.Handler(r)
	return pattern != ""
}

func (a *oauthAccess) sessionAccount(r *http.Request) string {
	account, _ := a.currentUser(a.jaws.GetSession(r))
	return account
}

// Only a canonical usage URL may survive an unauthenticated sign-in detour.
func userUsageReturnTarget(raw string) string {
	if len(raw) == 0 || len(raw) > 1024 {
		return ""
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "" || target.Host != "" || target.User != nil ||
		target.Opaque != "" || target.Fragment != "" || target.Path != "/user/usage" {
		return ""
	}
	repo, after, ok := parseUserUsageQuery(target.RawQuery)
	if !ok {
		return ""
	}
	return userUsageContinueURL(repo, after)
}

func (a *oauthAccess) routes(mux *http.ServeMux, admin *adminAccess) {
	if a == nil {
		return
	}
	for _, provider := range a.providers {
		provider.server.LoginEvent = admin.recordOAuthRotation
	}
	a.mux.Handle("POST /auth/logout", a.oauthErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.logout(w, r, admin)
	})))
	mux.Handle("/auth/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			guestPage, callback := r.URL.Path == "/auth/login", false
			for _, provider := range a.providers {
				path := "/auth/" + provider.name
				guestPage = guestPage || r.URL.Path == path+"/login"
				callback = callback || r.URL.Path == path+"/callback"
			}
			if callback {
				admin.serveOAuthCallback(w, r, a.mux)
				return
			}
			if guestPage {
				admin.sessionAdmissionMu.Lock()
				defer admin.sessionAdmissionMu.Unlock()
				if admin.rejectRotatedCookie(w, r) {
					return
				}
				if admin.redirectClosedSession(w, r) {
					return
				}
			}
		}
		a.mux.ServeHTTP(w, r)
	}))
}

func (a *oauthAccess) loginPage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	sess := a.jaws.GetSession(r)
	account, provider := a.currentUser(sess)
	if account == "" && len(sess.Requests()) >= maxGuestPageRequests {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	view := userLoginView{SignedIn: account != "", Error: r.URL.Query().Get("error") == "request", Preferences: account != "" && a.choices != nil}
	if returnTo := userUsageReturnTarget(r.URL.Query().Get("next")); returnTo != "" {
		view.SignInSuffix = "?next=" + url.QueryEscape(returnTo)
	}
	if provider != nil {
		view.Provider = provider.name
	}
	canLink := account == "" || !a.links.hasAlias(account)
	for _, p := range a.providers {
		switch p.name {
		case "github":
			view.GitHub = provider != p && canLink
		case "gitlab":
			view.GitLab = provider != p && canLink
		}
	}
	ui.Handler(a.jaws, "user login", view).ServeHTTP(w, r)
}

func (a *oauthAccess) logout(w http.ResponseWriter, r *http.Request, admin *adminAccess) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	admin.sessionAdmissionMu.Lock()
	a.linkMu.Lock()
	sess := a.jaws.GetSession(r)
	if sess == nil {
		sess, _ = admin.rotatedSessionForLogout(r)
	}
	if sess != nil {
		account, _ := a.currentUser(sess)
		if admin.sessionUsername(sess) != "" || account != "" {
			admin.markSignedOutCookie(sess.CookieValue())
		}
		sess.Close()
	}
	a.linkMu.Unlock()
	admin.sessionAdmissionMu.Unlock()
	if sess != nil {
		for _, p := range a.providers {
			p.server.Logout(sess, r)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type oauthErrorWriter struct {
	http.ResponseWriter
	oauth   *oauthAccess
	request *http.Request
	failed  bool
}

func (w *oauthErrorWriter) WriteHeader(status int) {
	w.Header().Del("Strict-Transport-Security")
	if status >= 400 {
		w.failed = true
		if w.oauth != nil {
			w.oauth.browserError(w.ResponseWriter, w.request, status)
			return
		}
		w.Header().Del("Content-Type")
		w.Header().Del("Content-Length")
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *oauthErrorWriter) Write(body []byte) (int, error) {
	if w.failed {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func (a *oauthAccess) browserError(w http.ResponseWriter, r *http.Request, status int) {
	closeUnreadAdminBody(w, r)
	w.Header().Del("Strict-Transport-Security")
	w.Header().Del("Content-Type")
	w.Header().Del("Content-Length")
	w.Header().Set("Cache-Control", "no-store")
	sess := a.jaws.GetSession(r)
	if sess == nil {
		if a.jaws.SessionCount() >= maxOAuthAdmissionSessions {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Location", "/auth/login?error=request")
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	for _, p := range a.providers {
		if _, ok := p.identity(p.server.CurrentClaims(sess)); ok {
			view := userErrorView{}
			if status == http.StatusTooManyRequests {
				view.RetryAfter = w.Header().Get("Retry-After")
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			ui.Handler(a.jaws, "user error", view).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userErrorPageKey{}, true)))
			return
		}
	}
	w.Header().Set("Location", "/auth/login?error=request")
	w.WriteHeader(http.StatusSeeOther)
}

func (a *oauthAccess) oauthErrors(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		handler.ServeHTTP(&oauthErrorWriter{ResponseWriter: w, oauth: a, request: r}, r)
	})
}
