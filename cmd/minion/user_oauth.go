package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/jawsauth"
	"github.com/linkdata/minion/internal/config"
)

var userLoginHTML = template.Must(template.New("user login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Minion account</title>{{$.HeadHTML}}</head>
<body><main>
{{if .Dot.Error}}<p role="alert">Request could not be completed. Try again.</p>{{end}}
{{if .Dot.SignedIn}}
<h1>Signed in with {{.Dot.Provider}}</h1>
{{if .Dot.GitHub}}<form method="post" action="/auth/link/github"><button>Link GitHub account</button></form>{{end}}
{{if .Dot.GitLab}}<form method="post" action="/auth/link/gitlab"><button>Link GitLab account</button></form>{{end}}
<form method="post" action="/auth/logout"><button>Sign out</button></form>
{{else}}
<h1>Sign in</h1>
{{if .Dot.GitHub}}<p><a href="/auth/github/login">Sign in with GitHub</a></p>{{end}}
{{if .Dot.GitLab}}<p><a href="/auth/gitlab/login">Sign in with GitLab</a></p>{{end}}
{{end}}
</main><script defer src="/admin/login.js"></script>{{$.TailHTML}}</body></html>`))

var userErrorHTML = template.Must(template.New("user error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Minion request failed</title>{{$.HeadHTML}}</head>
<body><main><h1>Request failed</h1><p role="alert">Request could not be completed. Try again.</p>
<a href="/auth/login">Return to account</a></main>{{$.TailHTML}}</body></html>`))

type userLoginView struct {
	SignedIn       bool
	Error          bool
	Provider       string
	GitHub, GitLab bool
}

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
	admissionMu sync.Mutex
	linkMu      sync.Mutex
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
	a := &oauthAccess{jaws: jw, links: links, mux: http.NewServeMux()}
	if err := jw.AddTemplateLookuper(userLoginHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(userErrorHTML); err != nil {
		return nil, err
	}
	page := jw.SessionMiddleware(http.HandlerFunc(a.loginPage))
	a.mux.Handle("GET /auth/login", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		a.admissionMu.Lock()
		defer a.admissionMu.Unlock()
		if jw.GetSession(r) == nil && jw.SessionCount() >= maxOAuthAdmissionSessions {
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
	a.mux.Handle("POST /auth/logout", a.oauthErrors(http.HandlerFunc(a.logout)))
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
					a.admissionMu.Lock()
					defer a.admissionMu.Unlock()
					if a.jaws.SessionCount() >= maxOAuthAdmissionSessions {
						w.Header().Set("Cache-Control", "no-store")
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					r.Header.Set("Referer", "/auth/login")
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
		a.linkMu.Lock()
		defer a.linkMu.Unlock()
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
	_, pattern := a.mux.Handler(r)
	return pattern != ""
}

func (a *oauthAccess) routes(mux *http.ServeMux) {
	if a != nil {
		mux.Handle("/auth/", a.mux)
	}
}

func (a *oauthAccess) loginPage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	sess := a.jaws.GetSession(r)
	account, provider := a.currentUser(sess)
	if account == "" && len(sess.Requests()) >= maxGuestLoginRequests {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	view := userLoginView{SignedIn: account != "", Error: r.URL.Query().Get("error") == "request"}
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

func (a *oauthAccess) logout(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	a.linkMu.Lock()
	defer a.linkMu.Unlock()
	sess := a.jaws.GetSession(r)
	if account, _ := a.currentUser(sess); account == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	for _, p := range a.providers {
		p.server.Logout(sess, r)
	}
	sess.Close()
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
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
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			ui.Handler(a.jaws, "user error", nil).ServeHTTP(w, r)
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
