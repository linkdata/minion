package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/config"
)

const (
	advancedSecretMarker   = "PASTE_NEW_SECRET_OR_KEEP_THIS_VALUE"
	advancedPasswordMarker = "ADMIN_PASSWORD_CANNOT_BE_CHANGED_HERE"
	maxAdvancedConfig      = 16 << 20
)

type adminAdvancedView struct {
	JSON, Version     string
	Setup             bool
	RestartRequired   bool
	PendingActivation bool
}

var adminAdvancedHTML = template.Must(template.New("admin advanced").Parse(headHTML + adminNavHTML + `{{template "head.html" $}}<title>Configuration · Minion admin</title></head>
<body>{{if .Dot.Setup}}<header class="d-flex align-items-center gap-3 px-4 py-3 border-bottom"><strong>MINION / setup</strong><a href="/">Home</a><form class="ms-auto" method="post" action="/admin/logout"><button class="btn btn-outline-light" type="submit">Sign out</button></form></header>{{else}}{{template "admin-nav" $}}{{end}}<main class="container py-4">
<h1>Configuration</h1><p>Use this editor for settings without a dedicated control. Changes to service settings need a restart; AI API key changes apply to new jobs after any active action ends.</p>
{{if .Dot.RestartRequired}}<p role="status" class="alert alert-info">Saved configuration requires a Minion restart.</p>{{end}}
{{if .Dot.PendingActivation}}<p role="status" class="alert alert-info">AI key change is pending activation.</p>{{end}}
<p>Existing secrets appear as <code>PASTE_NEW_SECRET_OR_KEEP_THIS_VALUE</code>. Leave that value to keep a secret, replace it to set a new one, or use an empty string to clear it. The admin password hash cannot be changed here.</p>
<form method="post" action="/admin/config">
<input type="hidden" name="version" value="{{.Dot.Version}}">
<label class="d-grid gap-1" for="config-json">Configuration JSON</label>
<textarea class="form-control font-monospace" id="config-json" name="config" rows="30" spellcheck="false" required>{{.Dot.JSON}}</textarea>
<button class="btn btn-primary mt-3" type="submit">Save configuration</button>
</form></main>{{$.TailHTML}}</body></html>`))

func registerAdminAdvancedRoutes(mux *http.ServeMux, jw *jaws.Jaws, path string, live *adminLiveConfig, runtime config.AdminConfig) {
	mux.Handle("GET /admin/config", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		view, err := advancedConfigView(path, live, runtime)
		if err != nil {
			http.Error(w, "Configuration could not be loaded; check the file and reload", http.StatusConflict)
			return
		}
		ui.Handler(jw, "admin advanced", view).ServeHTTP(w, r)
	}))
	mux.Handle("POST /admin/config", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 4*maxAdvancedConfig)
		parseErr := r.ParseForm()
		closeUnreadAdminBody(w, r)
		if parseErr != nil || len(r.PostForm) != 2 || len(r.PostForm["version"]) != 1 || len(r.PostForm["config"]) != 1 {
			http.Error(w, "Invalid configuration form", http.StatusBadRequest)
			return
		}
		if err := saveAdvancedConfig(path, live, runtime, r.PostForm.Get("version"), r.PostForm.Get("config")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/admin/config", http.StatusSeeOther)
	}))
}

func advancedConfigView(path string, live *adminLiveConfig, runtime config.AdminConfig) (adminAdvancedView, error) {
	var view adminAdvancedView
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxAdvancedConfig {
		return view, errors.New("configuration is unavailable or exceeds the editor limit")
	}
	raw, err := decodeAdvancedConfig(data)
	if err != nil {
		return view, err
	}
	verified, err := cloneConfig(raw)
	if err != nil {
		return view, err
	}
	applyAdvancedRuntime(&verified, runtime)
	if err := verified.Validate(filepath.Dir(path)); err != nil {
		return view, err
	}
	view.Version, err = advancedVersion(verified)
	if err != nil {
		return view, err
	}
	maskAdvancedSecrets(&raw)
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return view, err
	}
	view.JSON = string(data)
	view.Setup = live == nil
	if live != nil {
		live.mu.RLock()
		view.RestartRequired, view.PendingActivation = live.restartRequired, live.pending
		live.mu.RUnlock()
	}
	return view, nil
}

func decodeAdvancedConfig(data []byte) (config.Config, error) {
	var cfg config.Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return cfg, errors.New("configuration has trailing data")
	}
	return cfg, nil
}

func advancedVersion(cfg config.Config) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func maskAdvancedSecrets(cfg *config.Config) {
	if cfg.GitHub.Token != "" {
		cfg.GitHub.Token = advancedSecretMarker
	}
	if cfg.Codex.APIKey != "" {
		cfg.Codex.APIKey = advancedSecretMarker
	}
	if cfg.Claude.APIKey != "" {
		cfg.Claude.APIKey = advancedSecretMarker
	}
	if cfg.RemoteExecutor != nil && cfg.RemoteExecutor.Token != "" {
		cfg.RemoteExecutor.Token = advancedSecretMarker
	}
	if cfg.Admin != nil {
		for i := range cfg.Admin.Administrators {
			cfg.Admin.Administrators[i].PasswordHash = advancedPasswordMarker
		}
		if cfg.Admin.GitHubOAuth != nil && cfg.Admin.GitHubOAuth.ClientSecret != "" {
			cfg.Admin.GitHubOAuth.ClientSecret = advancedSecretMarker
		}
		if cfg.Admin.GitLabOAuth != nil && cfg.Admin.GitLabOAuth.ClientSecret != "" {
			cfg.Admin.GitLabOAuth.ClientSecret = advancedSecretMarker
		}
	}
}

