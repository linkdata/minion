package main

import (
	_ "embed"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/linkdata/secureheaders"
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
{{if .Dot.Setup}}{{if .Dot.SetupComplete}}<p role="status">Setup saved. If a credential was rejected, sign in as admin and correct it in Advanced config. {{if .Dot.Repositories}}For each selected AI, set its API key in Advanced config or sign in to its CLI on the executor host. Create the <code>work_root</code> directory (default <code>var/work</code> beside the config file). On the executor host, install skills as its service account with <code>minion -config CONFIG_FILE -install-skills</code> (use a worker skills config for a remote worker), then restart Minion.{{else}}Restart Minion to check invitations from approved owners. Set up the AI worker and <code>work_root</code> before restarting to monitor an accepted repository.{{if not .Dot.CanAddRepository}} Add an available command in Advanced config and include it in the accepted repository's actions before monitoring.{{end}}{{end}} No jobs run during setup or recovery.</p>
{{else}}<p role="status">Setup is incomplete. {{if .Dot.SignedAdmin}}Save the GitHub bot credentials, then add a repository or approve its owner and invite the bot. Add a command in Advanced config if none is available.{{else}}Sign in as admin to finish setup.{{end}} No jobs run during setup.</p>{{end}}
{{if .Dot.SignedAdmin}}
<h2 class="h5">GitHub bot</h2>
<form method="post" action="/admin/setup" class="vstack gap-3 col-lg-6">
<input type="hidden" name="version" value="{{.Dot.Version}}">
<label class="form-label">GitHub bot login<input class="form-control" name="bot_login" value="{{.Dot.BotLogin}}" maxlength="39" required></label>
<label class="form-label">GitHub bot token<input class="form-control" name="github_token" type="password" autocomplete="new-password" maxlength="4096" {{if not .Dot.TokenSet}}required{{end}}></label>
{{if .Dot.TokenSet}}<small>Leave blank to keep the saved token.</small>{{end}}
<button class="btn btn-primary" type="submit">Save bot credentials</button></form>
<h2 class="h5 mt-4">Repository owners allowed to invite Minion</h2>
<p>Approve a GitHub organization or personal account that owns a repository. GitHub decides who may invite collaborators to it. Once bot credentials are saved, invite the bot to a repository owned by an approved account. Repositories added by invitation start monitoring after a restart and initially allow no requesters.</p>
{{if .Dot.InvitationOwners}}<ul class="list-group mb-3 col-lg-6">{{range .Dot.InvitationOwners}}<li class="list-group-item d-flex flex-wrap align-items-center gap-2"><span class="me-auto">{{.Login}} <small class="text-body-secondary">(ID {{.ID}})</small></span><form method="post" action="/admin/invitation-owners/remove"><input type="hidden" name="version" value="{{$.Dot.Version}}"><input type="hidden" name="login" value="{{.Login}}"><button class="btn btn-sm btn-outline-danger" type="submit" aria-label="Remove owner {{.Login}}">Remove</button></form></li>{{end}}</ul>
{{else}}<p>No repository owners are approved for invitations.</p>{{end}}
<form method="post" action="/admin/invitation-owners/add" class="vstack gap-3 col-lg-6">
<input type="hidden" name="version" value="{{.Dot.Version}}">
<label class="form-label">GitHub owner login<input class="form-control" name="login" maxlength="39" required></label>
<label class="form-label">Numeric GitHub owner ID<input class="form-control" type="number" name="id" min="1" required><small class="text-body-secondary">Find it with <code>gh api users/OWNER --jq .id</code>.</small></label>
<button class="btn btn-primary" type="submit">Approve owner</button></form>
{{if and .Dot.BotLogin .Dot.TokenSet .Dot.CanAddRepository}}
<h2 class="h5 mt-4">Add repository</h2>
{{if .Dot.Repositories}}<p>Configured repositories: {{range $i, $name := .Dot.Repositories}}{{if $i}}, {{end}}<code>{{$name}}</code>{{end}}</p>{{end}}
<form method="post" action="/admin/repositories/add" class="vstack gap-3 col-lg-6">
<input type="hidden" name="version" value="{{.Dot.Version}}">
<label class="form-label">GitHub repository URL<input class="form-control" name="url" type="url" placeholder="https://github.com/owner/repo" maxlength="225" required></label>
<p class="text-body-secondary">The repository starts with one available command and no allowed requesters. Add requesters after restarting.</p>
<button class="btn btn-primary" type="submit">Add repository</button></form>
{{end}}
{{end}}{{end}}
</main>{{$.TailHTML}}</body></html>`))

type adminHomeView struct {
	Admin, User, SignedUser, SignedAdmin, Setup, SetupComplete, TokenSet, CanAddRepository bool
	BotLogin, Version                                                                      string
	Repositories                                                                           []string
	InvitationOwners                                                                       []adminGitHubAccount
}
type adminLoginView struct{ SignedIn, PasswordSet bool }

const (
	maxAdminSessions     = 512
	maxGuestPageRequests = 4
	// Spare slots support OAuth callback rotation; existing admin login sessions can still sign in.
	authCompletionReserve     = 32
	maxGuestAdmissionSessions = maxAdminSessions - authCompletionReserve // OAuth starts stop here; home and OAuth login pages stop one slot earlier.
	maxGuestPageSessions      = maxGuestAdmissionSessions - 1
)

type adminIdentity string

func headPublicPage(w http.ResponseWriter, r *http.Request) {
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
	if oauth != nil {
		guestMu = &oauth.admissionMu
	}
	mux := http.NewServeMux()
	if err := jw.Setup(mux.Handle, "/static", jawsboot.Setup); err != nil {
		return nil, err
	}
	loginPage := jw.SessionMiddleware(http.HandlerFunc(access.loginPage))
	mux.HandleFunc("GET /admin/login", func(w http.ResponseWriter, r *http.Request) {
		guestMu.Lock()
		defer guestMu.Unlock()
		if oauth != nil && jw.GetSession(r) == nil && jw.SessionCount() >= maxGuestAdmissionSessions {
			guestAdmissionUnavailable(w)
			return
		}
		loginPage.ServeHTTP(w, r)
	})
	mux.HandleFunc("HEAD /admin/login", headPublicPage)
	mux.HandleFunc("GET /admin/login.js", func(w http.ResponseWriter, r *http.Request) {
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
		mux.Handle("POST /admin/repositories/add", access.setupRepositoryPost())
		mux.Handle("POST /admin/invitation-owners/add", access.setupInvitationOwnerAdd())
		mux.Handle("POST /admin/invitation-owners/remove", access.setupInvitationOwnerRemove())
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
		guestMu.Lock()
		defer guestMu.Unlock()
		if jw.GetSession(r) == nil && jw.SessionCount() >= maxGuestPageSessions {
			if access.passwordSet && oauth == nil {
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
		mux.Handle("POST /admin/status/check", adminStatusHandler(jw, owner, live.current, true))
		for _, page := range []string{"users", "repositories", "skills"} {
			mux.Handle("GET /admin/"+page, adminConfigurationHandler(jw, live, page))
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
	mux.Handle("/", jw)
	crossOrigin := http.NewCrossOriginProtection()
	return jw.SecureHeadersMiddleware(access.browserErrors(adminGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if crossSchemeAdminOrigin(r, jw.TrustForwardedHeaders) || crossOrigin.Check(r) != nil {
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
		http.Error(w, "Invalid stop request", http.StatusBadRequest)
		return
	}
	jobID, attemptID := r.PostForm.Get("job_id"), r.PostForm.Get("attempt_id")
	if len(r.PostForm["job_id"]) != 1 || len(r.PostForm["attempt_id"]) != 1 ||
		!workspace.ValidIdentitySegment(jobID) || !workspace.ValidIdentitySegment(attemptID) {
		http.Error(w, "Invalid stop request", http.StatusBadRequest)
		return
	}
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
func crossSchemeAdminOrigin(r *http.Request, trustForwardedHeaders bool) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	scheme := "http"
	if secureheaders.RequestIsSecure(r, trustForwardedHeaders) {
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
		return r.Method == http.MethodPost && (r.URL.Path == "/admin/setup" || r.URL.Path == "/admin/repositories/add" ||
			r.URL.Path == "/admin/invitation-owners/add" || r.URL.Path == "/admin/invitation-owners/remove") ||
			r.URL.Path == "/admin/config" && (r.Method == http.MethodGet || r.Method == http.MethodPost) ||
			r.URL.Path == "/admin/config/check" && r.Method == http.MethodPost
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
	sess := a.jaws.GetSession(r)
	account, _ := a.oauth.currentUser(sess)
	if a.setup && a.sessionUsername(sess) == "" && account == "" && len(sess.Requests()) >= maxGuestPageRequests {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	view := adminHomeView{Admin: a.passwordSet, User: a.oauth != nil, SignedUser: account != "", SignedAdmin: a.sessionUsername(sess) != "", Setup: a.setup}
	if a.setup && a.setupPath != "" {
		if cfg, version, err := loadSetupConfig(a.setupPath, a.setupAdmin); err == nil {
			view.BotLogin = cfg.BotLogin
			view.TokenSet = cfg.GitHub.Token != ""
			view.SetupComplete = cfg.BotLogin != "" && view.TokenSet &&
				(len(cfg.Repositories) == 0 && len(cfg.InvitationOwners) > 0 ||
					len(cfg.Repositories) != 0 && hasUnselectedCommand(nil, cfg, true))
			_, addErr := setupRepositoryActions(cfg)
			view.CanAddRepository = addErr == nil
			view.Version = version
			for _, repo := range cfg.Repositories {
				view.Repositories = append(view.Repositories, repo.Name)
			}
			for _, login := range cfg.InvitationOwners {
				view.InvitationOwners = append(view.InvitationOwners, adminGitHubAccount{Login: login, ID: cfg.InvitationOwnerIDs[login]})
			}
		}
	}
	a.publicPage(w, r, "home", view)
}

func loadSetupConfig(path string, runtime config.AdminConfig) (config.Config, string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return config.Config{}, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return config.Config{}, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAdvancedConfig+1))
	if err != nil {
		return config.Config{}, "", err
	}
	if len(data) > maxAdvancedConfig {
		return config.Config{}, "", errors.New("configuration exceeds the editor limit")
	}
	cfg, err := decodeAdvancedConfig(data)
	if err != nil {
		return cfg, "", err
	}
	applyAdvancedRuntime(&cfg, runtime)
	version, err := advancedVersion(cfg)
	if err != nil {
		return cfg, "", err
	}
	if err := cfg.Validate(filepath.Dir(path)); err != nil {
		return cfg, "", err
	}
	return cfg, version, nil
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
		http.Error(w, "Invalid login request", http.StatusBadRequest)
		return
	}
	password := r.PostForm.Get("password")
	if len(r.PostForm) != 1 || len(r.PostForm["password"]) != 1 || !config.ValidAdminPassword(password) {
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
