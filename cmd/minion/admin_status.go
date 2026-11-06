package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

var adminStatusHTML = template.Must(template.New("admin status").Parse(headHTML + adminNavHTML + `{{template "head.html" $}}<title>Status · Minion admin</title></head>
<body>{{template "admin-nav" $}}<main class="container py-4">
  <div class="border-bottom mb-4"><h1>Status</h1><p class="text-body-secondary">The identity and settings used by this Minion instance.</p></div>
  {{with .Dot}}{{if .Unavailable}}<p role="alert" class="alert alert-warning">Status is unavailable.</p>{{else}}
  <div class="row row-cols-1 row-cols-lg-2 g-3 mb-4">
    <section class="col"><div class="h-100 border rounded p-3"><h2 class="h5">GitHub account</h2>
      <p>Configured bot: <strong>@{{.BotLogin}}</strong> (ID {{.BotID}})</p>
      <form method="get" action="/admin/status/check"><button class="btn btn-primary" type="submit">Test GitHub auth</button></form>
      {{if .Check.Checked}}<p role="status" class="alert {{if .Check.OK}}alert-success{{else}}alert-danger{{end}} mt-3 mb-0">{{.Check.Message}}</p>{{end}}
    </div></section>
    <section class="col"><div class="h-100 border rounded p-3"><h2 class="h5">Configured AI</h2>
      <dl class="mb-0"><dt>Main</dt><dd>{{.Main.Provider}}{{with .Main.Settings.Model}} · {{.}}{{end}}{{with .Main.Settings.Effort}} · {{.}} effort{{end}}</dd>
      <dt>Critic</dt><dd>{{if .Critic.Provider}}{{.Critic.Provider}}{{with .Critic.Settings.Model}} · {{.}}{{end}}{{with .Critic.Settings.Effort}} · {{.}} effort{{end}}{{else}}Disabled{{end}}</dd></dl>
    </div></section>
  </div>
  <div class="row row-cols-1 row-cols-md-3 g-3">
    <div class="col"><a class="d-block h-100 border rounded p-3 text-decoration-none" href="/admin/users"><span class="h5 d-block">Users</span>Named requesters: {{.Requesters}}{{if .OpenRepositories}} · Any writer on {{.OpenRepositories}} {{if eq .OpenRepositories 1}}repository{{else}}repositories{{end}}{{end}}</a></div>
    <div class="col"><a class="d-block h-100 border rounded p-3 text-decoration-none" href="/admin/repositories"><span class="h5 d-block">Repositories</span>Monitored: {{.Repositories}}</a></div>
    <div class="col"><a class="d-block h-100 border rounded p-3 text-decoration-none" href="/admin/skills"><span class="h5 d-block">Skills</span>Configured: {{.Skills}}</a></div>
  </div>{{end}}{{end}}
</main>{{$.TailHTML}}</body></html>`))

type adminStatusRole struct {
	Provider string
	Settings config.RoleModelConfig
}

type adminGitHubCheck struct {
	Checked bool
	OK      bool
	Message string
}

type adminStatusView struct {
	BotLogin                     string
	BotID                        int64
	Main, Critic                 adminStatusRole
	Requesters, OpenRepositories int
	Repositories, Skills         int
	Check                        adminGitHubCheck
	Unavailable                  bool
}

func statusView(owner *action.Orchestrator, cfg config.Config) adminStatusView {
	if owner == nil {
		return adminStatusView{Unavailable: true}
	}
	view := adminStatusView{
		BotLogin: owner.BotLogin, BotID: owner.BotID,
		Main:         adminStatusRole{Provider: cfg.Main, Settings: cfg.RoleModelSettings(false)},
		Critic:       adminStatusRole{Provider: cfg.Adversarial, Settings: cfg.RoleModelSettings(true)},
		Repositories: len(cfg.Repositories),
	}
	requesters := make(map[string]struct{})
	for _, repo := range cfg.Repositories {
		if repo.AllowedActors == nil {
			view.OpenRepositories++
		}
		for _, login := range repo.AllowedActors {
			requesters[strings.ToLower(login)] = struct{}{}
		}
	}
	view.Requesters = len(requesters)
	sources := make(map[string]struct{})
	for _, source := range cfg.AlwaysOnSkills {
		sources[source] = struct{}{}
	}
	for _, command := range cfg.Commands {
		for _, source := range command.Skills {
			sources[source] = struct{}{}
		}
	}
	view.Skills = len(sources)
	return view
}

func checkGitHubAuth(ctx context.Context, owner *action.Orchestrator) adminGitHubCheck {
	result := adminGitHubCheck{Checked: true}
	if owner == nil || owner.GitHub == nil {
		result.Message = "GitHub client is unavailable."
		return result
	}
	user, err := owner.GitHub.AuthenticatedUser(ctx)
	if err != nil {
		var status *github.StatusError
		if errors.As(err, &status) {
			result.Message = fmt.Sprintf("GitHub returned HTTP %d.", status.StatusCode)
		} else {
			result.Message = "GitHub request failed."
		}
		return result
	}
	if user.ID <= 0 || user.ID != owner.BotID || !strings.EqualFold(user.Login, owner.BotLogin) {
		result.Message = "GitHub account does not match the active bot identity."
		return result
	}
	result.OK = true
	result.Message = fmt.Sprintf("Authenticated as @%s (ID %d).", user.Login, user.ID)
	return result
}

func adminStatusHandler(jw *jaws.Jaws, owner *action.Orchestrator, current func() config.Config, check bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		if current == nil {
			http.Error(w, "Status unavailable", http.StatusServiceUnavailable)
			return
		}
		view := statusView(owner, current())
		if check {
			view.Check = checkGitHubAuth(r.Context(), owner)
		}
		ui.Handler(jw, "admin status", view).ServeHTTP(w, r)
	})
}
