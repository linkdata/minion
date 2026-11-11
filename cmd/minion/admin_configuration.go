package main

import (
	"html/template"
	"net/http"
	"slices"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
)

type adminConfigSnapshot struct {
	Config          config.Config
	Pending         bool
	RestartRequired bool
	Error           string
	Remote          bool
	RemoteCommands  map[string]bool
}

type adminGitHubAccount struct {
	Login string
	ID    int64
}

type adminRequesterRepository struct {
	Name      string
	AnyWriter bool
	Users     []adminGitHubAccount
}

type adminRepositoryRow struct {
	Name, Policy string
	ID, OwnerID  int64
	Actions      []string
	CanRemove    bool
}

type adminSkillRow struct {
	Name, URL, Target string
	CanRemove         bool
	Remote            bool
}

type adminCommandSkills struct {
	Name   string
	Skills []adminSkillRow
}

type adminConfigurationView struct {
	Pending, Remote, RestartRequired bool
	Error                            string
	Users                            []adminRequesterRepository
	InvitationOwners                 []adminGitHubAccount
	Repositories                     []adminRepositoryRow
	AwaitingRestart                  []string
	Commands                         []string
	AllCommands                      []string
	AlwaysOnSkills                   []adminSkillRow
	CommandSkills                    []adminCommandSkills
}

func configurationView(snapshot adminConfigSnapshot) adminConfigurationView {
	cfg := snapshot.Config
	view := adminConfigurationView{Pending: snapshot.Pending, Error: snapshot.Error, Remote: snapshot.Remote, RestartRequired: snapshot.RestartRequired}
	view.AllCommands = cfg.CommandNames()
	for _, name := range view.AllCommands {
		if cfg.Commands[name].SupportsProvider(cfg.Main) && cfg.Commands[name].SupportedOnHost() && (!snapshot.Remote || len(cfg.Repositories) == 0 || snapshot.RemoteCommands[name]) {
			view.Commands = append(view.Commands, name)
		}
	}
	for _, repo := range cfg.Repositories {
		users := adminRequesterRepository{Name: repo.Name, AnyWriter: !repo.DenyAllActors && len(repo.AllowedActors) == 0}
		for _, login := range repo.AllowedActors {
			users.Users = append(users.Users, adminGitHubAccount{Login: login, ID: repo.AllowedActorIDs[login]})
		}
		view.Users = append(view.Users, users)
		policy := "Named requesters"
		if users.AnyWriter {
			policy = "Any writer"
		} else if len(users.Users) == 0 {
			policy = "Nobody"
		}
		view.Repositories = append(view.Repositories, adminRepositoryRow{
			Name: repo.Name, ID: repo.ID, OwnerID: repo.OwnerID, Actions: repo.Actions,
			Policy: policy, CanRemove: len(cfg.Repositories) > 1,
		})
	}
	for _, login := range cfg.InvitationOwners {
		view.InvitationOwners = append(view.InvitationOwners, adminGitHubAccount{Login: login, ID: cfg.InvitationOwnerIDs[login]})
	}
	for _, source := range cfg.AlwaysOnSkills {
		view.AlwaysOnSkills = append(view.AlwaysOnSkills, adminSkill(source, "always", true, snapshot.Remote))
	}
	for _, name := range view.AllCommands {
		mapping := cfg.Commands[name]
		group := adminCommandSkills{Name: name}
		for _, source := range mapping.Skills {
			row := adminSkill(source, "command:"+name, len(mapping.Skills) > 1, snapshot.Remote)
			if mapping.Action == "security-audit" && row.Name == "security-audit" {
				row.CanRemove = false
			}
			group.Skills = append(group.Skills, row)
		}
		view.CommandSkills = append(view.CommandSkills, group)
	}
	return view
}

func adminSkill(source, target string, removable, remote bool) adminSkillRow {
	name, err := skills.Name(source)
	if err != nil {
		name = source
	}
	return adminSkillRow{Name: name, URL: source, Target: target, CanRemove: removable, Remote: remote}
}

