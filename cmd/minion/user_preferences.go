package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"

	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/config"
)

var userPreferencesHTML = template.Must(template.New("user preferences").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Model preferences</title>{{$.HeadHTML}}</head>
<body><main><h1>Model preferences</h1>
{{range .Dot.Notices}}<p role="status">{{.}}</p>{{end}}
{{if .Dot.Fields}}
<p>Choose administrator-approved settings for future actions.</p>
<form method="post" action="/auth/preferences">
<input type="hidden" name="identity" value="{{.Dot.Identity}}">
{{range .Dot.Fields}}
<label>{{.Label}} model and effort <select name="{{.Name}}">
<option value="default"{{if .DefaultSelected}} selected{{end}}>Service default</option>
{{range .Options}}<option value="{{.Value}}"{{if .Selected}} selected{{end}}>{{.Model}} · {{.Effort}}</option>{{end}}
</select></label>
{{end}}
<button type="submit">Save preferences</button>
</form>
{{else}}<p>No model choices are currently offered.</p>{{end}}
<p><a href="/auth/login">Account</a></p>
</main>{{$.TailHTML}}</body></html>`))

type userPreferenceOption struct {
	Value, Model, Effort string
	Selected             bool
}

type userPreferenceField struct {
	Name, Label     string
	DefaultSelected bool
	Options         []userPreferenceOption
}

type userPreferencesView struct {
	Identity string
	Fields   []userPreferenceField
	Notices  []string
}

func preferenceChoiceValue(choice config.RoleModelConfig) string {
	return "pair:" + base64.RawURLEncoding.EncodeToString([]byte(choice.Model+"\x00"+choice.Effort))
}

func preferenceValue(raw string, choices []config.RoleModelConfig) (config.RoleModelConfig, bool) {
	if raw == "default" {
		return config.RoleModelConfig{}, true
	}
	for _, choice := range choices {
		if raw == preferenceChoiceValue(choice) {
			return choice, true
		}
	}
	return config.RoleModelConfig{}, false
}

func preferenceFormIdentity(account string) string {
	hash := sha256.Sum256([]byte(account))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func preferenceField(name, label string, choices []config.RoleModelConfig, saved config.RoleModelConfig) userPreferenceField {
	field := userPreferenceField{Name: name, Label: label, DefaultSelected: !slices.Contains(choices, saved)}
	for _, choice := range choices {
		field.Options = append(field.Options, userPreferenceOption{
			Value: preferenceChoiceValue(choice), Model: choice.Model, Effort: choice.Effort, Selected: choice == saved,
		})
	}
	return field
}

func (a *oauthAccess) preferencePage(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Cache-Control", "no-store")
	account, _ := a.currentUser(a.jaws.GetSession(r))
	if account == "" {
		if a.links.storageError() != nil {
			http.Error(w, "Preferences unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	saved, err := a.links.preferencesFor(account)
	if err != nil {
		http.Error(w, "Preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	view := userPreferencesView{Identity: preferenceFormIdentity(account)}
	for _, role := range []struct {
		name, label string
		choices     []config.RoleModelConfig
		saved       config.RoleModelConfig
	}{
		{"main", "Main", a.choices.Main, saved.Main},
		{"critic", "Critic", a.choices.Critic, saved.Critic},
	} {
		if role.saved != (config.RoleModelConfig{}) && !slices.Contains(role.choices, role.saved) {
			view.Notices = append(view.Notices, "Saved "+role.name+" choice is unavailable; future actions use the service default.")
		}
		if len(role.choices) != 0 || role.saved != (config.RoleModelConfig{}) {
			view.Fields = append(view.Fields, preferenceField(role.name, role.label, role.choices, role.saved))
		}
	}
	ui.Handler(a.jaws, "user preferences", view).ServeHTTP(w, r)
}

func (a *oauthAccess) savePreferences(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil || len(r.PostForm["identity"]) != 1 {
		closeUnreadAdminBody(w, r)
		http.Error(w, "Invalid preferences", http.StatusBadRequest)
		return
	}
	closeUnreadAdminBody(w, r)
	a.linkMu.Lock()
	defer a.linkMu.Unlock()
	account, _ := a.currentUser(a.jaws.GetSession(r))
	if account == "" {
		if a.links.storageError() != nil {
			http.Error(w, "Preferences unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "Sign in before saving preferences", http.StatusForbidden)
		return
	}
	if r.PostForm.Get("identity") != preferenceFormIdentity(account) {
		http.Error(w, "Sign-in changed; reopen preferences", http.StatusConflict)
		return
	}
	saved, err := a.links.preferencesFor(account)
	if err != nil {
		http.Error(w, "Preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	mainField := len(a.choices.Main) != 0 || saved.Main != (config.RoleModelConfig{})
	criticField := len(a.choices.Critic) != 0 || saved.Critic != (config.RoleModelConfig{})
	if !mainField && !criticField {
		http.Error(w, "No model choices are currently offered", http.StatusConflict)
		return
	}
	wantFields := 1
	if mainField {
		wantFields++
	}
	if criticField {
		wantFields++
	}
	if len(r.PostForm) != wantFields || mainField && len(r.PostForm["main"]) != 1 ||
		criticField && len(r.PostForm["critic"]) != 1 {
		http.Error(w, "Invalid preferences", http.StatusBadRequest)
		return
	}
	main, critic := saved.Main, saved.Critic
	if mainField {
		var ok bool
		main, ok = preferenceValue(r.PostForm.Get("main"), a.choices.Main)
		if !ok {
			http.Error(w, "Unapproved preferences", http.StatusBadRequest)
			return
		}
	}
	if criticField {
		var ok bool
		critic, ok = preferenceValue(r.PostForm.Get("critic"), a.choices.Critic)
		if !ok {
			http.Error(w, "Unapproved preferences", http.StatusBadRequest)
			return
		}
	}
	if err := a.links.setPreferences(account, config.UserModelPreferences{Main: main, Critic: critic}); err != nil {
		var limited preferenceRateLimitError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.Itoa(limited.retryAfterSeconds))
			http.Error(w, "Please retry preference update", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "Preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/auth/preferences", http.StatusSeeOther)
}