func applyAdvancedRuntime(cfg *config.Config, runtime config.AdminConfig) {
	if cfg.Admin == nil {
		return
	}
	cfg.Admin.WebservAddress = runtime.WebservAddress
	cfg.Admin.WebservCertDir = runtime.WebservCertDir
	cfg.Admin.WebservUser = runtime.WebservUser
	cfg.Admin.WebservDataDir = runtime.WebservDataDir
	cfg.Admin.WebservListenURL = runtime.WebservListenURL
}

func restoreAdvancedSecrets(after, before *config.Config) error {
	if before.Admin == nil || after.Admin == nil || len(before.Admin.Administrators) != 1 || len(after.Admin.Administrators) != 1 ||
		after.Admin.Administrators[0].Username != before.Admin.Administrators[0].Username ||
		after.Admin.Administrators[0].PasswordHash != advancedPasswordMarker {
		return errors.New("admin password and account cannot be changed here")
	}
	after.Admin.Administrators[0].PasswordHash = before.Admin.Administrators[0].PasswordHash
	keep := func(value *string, original string) {
		if *value == advancedSecretMarker {
			*value = original
		}
	}
	keep(&after.GitHub.Token, before.GitHub.Token)
	keep(&after.Codex.APIKey, before.Codex.APIKey)
	keep(&after.Claude.APIKey, before.Claude.APIKey)
	if after.RemoteExecutor != nil && before.RemoteExecutor != nil {
		keep(&after.RemoteExecutor.Token, before.RemoteExecutor.Token)
	} else if after.RemoteExecutor != nil && after.RemoteExecutor.Token == advancedSecretMarker {
		return errors.New("a new remote worker token is required")
	}
	if after.Admin.GitHubOAuth != nil && before.Admin.GitHubOAuth != nil {
		keep(&after.Admin.GitHubOAuth.ClientSecret, before.Admin.GitHubOAuth.ClientSecret)
	} else if after.Admin.GitHubOAuth != nil && after.Admin.GitHubOAuth.ClientSecret == advancedSecretMarker {
		return errors.New("a new GitHub OAuth secret is required")
	}
	if after.Admin.GitLabOAuth != nil && before.Admin.GitLabOAuth != nil {
		keep(&after.Admin.GitLabOAuth.ClientSecret, before.Admin.GitLabOAuth.ClientSecret)
	} else if after.Admin.GitLabOAuth != nil && after.Admin.GitLabOAuth.ClientSecret == advancedSecretMarker {
		return errors.New("a new GitLab OAuth secret is required")
	}
	return nil
}

func saveAdvancedConfig(path string, live *adminLiveConfig, runtime config.AdminConfig, version, source string) error {
	if len(source) > maxAdvancedConfig || len(version) != sha256.Size*2 {
		return errors.New("configuration form is too large or stale")
	}
	edited, err := decodeAdvancedConfig([]byte(source))
	if err != nil {
		return errors.New("configuration JSON is invalid")
	}
	if live != nil {
		live.mu.Lock()
		defer live.mu.Unlock()
	}
	updated, err := config.UpdateWithAdminRuntime(path, runtime, func(raw *config.Config) error {
		before, err := cloneConfig(*raw)
		if err != nil {
			return err
		}
		if err := before.Validate(filepath.Dir(path)); err != nil {
			return err
		}
		currentVersion, err := advancedVersion(before)
		if err != nil {
			return err
		}
		if currentVersion != version {
			return errors.New("configuration changed; reload before saving")
		}
		if err := restoreAdvancedSecrets(&edited, raw); err != nil {
			return err
		}
		applyAdvancedRuntime(&edited, runtime)
		*raw = edited
		return nil
	})
	if err != nil {
		return fmt.Errorf("configuration could not be saved: %w", err)
	}
	if live != nil {
		baseline := live.disk
		baseline.Codex.APIKey, baseline.Claude.APIKey = updated.Codex.APIKey, updated.Claude.APIKey
		beforeJSON, _ := json.Marshal(baseline)
		updatedJSON, _ := json.Marshal(updated)
		if bytes.Equal(beforeJSON, updatedJSON) {
			live.disk = updated
			live.restartRequired = false
		} else {
			live.restartRequired = true
		}
		if live.desired.Codex.APIKey != updated.Codex.APIKey || live.desired.Claude.APIKey != updated.Claude.APIKey {
			live.desired.Codex.APIKey = updated.Codex.APIKey
			live.desired.Claude.APIKey = updated.Claude.APIKey
			live.generation++
			live.pending = true
			live.lastError = ""
			select {
			case live.updates <- struct{}{}:
			default:
			}
		}
	}
	return nil
}