var adminConfigurationHTML = template.Must(template.New("admin config").Funcs(template.FuncMap{
	"hasAction": func(actions []string, name string) bool { return slices.Contains(actions, name) },
}).Parse(headHTML + adminNavHTML + `
{{define "admin-config-notice"}}
  {{if .RestartRequired}}<p role="status" class="alert alert-info">Saved configuration requires a Minion restart. Use <a href="/admin/config">Configuration</a> to stage further edits.</p>{{end}}
  {{if .Pending}}<p role="status" class="alert alert-info">A configuration change is pending activation.</p>{{end}}
  {{if .Error}}<p role="alert" class="alert alert-danger">Latest change failed: {{.Error}}</p>{{end}}
{{end}}
{{define "admin users"}}{{template "head.html" $}}<title>Users · Minion admin</title></head>
<body>{{template "admin-nav" $}}<main class="container py-4">
  <div class="border-bottom mb-4"><h1>Users</h1><p class="text-body-secondary">Choose which GitHub requesters may ask Minion to act in each repository. Every requester also needs write, maintain, or admin permission on GitHub.</p></div>
  {{with .Dot}}{{template "admin-config-notice" .}}<fieldset class="border-0 p-0 m-0 w-100" {{if .RestartRequired}}disabled{{end}}>
  {{range .Users}}{{$repoName := .Name}}<section class="border rounded p-3 mb-3"><h2 class="h5">{{.Name}}</h2>
    {{if .AnyWriter}}<p><strong>Any writer</strong> may request actions. Adding a named requester narrows access to the named list.</p>
    {{else if .Users}}<p>Only these named requesters may request actions:</p>
      <ul class="list-group mb-3">{{range .Users}}<li class="list-group-item d-flex flex-wrap align-items-center gap-2"><span class="me-auto">@{{.Login}} <small class="text-body-secondary">(ID {{.ID}})</small></span>
        <form method="post" action="/admin/users/remove"><input type="hidden" name="repository" value="{{$repoName}}"><input type="hidden" name="login" value="{{.Login}}"><button class="btn btn-sm btn-outline-danger" type="submit" aria-label="Remove @{{.Login}} from {{$repoName}}">Remove</button></form></li>{{end}}</ul>
    {{else}}<p><strong>Nobody</strong> may request actions. Add a requester or allow any writer.</p>{{end}}
    <form class="d-flex flex-wrap align-items-end gap-2" method="post" action="/admin/users/add">
      <input type="hidden" name="repository" value="{{.Name}}"><label class="d-grid gap-1">GitHub login<input class="form-control" name="login" maxlength="39" required></label>
      <button class="btn btn-primary" type="submit" aria-label="Add requester to {{.Name}}">Add requester</button>
    </form>
    {{if .AnyWriter}}<form class="mt-3" method="post" action="/admin/users/deny-all"><input type="hidden" name="repository" value="{{.Name}}"><button class="btn btn-outline-warning" type="submit" aria-label="Allow nobody in {{.Name}}">Allow nobody</button></form>
    {{else}}<form class="mt-3" method="post" action="/admin/users/allow-any"><input type="hidden" name="repository" value="{{.Name}}"><button class="btn btn-outline-warning" type="submit" aria-label="Allow any writer in {{.Name}}">Allow any writer</button><small class="d-block text-body-secondary">Any GitHub account with write, maintain, or admin permission will be able to request actions.</small></form>{{end}}
    {{if eq (len .Users) 1}}<small class="d-block text-body-secondary mt-2">Removing the last named requester allows nobody.</small>{{end}}
  </section>{{else}}{{if .RestartRequired}}<p>No repositories are active. Restart Minion to apply saved configuration.</p>{{else}}<p>No repositories are configured.</p>{{end}}{{end}}</fieldset>{{end}}
</main>{{$.TailHTML}}</body></html>{{end}}
{{define "admin repositories"}}{{template "head.html" $}}<title>Repositories · Minion admin</title></head>
<body>{{template "admin-nav" $}}<main class="container py-4">
  <div class="border-bottom mb-4"><h1>Repositories</h1><p class="text-body-secondary">Monitored GitHub repositories and the commands each accepts.</p></div>
  {{with .Dot}}{{template "admin-config-notice" .}}{{if and .Remote .Repositories}}<p class="text-body-secondary">Only commands selected by the remote worker can be changed here. To add another command, edit the Minion configuration file and restart Minion and the remote worker.</p>{{end}}<fieldset class="border-0 p-0 m-0 w-100" {{if .RestartRequired}}disabled{{end}}>
  <section class="border rounded p-3 mb-3"><h2 class="h5">Repository owners allowed to invite Minion</h2>
    <p class="text-body-secondary small">Approve a GitHub organization or personal account. Minion accepts invitations to repositories owned by approved accounts; GitHub decides who may send them. Repositories added by invitation start monitoring after a restart and initially allow no requesters.</p>
    {{if .InvitationOwners}}<ul class="list-group mb-3">{{range .InvitationOwners}}<li class="list-group-item d-flex flex-wrap align-items-center gap-2"><span class="me-auto">{{.Login}} <small class="text-body-secondary">(ID {{.ID}})</small></span><form method="post" action="/admin/invitation-owners/remove"><input type="hidden" name="login" value="{{.Login}}"><button class="btn btn-sm btn-outline-danger" type="submit" aria-label="Remove owner {{.Login}}">Remove</button></form></li>{{end}}</ul>
    {{else}}<p>No repository owners are approved for invitations.</p>{{end}}
    <form class="d-flex flex-wrap align-items-end gap-2" method="post" action="/admin/invitation-owners/add">
      <label class="d-grid gap-1">GitHub owner login<input class="form-control" name="login" maxlength="39" required></label>
      <label class="d-grid gap-1">Numeric GitHub owner ID<input class="form-control" type="number" name="id" min="1" required><small class="text-body-secondary">Find it with <code>gh api users/OWNER --jq .id</code>.</small></label>
      <button class="btn btn-primary" type="submit">Approve owner</button>
    </form>
  </section>
  {{range .Repositories}}<section class="border rounded p-3 mb-3"><div class="d-flex flex-wrap align-items-start gap-2"><div class="me-auto"><h2 class="h5 mb-1">{{.Name}}</h2><p class="text-body-secondary small mb-2">Repository ID {{.ID}} · owner ID {{.OwnerID}} · requesters: {{.Policy}}</p></div>
    {{if .CanRemove}}<form method="post" action="/admin/repositories/remove"><input type="hidden" name="name" value="{{.Name}}"><button class="btn btn-sm btn-outline-danger" type="submit" aria-label="Remove repository {{.Name}}">Remove repository</button></form>{{end}}</div>
    <p>Commands: {{range $i, $name := .Actions}}{{if $i}}, {{end}}<code>{{$name}}</code>{{else}}None{{end}}</p>
    <details><summary aria-label="Edit commands for {{.Name}}">Edit commands</summary><form method="post" action="/admin/repositories/actions" class="mt-3">
      <input type="hidden" name="name" value="{{.Name}}"><fieldset><legend class="fs-6">Allowed commands</legend>{{$selected := .Actions}}{{range $.Dot.AllCommands}}<label class="form-check"><input class="form-check-input" type="checkbox" name="actions" value="{{.}}" {{if hasAction $selected .}}checked{{else if not (hasAction $.Dot.Commands .)}}disabled{{end}}><span class="form-check-label">{{.}}{{if not (hasAction $.Dot.Commands .)}} {{if $.Dot.Remote}}(unavailable on this host or not selected by the remote worker){{else}}(unavailable with the current main AI or on this host){{end}}{{end}}</span></label>{{end}}</fieldset>
      <button class="btn btn-outline-light mt-2" type="submit" aria-label="Save commands for {{.Name}}">Save commands</button>
    </form></details>
    {{if not .CanRemove}}<small class="text-body-secondary">To remove the last repository, edit Configuration and restart Minion.</small>{{end}}
  </section>{{else}}{{if .RestartRequired}}<p>No repositories are active. Restart Minion to apply saved configuration.</p>{{else}}<p>No repositories are configured. {{if .InvitationOwners}}Minion is waiting for invitations from approved owners.{{end}}</p>{{end}}{{end}}
  <section class="border rounded p-3"><h2 class="h5">Add repository</h2><p class="text-body-secondary small">Minion will verify its GitHub identity and repository access before saving. The new repository starts with no allowed requesters; add requesters on the Users page. If Minion started with no repositories, restart it after adding the first one to start monitoring.</p>
    <form method="post" action="/admin/repositories/add" class="vstack gap-3">
      <label class="d-grid gap-1">GitHub repository URL <input class="form-control" type="url" name="url" placeholder="https://github.com/owner/repo" maxlength="225" required></label>
      <fieldset><legend class="fs-6">Allowed commands</legend>{{range .Commands}}<label class="form-check"><input class="form-check-input" type="checkbox" name="actions" value="{{.}}"><span class="form-check-label">{{.}}</span></label>{{else}}<p>No commands are configured.</p>{{end}}</fieldset>
      <div><button class="btn btn-primary" type="submit" {{if not .Commands}}disabled{{end}}>Add repository</button></div>
    </form>
  </section></fieldset>
  {{if .AwaitingRestart}}<section class="border rounded p-3 mt-3" role="status"><h2 class="h5">Saved for restart</h2><p>These repositories start monitoring after Minion restarts. Invitation checks pause until then in owner-only mode.</p><ul class="mb-0">{{range .AwaitingRestart}}<li>{{.}}</li>{{end}}</ul></section>{{end}}{{end}}
</main>{{$.TailHTML}}</body></html>{{end}}
{{define "admin skills"}}{{template "head.html" $}}<title>Skills · Minion admin</title></head>
<body>{{template "admin-nav" $}}<main class="container py-4">
  <div class="border-bottom mb-4"><h1>Skills</h1><p class="text-body-secondary">Sources used for every action or for a selected command.</p></div>
  {{with .Dot}}{{template "admin-config-notice" .}}{{if .Remote}}<p class="text-body-secondary">Skill edits require restarting the remote worker. Installed skill status is checked on that worker.</p>{{end}}<fieldset class="border-0 p-0 m-0 w-100" {{if .RestartRequired}}disabled{{end}}>
  <p class="text-body-secondary small">To replace a required skill, stop Minion, update its configuration and any remote worker skill config, run <code>-install-skills</code> on the worker and, for remote security audits, on Minion's host too, then restart.</p>
  <section class="border rounded p-3 mb-3"><h2 class="h5">Every action</h2>{{template "admin-skill-list" .AlwaysOnSkills}}</section>
  {{range .CommandSkills}}<section class="border rounded p-3 mb-3"><h2 class="h5">{{.Name}} command</h2>{{template "admin-skill-list" .Skills}}</section>{{else}}<p>No commands are configured.</p>{{end}}
  {{if not .Remote}}<section class="border rounded p-3"><h2 class="h5">Add skill</h2><form method="post" action="/admin/skills/add" class="vstack gap-3">
    <label class="d-grid gap-1">Use for <select class="form-select" name="target"><option value="always">Every action</option>{{range .Commands}}<option value="command:{{.}}">{{.}} command</option>{{end}}</select></label>
    <label class="d-grid gap-1">GitHub skill URL <input class="form-control" type="url" name="url" placeholder="https://github.com/owner/repo/tree/main/skills/name" maxlength="4096" required></label>
    <div><button class="btn btn-primary" type="submit">Add skill</button></div>
  </form></section>{{end}}</fieldset>{{end}}
</main>{{$.TailHTML}}</body></html>{{end}}
{{define "admin-skill-list"}}{{if .}}<ul class="list-group">{{range .}}<li class="list-group-item d-flex flex-wrap align-items-center gap-2"><div class="me-auto"><strong>{{.Name}}</strong><small class="d-block text-body-secondary text-break">{{.URL}}</small></div>
  {{if not .Remote}}{{if .CanRemove}}<form method="post" action="/admin/skills/remove"><input type="hidden" name="target" value="{{.Target}}"><input type="hidden" name="url" value="{{.URL}}"><button class="btn btn-sm btn-outline-danger" type="submit" aria-label="Remove {{.Name}} skill from {{.Target}}">Remove</button></form>{{else}}<small class="text-body-secondary">Required</small>{{end}}{{end}}
</li>{{end}}</ul>{{else}}<p class="mb-0 text-body-secondary">No skills configured.</p>{{end}}{{end}}
`))

func adminConfigurationHandler(jw *jaws.Jaws, live *adminLiveConfig, page string) http.Handler {
	name := "admin " + page
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		snapshot := live.snapshot()
		view := configurationView(snapshot)
		if page == "repositories" {
			var runtime config.AdminConfig
			if snapshot.Config.Admin != nil {
				runtime = *snapshot.Config.Admin
			}
			saved, err := config.LoadWithAdminRuntime(live.path, runtime)
			if err != nil {
				http.Error(w, "Configuration could not be loaded; check the file and reload", http.StatusConflict)
				return
			}
			for _, repo := range saved.Repositories {
				if _, active := snapshot.Config.Repository(repo.Name); !active {
					view.AwaitingRestart = append(view.AwaitingRestart, repo.Name)
				}
			}
		}
		ui.Handler(jw, name, view).ServeHTTP(w, r)
	})
}
