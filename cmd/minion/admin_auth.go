package main

import (
	"crypto/sha256"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
<form method="post" action="/admin/logout"><button class="btn btn-outline-light">Sign out</button></form>
{{else}}
<h1 class="h3 mb-3">Minion admin</h1>
<form method="post" action="/admin/login" class="vstack gap-3 col-md-6 col-lg-4">
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
{{if not (or .Dot.Admin .Dot.User)}}<p>Sign-in is not configured.</p><button class="btn btn-primary" disabled>Sign in</button>{{end}}
</main>{{$.TailHTML}}</body></html>`))

type adminHomeView struct{ Admin, User bool }

const (
	maxAdminSessions          = 512
	maxGuestPageRequests      = 4
	maxRotatedCookies         = 2048
	adminSignInReserve        = 32
	maxGuestAdmissionSessions = maxAdminSessions - adminSignInReserve
	// One sign-out marker may be needed for each live JaWS session.
	maxSignInRotations = maxRotatedCookies - maxAdminSessions
	// OAuth leaves room for those sign-outs and admin password rotations.
	maxOAuthRotations = maxSignInRotations - maxAdminSessions - adminSignInReserve
)

type adminIdentity string

func headPublicPage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

func (adminIdentity) Data() map[string]any { return nil }
func (adminIdentity) Email() string        { return "" }
func (id adminIdentity) IsAdmin() bool     { return id != "" }

type loginWindow struct {
	start time.Time
	count int
}

type rotatedCookie struct {
	until       time.Time
	pending     int
	signedOut   bool
	homeIssued  bool
	originIP    netip.Addr
	replacement *jaws.Session
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
	rotatedCookies     map[[32]byte]rotatedCookie
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
	access := &adminAccess{jaws: jw, oauth: oauth, accounts: make(map[string]config.AdminAccount, len(cfg.Administrators)), attempts: make(map[string]loginWindow), hashSlots: make(chan struct{}, 2), rotatedCookies: make(map[[32]byte]rotatedCookie)}
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
		if access.redirectClosedSession(w, r) {
			return
		}
		loginPage.ServeHTTP(w, r)
	})
	mux.HandleFunc("HEAD /admin/login", headPublicPage)
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
		var signedOutKey [32]byte
		signedOutHome := false
		if cookie, err := r.Cookie(jw.CookieName); err == nil {
			signedOutKey = sha256.Sum256([]byte(cookie.Value))
			state := access.rotatedCookies[signedOutKey]
			if state.signedOut && time.Now().Before(state.until) {
				if state.homeIssued {
					// shortcut: a lost first response retries until expiry; revisit if immediate recovery matters.
					// A second guest cookie could overwrite a newer sign-in response.
					w.Header().Set("Refresh", "3; url=/")
					w.WriteHeader(http.StatusConflict)
					return
				}
				signedOutHome = true
			}
		}
		if jw.GetSession(r) == nil && jw.SessionCount() >= maxGuestAdmissionSessions-1 {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		home.ServeHTTP(w, r)
		if signedOutHome && jw.GetSession(r) != nil {
			state := access.rotatedCookies[signedOutKey]
			state.homeIssued = true
			access.rotatedCookies[signedOutKey] = state
		}
	})
	mux.HandleFunc("HEAD /{$}", headPublicPage)
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
	if a.sessionUsername(sess) == "" && len(sess.Requests()) >= maxGuestPageRequests {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Too many public pages", http.StatusTooManyRequests)
		return
	}
	ui.Handler(a.jaws, name, view).ServeHTTP(w, r)
}

// rememberRotatedCookie is called under sessionAdmissionMu before closing the old session.
func (a *adminAccess) rememberRotatedCookie(value string, limit int) bool {
	now := time.Now()
	for key, state := range a.rotatedCookies {
		if state.pending == 0 && !now.Before(state.until) {
			delete(a.rotatedCookies, key)
		}
	}
	key := sha256.Sum256([]byte(value))
	state, known := a.rotatedCookies[key]
	if !known && len(a.rotatedCookies) >= limit {
		return false
	}
	state.until = now.Add(time.Minute)
	a.rotatedCookies[key] = state
	return true
}

// rejectRotatedCookie is called under sessionAdmissionMu before minting a guest cookie.
func (a *adminAccess) rejectRotatedCookie(w http.ResponseWriter, r *http.Request) bool {
	if cookie, err := r.Cookie(a.jaws.CookieName); err == nil {
		key := sha256.Sum256([]byte(cookie.Value))
		state := a.rotatedCookies[key]
		if !state.signedOut && (state.pending > 0 || time.Now().Before(state.until)) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Refresh", "3; url=/")
			http.Error(w, "Sign-in session changed; reload. If this continues, retry in one minute", http.StatusConflict)
			return true
		}
	}
	return false
}

// signedOutCookie is called under sessionAdmissionMu.
func (a *adminAccess) signedOutCookie(r *http.Request) bool {
	if cookie, err := r.Cookie(a.jaws.CookieName); err == nil {
		state := a.rotatedCookies[sha256.Sum256([]byte(cookie.Value))]
		return state.signedOut && time.Now().Before(state.until)
	}
	return false
}

// redirectClosedSession is called under sessionAdmissionMu before guest middleware.
func (a *adminAccess) redirectClosedSession(w http.ResponseWriter, r *http.Request) bool {
	if a.signedOutCookie(r) && a.jaws.GetSession(r) == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusSeeOther)
		return true
	}
	return false
}

// markSignedOutCookie is called under sessionAdmissionMu when sign-out closes a session.
func (a *adminAccess) markSignedOutCookie(value string) {
	if !a.rememberRotatedCookie(value, maxRotatedCookies) {
		slog.Warn("sign-out page reload may not reach home: cookie marker capacity reached")
		return
	}
	key := sha256.Sum256([]byte(value))
	state := a.rotatedCookies[key]
	state.signedOut = true
	a.rotatedCookies[key] = state
}

// rotatedSessionForLogout is called under sessionAdmissionMu after the old cookie stopped resolving.
func (a *adminAccess) rotatedSessionForLogout(r *http.Request) *jaws.Session {
	cookie, err := r.Cookie(a.jaws.CookieName)
	if err != nil {
		return nil
	}
	value := cookie.Value
	var chain []string
	var sess *jaws.Session
	var pendingIP netip.Addr
	pending := false
	for range maxRotatedCookies {
		state, known := a.rotatedCookies[sha256.Sum256([]byte(value))]
		if !known || state.pending == 0 && !time.Now().Before(state.until) {
			break
		}
		chain = append(chain, value)
		if state.replacement == nil {
			pending = state.pending > 0
			pendingIP = state.originIP
			break
		}
		sess = state.replacement
		value = sess.CookieValue()
	}
	if len(chain) == 0 {
		return nil
	}
	if pending {
		// A callback can be between closing its old session and publishing the new one.
		// shortcut: pending logout cannot validate forwarded IPs; revisit if Minion enables trusted proxy headers.
		if a.jaws.TrustForwardedHeaders || !samePeerIP(pendingIP, r.RemoteAddr) {
			return nil
		}
	} else {
		if sess == nil {
			return nil
		}
		probe := r.Clone(r.Context())
		probe.Header.Del("Cookie")
		probe.AddCookie(&http.Cookie{Name: a.jaws.CookieName, Value: sess.CookieValue()})
		if a.jaws.GetSession(probe) != sess {
			return nil
		}
	}
	for _, value := range chain {
		a.markSignedOutCookie(value)
	}
	return sess
}

// samePeerIP matches JaWS peer binding when forwarded headers are not trusted.
func samePeerIP(sessionIP netip.Addr, remoteAddr string) bool {
	if !sessionIP.IsValid() {
		return false
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	peerIP, err := netip.ParseAddr(remoteAddr)
	if err != nil {
		return false
	}
	sessionIP, peerIP = sessionIP.Unmap(), peerIP.Unmap()
	return sessionIP.Compare(peerIP) == 0 || sessionIP.IsLoopback() && peerIP.IsLoopback()
}

func (a *adminAccess) recordOAuthRotation(sess *jaws.Session, r *http.Request) {
	a.sessionAdmissionMu.Lock()
	defer a.sessionAdmissionMu.Unlock()
	for _, cookie := range r.Cookies() {
		if cookie.Name != a.jaws.CookieName {
			continue
		}
		key := sha256.Sum256([]byte(cookie.Value))
		state := a.rotatedCookies[key]
		if state.pending == 0 {
			continue
		}
		state.replacement = sess
		a.rotatedCookies[key] = state
		if state.signedOut {
			sess.Clear()
			sess.Close()
		}
		return
	}
}

type oauthCallbackWriter struct {
	http.ResponseWriter
	admin     *adminAccess
	key       [32]byte
	committed bool
}

func (w *oauthCallbackWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.committed = true
	w.admin.sessionAdmissionMu.Lock()
	defer w.admin.sessionAdmissionMu.Unlock()
	if w.admin.rotatedCookies[w.key].signedOut {
		header := w.Header()
		cookies := header.Values("Set-Cookie")
		header.Del("Set-Cookie")
		for _, line := range cookies {
			name, _, _ := strings.Cut(line, "=")
			if strings.TrimSpace(name) != w.admin.jaws.CookieName {
				header.Add("Set-Cookie", line)
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *oauthCallbackWriter) Write(body []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// serveOAuthCallback holds the old cookie out of guest admission until every callback using it finishes.
func (a *adminAccess) serveOAuthCallback(w http.ResponseWriter, r *http.Request, next http.Handler) {
	closeUnreadAdminBody(w, r)
	a.sessionAdmissionMu.Lock()
	old := a.jaws.GetSession(r)
	if old == nil {
		a.sessionAdmissionMu.Unlock()
		next.ServeHTTP(w, r)
		return
	}
	value := old.CookieValue()
	if !a.rememberRotatedCookie(value, maxOAuthRotations) {
		a.sessionAdmissionMu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	key := sha256.Sum256([]byte(value))
	state := a.rotatedCookies[key]
	state.originIP = old.IP()
	state.pending++
	a.rotatedCookies[key] = state
	a.sessionAdmissionMu.Unlock()
	defer func() {
		a.sessionAdmissionMu.Lock()
		state := a.rotatedCookies[key]
		state.pending--
		if state.pending == 0 {
			if state.replacement == nil && !state.signedOut {
				delete(a.rotatedCookies, key)
			} else {
				state.until = time.Now().Add(time.Minute)
				a.rotatedCookies[key] = state
			}
		} else {
			a.rotatedCookies[key] = state
		}
		a.sessionAdmissionMu.Unlock()
	}()
	callback := &oauthCallbackWriter{ResponseWriter: w, admin: a, key: key}
	next.ServeHTTP(callback, r)
	if !callback.committed {
		callback.WriteHeader(http.StatusOK)
	}
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
	if !a.rememberRotatedCookie(previousCookie, maxSignInRotations) {
		a.sessionAdmissionMu.Unlock()
		http.Error(w, "Sign-in temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	key := sha256.Sum256([]byte(previousCookie))
	previous.Clear()
	previous.Close()
	sess := a.jaws.NewSession(w, r)
	if sess != nil {
		sess.Set(adminSessionKey, username)
		state := a.rotatedCookies[key]
		state.replacement = sess
		a.rotatedCookies[key] = state
	} else if a.rotatedCookies[key].pending == 0 {
		delete(a.rotatedCookies, key)
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
	if sess == nil {
		sess = a.rotatedSessionForLogout(r)
	}
	if sess != nil {
		account := ""
		if a.oauth != nil {
			account, _ = a.oauth.currentUser(sess)
		}
		if a.sessionUsername(sess) != "" || account != "" {
			a.markSignedOutCookie(sess.CookieValue())
		}
		sess.Clear()
		sess.Close()
	}
	a.sessionAdmissionMu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
