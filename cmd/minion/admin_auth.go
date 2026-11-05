package main

import (
	"crypto/sha256"
	_ "embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/jawsboot"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

const adminSessionKey = "minion.admin.username"

//go:embed head.html
var headHTML string

var adminLoginHTML = template.Must(template.New("admin login").Parse(headHTML + `<!doctype html>
<html lang="en" data-bs-theme="dark"><head>{{template "head.html" $}}<title>Minion admin</title></head>
<body><main class="container py-5">
{{if .Dot}}
<h1 class="h3 mb-3">Signed in</h1><p><a class="btn btn-primary" href="/">Open dashboard</a></p>
<form method="post" action="/admin/logout"><button class="btn btn-outline-secondary">Sign out</button></form>
{{else}}
<h1 class="h3 mb-3">Minion admin</h1>
<form method="post" action="/admin/login" class="vstack gap-3 col-12 col-md-6 col-lg-4">
<label class="form-label">Username <input class="form-control" name="username" autocomplete="username" required></label>
<label class="form-label">Password <input class="form-control" name="password" type="password" autocomplete="current-password" required></label>
<button class="btn btn-primary">Sign in</button></form><script defer src="/admin/login.js"></script>
{{end}}
</main>{{$.TailHTML}}</body></html>`))

//go:embed admin_login.js
var adminLoginJS []byte

var adminHomeHTML = template.Must(template.New("home").Parse(headHTML + `<!doctype html>
<html lang="en" data-bs-theme="dark"><head>{{template "head.html" $}}<title>Minion</title></head>
<body><main class="container py-5"><h1>Minion</h1>
{{if .Dot.Admin}}<a class="btn btn-primary me-2" href="/admin/login">Admin sign in</a>{{end}}
{{if .Dot.User}}<a class="btn btn-primary" href="/auth/login">User sign in</a>{{end}}
{{if not (or .Dot.Admin .Dot.User)}}<p role="status">Sign-in is not configured.</p><button class="btn btn-primary" disabled>Sign in</button>{{end}}
</main>{{$.TailHTML}}</body></html>`))

type adminHomeView struct{ Admin, User bool }

const (
	maxAdminSessions      = 512
	maxGuestLoginRequests = 4
	maxRotatedCookies     = 2048
)

type adminIdentity string

func (adminIdentity) Data() map[string]any { return nil }
func (adminIdentity) Email() string        { return "" }
func (id adminIdentity) IsAdmin() bool     { return id != "" }

type loginWindow struct {
	start time.Time
	count int
}

type adminAccess struct {
	jaws               *jaws.Jaws
	oauth              *oauthAccess
	actionAccess       *userActionAccess
	accounts           map[string]config.AdminAccount
	dummy              config.AdminAccount
	mu                 sync.Mutex
	attempts           map[string]loginWindow
	hashSlots          chan struct{}
	sessionAdmissionMu sync.Mutex
	rotatedCookies     map[[32]byte]time.Time
	pendingRotations   map[[32]byte]int
}

func newAdminHandler(jw *jaws.Jaws, cfg config.AdminConfig, owner *action.Orchestrator, oauth *oauthAccess) (http.Handler, error) {
	jw.MaxSessions = maxAdminSessions
	if err := registerAdminDashboard(jw); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminActionDetailTemplate); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminUsageHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminLoginHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminHomeHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminNoticeHTML); err != nil {
		return nil, err
	}
	access := &adminAccess{jaws: jw, oauth: oauth, accounts: make(map[string]config.AdminAccount, len(cfg.Administrators)), attempts: make(map[string]loginWindow), hashSlots: make(chan struct{}, 2), rotatedCookies: make(map[[32]byte]time.Time), pendingRotations: make(map[[32]byte]int)}
	for _, account := range cfg.Administrators {
		if account.PasswordHash == config.AdminPasswordHashPlaceholder {
			continue
		}
		access.accounts[account.Username] = account
		if access.dummy.Username == "" {
			access.dummy = account
		}
	}
	jw.MakeAuth = func(rq *jaws.Request) jaws.Auth { return adminIdentity(access.sessionUsername(rq.Session())) }
	mux := http.NewServeMux()
	if err := jw.Setup(func(pattern string, handler http.Handler) {
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			closeUnreadAdminBody(w, r)
			handler.ServeHTTP(w, r)
		}))
	}, "/static", jawsboot.Setup); err != nil {
		return nil, err
	}
	loginPage := jw.SessionMiddleware(http.HandlerFunc(access.loginPage))
	mux.HandleFunc("GET /admin/login", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		access.sessionAdmissionMu.Lock()
		defer access.sessionAdmissionMu.Unlock()
		if access.rejectRotatedCookie(w, r) {
			return
		}
		loginPage.ServeHTTP(w, r)
	})
	mux.HandleFunc("HEAD /admin/login", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	})
	mux.HandleFunc("GET /admin/login.js", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if r.Method == http.MethodGet {
			_, _ = w.Write(adminLoginJS)
		}
	})
	mux.HandleFunc("POST /admin/login", access.login)
	mux.HandleFunc("POST /admin/logout", access.logout)
	mux.HandleFunc("POST /admin/stop", func(w http.ResponseWriter, r *http.Request) {
		access.stop(w, r, owner)
	})
	dashboard := adminDashboardHandler(jw, owner)
	home := jw.SessionMiddleware(http.HandlerFunc(access.homePage))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		sess := jw.GetSession(r)
		if access.sessionUsername(sess) != "" {
			dashboard.ServeHTTP(w, r)
			return
		}
		if oauth != nil && oauth.sessionAccount(r) != "" {
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		access.sessionAdmissionMu.Lock()
		defer access.sessionAdmissionMu.Unlock()
		if access.rejectRotatedCookie(w, r) {
			return
		}
		home.ServeHTTP(w, r)
	})
	mux.HandleFunc("HEAD /{$}", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	})
	mux.Handle("GET /admin/actions/{attempt}", adminActionDetailHandler(jw, owner))
	mux.Handle("GET /admin/usage", adminUsageHandler(jw, owner, false))
	mux.Handle("GET /admin/usage.json", adminUsageHandler(jw, owner, true))
	if oauth != nil && owner != nil {
		access.actionAccess = &userActionAccess{oauth: oauth, owner: owner}
		if err := oauth.actionRoutes(access.actionAccess); err != nil {
			return nil, err
		}
	}
	oauth.routes(mux, access)
	if oauth != nil {
		mux.Handle("GET /user/usage", oauth.usageHandler(owner))
	}
	// Pinned JaWS endpoints ignore HTTP bodies. Revisit this if JaWS adds a
	// body-reading callback, which must not inherit an immediate read deadline.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		jw.ServeHTTP(w, r)
	})
	crossOrigin := http.NewCrossOriginProtection()
	return jw.SecureHeadersMiddleware(access.browserErrors(adminGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if crossSchemeAdminOrigin(r) || crossOrigin.Check(r) != nil {
			closeUnreadAdminBody(w, r)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}), func(r *http.Request) bool {
		return validJaWSCookie(r, jw.CookieName) &&
			(access.allowed(r) || oauth.allowed(r))
	}))), nil
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

