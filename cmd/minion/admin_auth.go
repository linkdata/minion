package main

import (
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

var adminLoginHTML = template.Must(template.New("admin login").Parse(headHTML + `{{template "head.html" $}}<title>Minion admin</title></head>
<body><main class="container py-5">
{{if .Dot.SignedIn}}
<h1 class="h3 mb-3">Signed in</h1><p><a class="btn btn-primary" href="/">Continue to Minion</a></p>
<form method="post" action="/admin/logout"><button class="btn btn-outline-light">Sign out</button></form>
{{else if .Dot.PasswordSet}}
<h1 class="h3 mb-3">Minion admin</h1>
<form method="post" action="/admin/login" class="vstack gap-3 col-md-6 col-lg-4">
<label class="form-label">Password <input class="form-control" name="password" type="password" autocomplete="current-password" required></label>
<button class="btn btn-primary">Sign in</button></form><script defer src="/admin/login.js"></script>
{{else}}
<h1 class="h3 mb-3">Minion admin</h1>
<p role="status">The admin password is not set. Run <code>minion -mode=hash</code>, set the <code>admin</code> account's <code>password_hash</code> to the generated hash, then restart Minion.</p>
{{end}}
</main>{{$.TailHTML}}</body></html>`))

//go:embed admin_login.js
var adminLoginJS []byte

var adminHomeHTML = template.Must(template.New("home").Parse(headHTML + `{{template "head.html" $}}<title>Minion</title></head>
<body><main class="container py-5"><h1>Minion</h1>
{{if .Dot.SignedAdmin}}<div class="d-flex gap-2 mb-3"><a class="btn btn-outline-light" href="/admin/config">Advanced config</a><form method="post" action="/admin/logout"><button class="btn btn-outline-light">Admin sign out</button></form></div>
{{else if .Dot.Admin}}<a class="btn btn-primary me-2" href="/admin/login">Admin sign in</a>{{end}}
{{if .Dot.User}}<a class="btn btn-primary" href="/auth/login">{{if .Dot.SignedUser}}Your account{{else}}User sign in{{end}}</a>{{end}}
{{if not .Dot.Admin}}<p role="status">The admin password is not set. Run <code>minion -mode=hash</code>, set the <code>admin</code> account's <code>password_hash</code> to the generated hash, then restart Minion.</p>{{end}}
{{if .Dot.Setup}}{{if .Dot.SetupComplete}}<p role="status">Setup saved. If a credential was rejected, sign in as admin and correct it in Advanced config. For each selected AI, set its API key in Advanced config or sign in to its CLI on the executor host. Create the <code>work_root</code> directory (default <code>var/work</code> beside the config file), install skills with <code>minion -config CONFIG_FILE -install-skills</code> using this service's config file, then restart Minion. No jobs run during setup or recovery.</p>
{{else}}<p role="status">Setup is incomplete. {{if .Dot.SignedAdmin}}Save the bot account and first repository below.{{else}}Sign in as admin to save the bot account and first repository.{{end}} No jobs run during setup.</p>
{{if .Dot.SignedAdmin}}
<form method="post" action="/admin/setup" class="vstack gap-3 col-lg-6">
<label class="form-label">GitHub bot login<input class="form-control" name="bot_login" value="{{.Dot.BotLogin}}" maxlength="39" required></label>
<label class="form-label">GitHub bot token<input class="form-control" name="github_token" type="password" autocomplete="new-password" maxlength="4096" required></label>
<label class="form-label">First repository (owner/name)<input class="form-control" name="repository" value="{{.Dot.Repository}}" maxlength="201" required></label>
<label class="form-label">Repository ID<input class="form-control" name="repository_id" type="number" min="1" step="1" required></label>
<label class="form-label">Owner ID<input class="form-control" name="owner_id" type="number" min="1" step="1" required></label>
<p class="text-body-secondary">The first repository starts with one configured command, if any, and no allowed requesters. Add requesters in the admin UI after restarting.</p>
<button class="btn btn-primary" type="submit">Save setup</button></form>
{{end}}{{end}}{{end}}
</main>{{$.TailHTML}}</body></html>`))

type adminHomeView struct {
	Admin, User, SignedUser, SignedAdmin, Setup, SetupComplete bool
	BotLogin, Repository                                       string
}
type adminLoginView struct{ SignedIn, PasswordSet bool }

const (
	maxAdminSessions          = 512
	maxGuestPageRequests      = 4
	adminSignInReserve        = 32
	maxGuestAdmissionSessions = maxAdminSessions - adminSignInReserve // OAuth starts stop here; home and OAuth login pages stop one slot earlier.
	maxGuestPageSessions      = maxGuestAdmissionSessions - 1
)

type adminIdentity string

func headPublicPage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

func guestAdmissionUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (adminIdentity) Data() map[string]any { return nil }
func (adminIdentity) Email() string        { return "" }
func (id adminIdentity) IsAdmin() bool     { return id != "" }

type loginWindow struct {
	start time.Time
	count int
}

type adminAccess struct {
	jaws         *jaws.Jaws
	oauth        *oauthAccess
	actionAccess *userActionAccess
	setup        bool
	setupPath    string
	setupAdmin   config.AdminConfig
	account      config.AdminAccount
	passwordSet  bool
	mu           sync.Mutex
	noticeMu     sync.Mutex
	attempts     loginWindow
	hashSlots    chan struct{}
}

func newAdminHandler(jw *jaws.Jaws, cfg config.AdminConfig, owner *action.Orchestrator, oauth *oauthAccess, liveConfigs ...*adminLiveConfig) (http.Handler, error) {
	return newAdminHandlerWithSetupPath(jw, cfg, owner, oauth, false, "", liveConfigs...)
}

func newAdminHandlerWithSetupPath(jw *jaws.Jaws, cfg config.AdminConfig, owner *action.Orchestrator, oauth *oauthAccess, setup bool, setupPath string, liveConfigs ...*adminLiveConfig) (http.Handler, error) {
	jw.MaxSessions = maxAdminSessions
	if err := jw.AddTemplateLookuper(adminDashboardTemplate); err != nil {
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
	if err := jw.AddTemplateLookuper(adminStatusHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminConfigurationHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminPolicyHTML); err != nil {
		return nil, err
	}
	if err := jw.AddTemplateLookuper(adminAdvancedHTML); err != nil {
		return nil, err
	}
	access := &adminAccess{jaws: jw, oauth: oauth, setup: setup, setupPath: setupPath, setupAdmin: cfg, hashSlots: make(chan struct{}, 2)}
	if len(cfg.Administrators) == 1 {
		account := cfg.Administrators[0]
		if account.Username == "admin" && account.PasswordHash != "" && account.PasswordHash != config.AdminPasswordHashPlaceholder {
			access.account, access.passwordSet = account, true
		}
	}
	jw.MakeAuth = func(rq *jaws.Request) jaws.Auth { return adminIdentity(access.sessionUsername(rq.Session())) }
	guestMu := new(sync.Mutex)
	guestLoginLimit := maxGuestPageSessions
	if access.passwordSet {
		guestLoginLimit = maxAdminSessions
	}
	if oauth != nil {
		guestMu = &oauth.admissionMu
	}
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
		guestMu.Lock()
		defer guestMu.Unlock()
		if jw.GetSession(r) == nil && jw.SessionCount() >= guestLoginLimit {
			guestAdmissionUnavailable(w)
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
	if setup && setupPath != "" {
		mux.Handle("POST /admin/setup", access.setupPost())
		registerAdminAdvancedRoutes(mux, jw, setupPath, nil, cfg)
	}
	mux.HandleFunc("POST /admin/stop", func(w http.ResponseWriter, r *http.Request) {
		access.stop(w, r, owner)
	})
	dashboard := adminDashboardHandler(jw, owner)
	home := jw.SessionMiddleware(http.HandlerFunc(access.homePage))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		sess := jw.GetSession(r)
		if !setup && access.sessionUsername(sess) != "" {
			dashboard.ServeHTTP(w, r)
			return
		}
		closeUnreadAdminBody(w, r)
		guestMu.Lock()
		defer guestMu.Unlock()
		if jw.GetSession(r) == nil && jw.SessionCount() >= maxGuestPageSessions {
			if access.passwordSet {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			guestAdmissionUnavailable(w)
			return
		}
		home.ServeHTTP(w, r)
	})
	mux.HandleFunc("HEAD /{$}", headPublicPage)
	mux.Handle("GET /admin/actions/{attempt}", adminActionDetailHandler(jw, owner))
	mux.Handle("GET /admin/usage", adminUsageHandler(jw, owner, false))
	mux.Handle("GET /admin/usage.json", adminUsageHandler(jw, owner, true))
	if len(liveConfigs) != 0 && liveConfigs[0] != nil {
		live := liveConfigs[0]
		mux.Handle("GET /admin/status", adminStatusHandler(jw, owner, live.current, false))
		mux.Handle("GET /admin/status/check", adminStatusHandler(jw, owner, live.current, true))
		for _, page := range []string{"users", "repositories", "skills"} {
			mux.Handle("GET /admin/"+page, adminConfigurationHandler(jw, live.snapshot, page))
		}
		mux.Handle("GET /admin/policy", adminPolicyHandler(jw, live.snapshot))
		registerAdminConfigActions(mux, live, owner)
		registerAdminPolicyActions(mux, live, owner)
		registerAdminAdvancedRoutes(mux, jw, live.path, live, cfg)
	}
	if oauth != nil && owner != nil {
		access.actionAccess = &userActionAccess{oauth: oauth, owner: owner}
		if err := oauth.actionRoutes(access.actionAccess); err != nil {
			return nil, err
		}
	}
	if oauth != nil {
		oauth.mux.HandleFunc("POST /auth/logout", access.logout)
		mux.Handle("/auth/", oauth.mux)
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
		return access.allowed(r) || oauth.allowed(r)
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
	if username == "admin" && a.passwordSet {
		return username
	}
	return ""
}

func (a *adminAccess) allowed(r *http.Request) bool {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/" {
		return true
	}
	if r.URL.Path == "/admin/login" && (r.Method == http.MethodGet || r.Method == http.MethodHead ||
		a.passwordSet && r.Method == http.MethodPost) {
		return true
	}
	if a.passwordSet && r.URL.Path == "/admin/logout" && r.Method == http.MethodPost {
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
	if a.setup {
		if a.setupPath == "" || a.sessionUsername(a.jaws.GetSession(r)) == "" {
			return false
		}
		return r.Method == http.MethodPost && r.URL.Path == "/admin/setup" ||
			r.URL.Path == "/admin/config" && (r.Method == http.MethodGet || r.Method == http.MethodPost)
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
	a.publicPage(w, r, "admin login", adminLoginView{SignedIn: signedIn, PasswordSet: a.passwordSet})
}

func (a *adminAccess) homePage(w http.ResponseWriter, r *http.Request) {
	account, _ := a.oauth.currentUser(a.jaws.GetSession(r))
	view := adminHomeView{Admin: a.passwordSet, User: a.oauth != nil, SignedUser: account != "", SignedAdmin: a.sessionUsername(a.jaws.GetSession(r)) != "", Setup: a.setup}
	if a.setup && a.setupPath != "" {
		if cfg, err := config.LoadWithAdminRuntime(a.setupPath, a.setupAdmin); err == nil {
			view.BotLogin = cfg.BotLogin
			view.SetupComplete = cfg.BotLogin != "" && cfg.GitHub.Token != "" && len(cfg.Repositories) != 0
			if len(cfg.Repositories) != 0 {
				view.Repository = cfg.Repositories[0].Name
			}
		}
	}
	a.publicPage(w, r, "home", view)
}

func (a *adminAccess) publicPage(w http.ResponseWriter, r *http.Request, name string, view any) {
	sess := a.jaws.GetSession(r)
	account, _ := a.oauth.currentUser(sess)
	if a.sessionUsername(sess) == "" && account == "" && len(sess.Requests()) >= maxGuestPageRequests {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	ui.Handler(a.jaws, name, view).ServeHTTP(w, r)
}

func (a *adminAccess) reserveAttempt() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	window := a.attempts
	now := time.Now()
	if window.start.IsZero() || now.Sub(window.start) >= time.Minute {
		window = loginWindow{start: now}
	}
	if window.count >= 5 {
		return false
	}
	window.count++
	a.attempts = window
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
	password := r.PostForm.Get("password")
	if len(r.PostForm) != 1 || len(r.PostForm["password"]) != 1 || !config.ValidAdminPassword(password) {
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
	if !a.reserveAttempt() {
		http.Error(w, "Too many login attempts", http.StatusTooManyRequests)
		return
	}
	if !a.account.VerifyPassword(password) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}
	if a.oauth != nil {
		a.oauth.linkMu.Lock()
		defer a.oauth.linkMu.Unlock()
	}
	previous := a.jaws.GetSession(r)
	if previous == nil {
		http.Error(w, "Sign-in session unavailable; load the login page and try again", http.StatusConflict)
		return
	}
	previous.Clear()
	previous.Close()
	sess := a.jaws.NewSession(w, r)
	if sess == nil {
		http.Error(w, "Session unavailable", http.StatusServiceUnavailable)
		return
	}
	sess.Set(adminSessionKey, "admin")
	a.mu.Lock()
	a.attempts = loginWindow{}
	a.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *adminAccess) logout(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	if a.oauth != nil {
		a.oauth.linkMu.Lock()
	}
	sess := a.jaws.GetSession(r)
	sess.Close()
	if a.oauth != nil {
		a.oauth.linkMu.Unlock()
		for _, provider := range a.oauth.providers {
			provider.server.Logout(sess, r)
		}
	}
	sess.Clear()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
