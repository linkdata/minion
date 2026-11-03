package main

import (
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

const adminSessionKey = "minion.admin.username"

const loginPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Minion admin</title></head><body><main><h1>Minion admin</h1><form method="post" action="/admin/login"><label>Username <input name="username" autocomplete="username" required></label><label>Password <input name="password" type="password" autocomplete="current-password" required></label><button>Sign in</button></form></main></body></html>`
const signedInPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Minion admin</title></head><body><main><h1>Signed in</h1><a href="/">Open dashboard</a><form method="post" action="/admin/logout"><button>Sign out</button></form></main></body></html>`

type adminIdentity string

func (adminIdentity) Data() map[string]any { return nil }
func (adminIdentity) Email() string        { return "" }
func (id adminIdentity) IsAdmin() bool     { return id != "" }

type loginWindow struct {
	start time.Time
	count int
}

type adminAccess struct {
	jaws      *jaws.Jaws
	accounts  map[string]config.AdminAccount
	dummy     config.AdminAccount
	mu        sync.Mutex
	attempts  map[string]loginWindow
	hashSlots chan struct{}
}

func newAdminHandler(jw *jaws.Jaws, cfg config.AdminConfig, owner *action.Orchestrator) (http.Handler, error) {
	if err := registerAdminDashboard(jw); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminUsageHTML); err != nil {
		return nil, err
	}
	access := &adminAccess{jaws: jw, accounts: make(map[string]config.AdminAccount, len(cfg.Administrators)), attempts: make(map[string]loginWindow), hashSlots: make(chan struct{}, 2)}
	for _, account := range cfg.Administrators {
		access.accounts[account.Username] = account
		if access.dummy.Username == "" {
			access.dummy = account
		}
	}
	jw.MakeAuth = func(rq *jaws.Request) jaws.Auth { return adminIdentity(access.sessionUsername(rq.Session())) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/login", access.loginPage)
	mux.HandleFunc("POST /admin/login", access.login)
	mux.HandleFunc("POST /admin/logout", access.logout)
	mux.HandleFunc("POST /admin/stop", func(w http.ResponseWriter, r *http.Request) {
		access.stop(w, r, owner)
	})
	mux.Handle("GET /{$}", adminDashboardHandler(jw, owner))
	mux.HandleFunc("GET /admin/dashboard.css", serveAdminDashboardCSS)
	mux.Handle("GET /admin/usage", adminUsageHandler(jw, owner, false))
	mux.Handle("GET /admin/usage.json", adminUsageHandler(jw, owner, true))
	// Pinned JaWS endpoints ignore HTTP bodies. Revisit this if JaWS adds a
	// body-reading callback, which must not inherit an immediate read deadline.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		jw.ServeHTTP(w, r)
	})
	crossOrigin := http.NewCrossOriginProtection()
	return jw.SecureHeadersMiddleware(adminGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if crossSchemeAdminOrigin(r) || crossOrigin.Check(r) != nil {
			closeUnreadAdminBody(w, r)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}), access.allowed)), nil
}

func (a *adminAccess) stop(w http.ResponseWriter, r *http.Request, owner *action.Orchestrator) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		closeUnreadAdminBody(w, r)
		http.Error(w, "Invalid stop request", http.StatusBadRequest)
		return
	}
	jobID, attemptID := r.PostForm.Get("job_id"), r.PostForm.Get("attempt_id")
	if len(r.PostForm["job_id"]) != 1 || len(r.PostForm["attempt_id"]) != 1 ||
		!workspace.ValidIdentitySegment(jobID) || !workspace.ValidIdentitySegment(attemptID) {
		closeUnreadAdminBody(w, r)
		http.Error(w, "Invalid stop request", http.StatusBadRequest)
		return
	}
	closeUnreadAdminBody(w, r)
	if owner == nil || owner.Store == nil {
		http.Error(w, "Orchestrator state unavailable", http.StatusServiceUnavailable)
		return
	}
	requestedBy := a.sessionUsername(a.jaws.GetSession(r))
	if requestedBy == "" {
		http.Error(w, "Admin session ended", http.StatusForbidden)
		return
	}
	_, err := owner.Store.RequestStop(jobID, attemptID, requestedBy, time.Now().UTC())
	if errors.Is(err, state.ErrStopConflict) {
		http.Error(w, "Selected attempt is not stoppable", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "Stop persistence could not be confirmed; reload the dashboard", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// CrossOriginProtection's legacy Origin fallback compares hosts but not schemes.
func crossSchemeAdminOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if raw := r.Header.Get("Origin"); raw != "" {
		origin, err := url.Parse(raw)
		return err != nil || origin.Scheme != scheme
	}
	return false
}

func (a *adminAccess) sessionUsername(sess *jaws.Session) string {
	username, _ := sess.Get(adminSessionKey).(string)
	if _, ok := a.accounts[username]; ok {
		return username
	}
	return ""
}

func (a *adminAccess) allowed(r *http.Request) bool {
	if len(a.accounts) == 0 {
		return false
	}
	if r.URL.Path == "/admin/login" && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost) {
		return true
	}
	if r.URL.Path == "/admin/logout" && r.Method == http.MethodPost {
		return true
	}
	return a.sessionUsername(a.jaws.GetSession(r)) != ""
}

func (a *adminAccess) loginPage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if a.sessionUsername(a.jaws.GetSession(r)) != "" {
		_, _ = w.Write([]byte(signedInPage))
		return
	}
	_, _ = w.Write([]byte(loginPage))
}

func (a *adminAccess) reserveAttempt(username string) bool {
	if _, ok := a.accounts[username]; !ok {
		username = ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	window := a.attempts[username]
	now := time.Now()
	if window.start.IsZero() || now.Sub(window.start) >= time.Minute {
		window = loginWindow{start: now}
	}
	if window.count >= 5 {
		return false
	}
	window.count++
	a.attempts[username] = window
	return true
}

func (a *adminAccess) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// A 4096-byte password can triple when URL-encoded, plus the form field names.
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		closeUnreadAdminBody(w, r)
		http.Error(w, "Invalid login request", http.StatusBadRequest)
		return
	}
	username, password := r.PostForm.Get("username"), r.PostForm.Get("password")
	if username == "" || password == "" || len(username) > 100 || len(password) > 4096 ||
		len(r.PostForm["username"]) != 1 || len(r.PostForm["password"]) != 1 {
		closeUnreadAdminBody(w, r)
		http.Error(w, "Invalid login request", http.StatusBadRequest)
		return
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		http.Error(w, "Login busy", http.StatusServiceUnavailable)
		return
	}
	if !a.reserveAttempt(username) {
		http.Error(w, "Too many login attempts", http.StatusTooManyRequests)
		return
	}
	account, exists := a.accounts[username]
	if !exists {
		account = a.dummy
	}
	valid := account.VerifyPassword(password)
	if !exists || !valid {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}
	sess := a.jaws.NewSession(w, r)
	if sess == nil {
		http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
		return
	}
	sess.Set(adminSessionKey, username)
	a.mu.Lock()
	delete(a.attempts, username)
	a.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *adminAccess) logout(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	sess := a.jaws.GetSession(r)
	if sess == nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	sess.Clear()
	if cookie := sess.Close(); cookie != nil {
		http.SetCookie(w, cookie)
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}