func validJaWSCookie(r *http.Request, name string) bool {
	seen := false
	cookies := r.Cookies()
	// Go drops all cookies when a header has too many parts; JaWS still reads it.
	if len(r.Header.Values("Cookie")) != 0 && len(cookies) == 0 {
		return false
	}
	for _, cookie := range cookies {
		if cookie.Name == name {
			if seen {
				return false
			}
			seen = true
		}
	}
	return true
}

func (a *adminAccess) allowed(r *http.Request) bool {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/" {
		return true
	}
	if len(a.accounts) != 0 && r.URL.Path == "/admin/login" &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost) {
		return true
	}
	if len(a.accounts) != 0 && r.URL.Path == "/admin/logout" && r.Method == http.MethodPost {
		return true
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// JaWS fingerprints its bundled CSS and JavaScript asset paths.
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/admin/login.js" || r.URL.Path == "/jaws/.ping" ||
			strings.HasPrefix(r.URL.Path, "/jaws/.jaws.") &&
				(strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css")) {
			return true
		}
	}
	if strings.HasPrefix(r.URL.Path, "/jaws/") {
		return a.ownsJaWSRequest(r)
	}
	return a.sessionUsername(a.jaws.GetSession(r)) != ""
}

func (a *adminAccess) ownsJaWSRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	var key string
	if strings.HasPrefix(r.URL.Path, "/jaws/.tail/") {
		key = strings.TrimPrefix(r.URL.Path, "/jaws/.tail/")
	} else {
		key = strings.TrimPrefix(r.URL.Path, "/jaws/")
		key = strings.TrimSuffix(key, "/noscript")
	}
	if key == "" {
		return false
	}
	for _, rq := range a.jaws.GetSession(r).Requests() {
		if rq.JawsKeyString() == key {
			if initial := rq.Initial(); initial != nil && initial.Context().Value(userErrorPageKey{}) == true {
				return true
			} else if initial != nil && strings.HasPrefix(initial.URL.Path, "/auth/actions") {
				return a.actionAccess != nil && a.actionAccess.authorizeJaWSRequest(r.Context(), a.jaws.GetSession(r), initial)
			}
			return true
		}
	}
	return false
}

func (a *adminAccess) loginPage(w http.ResponseWriter, r *http.Request) {
	sess := a.jaws.GetSession(r)
	signedIn := a.sessionUsername(sess) != ""
	a.publicPage(w, r, "admin login", signedIn)
}

func (a *adminAccess) homePage(w http.ResponseWriter, r *http.Request) {
	a.publicPage(w, r, "home", adminHomeView{Admin: len(a.accounts) != 0, User: a.oauth != nil})
}

func (a *adminAccess) publicPage(w http.ResponseWriter, r *http.Request, name string, view any) {
	sess := a.jaws.GetSession(r)
	if a.sessionUsername(sess) == "" && len(sess.Requests()) >= maxGuestLoginRequests {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Too many public pages", http.StatusTooManyRequests)
		return
	}
	ui.Handler(a.jaws, name, view).ServeHTTP(w, r)
}

// rememberRotatedCookie is called under sessionAdmissionMu before closing the old session.
func (a *adminAccess) rememberRotatedCookie(value string) bool {
	now := time.Now()
	for key, until := range a.rotatedCookies {
		if a.pendingRotations[key] == 0 && !now.Before(until) {
			delete(a.rotatedCookies, key)
		}
	}
	key := sha256.Sum256([]byte(value))
	if _, known := a.rotatedCookies[key]; !known && len(a.rotatedCookies) >= maxRotatedCookies {
		return false
	}
	a.rotatedCookies[key] = now.Add(time.Minute)
	return true
}

// rejectRotatedCookie is called under sessionAdmissionMu before minting a guest cookie.
func (a *adminAccess) rejectRotatedCookie(w http.ResponseWriter, r *http.Request) bool {
	if cookie, err := r.Cookie(a.jaws.CookieName); err == nil {
		key := sha256.Sum256([]byte(cookie.Value))
		if a.pendingRotations[key] > 0 || time.Now().Before(a.rotatedCookies[key]) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Sign-in session changed; reload. If this continues, retry in one minute", http.StatusConflict)
			return true
		}
	}
	return false
}

// forgetRotatedCookie is called under sessionAdmissionMu when sign-out closes a session.
func (a *adminAccess) forgetRotatedCookie(sess *jaws.Session) {
	key := sha256.Sum256([]byte(sess.CookieValue()))
	delete(a.pendingRotations, key)
	delete(a.rotatedCookies, key)
}

// serveOAuthCallback holds the old cookie out of guest admission until every callback using it finishes.
func (a *adminAccess) serveOAuthCallback(w http.ResponseWriter, r *http.Request, next http.Handler) {
	original := r.Clone(r.Context()) // JaWS adds the rotated cookie to r during the callback.
	a.sessionAdmissionMu.Lock()
	old := a.jaws.GetSession(original)
	if old == nil {
		a.sessionAdmissionMu.Unlock()
		next.ServeHTTP(w, r)
		return
	}
	value := old.CookieValue()
	if !a.rememberRotatedCookie(value) {
		a.sessionAdmissionMu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	key := sha256.Sum256([]byte(value))
	a.pendingRotations[key]++
	a.sessionAdmissionMu.Unlock()
	defer func() {
		a.sessionAdmissionMu.Lock()
		if a.pendingRotations[key] == 0 {
			a.sessionAdmissionMu.Unlock()
			return // sign-out canceled this cookie's callbacks
		}
		a.pendingRotations[key]--
		if a.pendingRotations[key] == 0 {
			delete(a.pendingRotations, key)
			if a.jaws.GetSession(original) == old {
				delete(a.rotatedCookies, key)
			} else {
				a.rotatedCookies[key] = time.Now().Add(time.Minute)
			}
		}
		a.sessionAdmissionMu.Unlock()
	}()
	next.ServeHTTP(w, r)
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
	// A 64-code-point password can expand to 768 URL-encoded bytes.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	if err := r.ParseForm(); err != nil {
		closeUnreadAdminBody(w, r)
		http.Error(w, "Invalid login request", http.StatusBadRequest)
		return
	}
	username, password := r.PostForm.Get("username"), r.PostForm.Get("password")
	if username == "" || !config.ValidAdminPassword(password) || len(username) > 100 ||
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
	a.sessionAdmissionMu.Lock()
	if a.oauth != nil {
		a.oauth.linkMu.Lock()
		defer a.oauth.linkMu.Unlock()
	}
	previous := a.jaws.GetSession(r)
	if previous == nil {
		a.sessionAdmissionMu.Unlock()
		http.Error(w, "Sign-in session unavailable; load the login page and try again", http.StatusConflict)
		return
	}
	previousCookie := previous.CookieValue()
	if !a.rememberRotatedCookie(previousCookie) {
		a.sessionAdmissionMu.Unlock()
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	previous.Clear()
	previous.Close()
	sess := a.jaws.NewSession(w, r)
	if sess != nil {
		sess.Set(adminSessionKey, username)
	} else {
		delete(a.rotatedCookies, sha256.Sum256([]byte(previousCookie)))
	}
	a.sessionAdmissionMu.Unlock()
	if sess == nil {
		http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
		return
	}
	a.mu.Lock()
	delete(a.attempts, username)
	a.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *adminAccess) logout(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	a.sessionAdmissionMu.Lock()
	if a.oauth != nil {
		a.oauth.linkMu.Lock()
		defer a.oauth.linkMu.Unlock()
	}
	sess := a.jaws.GetSession(r)
	if sess != nil {
		a.forgetRotatedCookie(sess)
		sess.Clear()
		sess.Close()
	}
	a.sessionAdmissionMu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
