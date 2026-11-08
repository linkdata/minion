package main

import (
	"html/template"
	"net/http"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
)

type adminRoundOverride struct {
	Name  string
	ID    int64
	Value int
	Set   bool
}

type adminProviderAuth struct {
	Name, Provider string
	Configured     bool
	MaxBytes       int
}

type adminPolicyView struct {
	Global          int
	Max             int
	Critic          bool
	Requesters      []adminRoundOverride
	Repositories    []adminRoundOverride
	Providers       []adminProviderAuth
	Instructions    string
	Custom          bool
	Pending         bool
	RestartRequired bool
	Error           string
}

func policyView(snapshot adminConfigSnapshot) adminPolicyView {
	cfg := snapshot.Config
	global := 1
	if cfg.CriticStopping.RequiredCleanRounds != nil {
		global = *cfg.CriticStopping.RequiredCleanRounds
	}
	view := adminPolicyView{Global: global, Max: config.MaxCriticMaxRounds,
		Critic: cfg.Adversarial != "", Pending: snapshot.Pending, RestartRequired: snapshot.RestartRequired, Error: snapshot.Error,
		Instructions: cfg.AIInstructions, Custom: cfg.AIInstructions != ""}
	view.Providers = []adminProviderAuth{
		{Name: "Codex", Provider: "codex", Configured: cfg.Codex.APIKey != "", MaxBytes: config.MaxProviderAPIKeyBytes},
		{Name: "Claude", Provider: "claude", Configured: cfg.Claude.APIKey != "", MaxBytes: config.MaxProviderAPIKeyBytes},
	}
	if !view.Custom {
		view.Instructions = action.DefaultAIInstructions
	}
	for _, requester := range cfg.RequesterPolicies {
		if requester.RequiredCleanRounds == nil {
			continue
		}
		row := adminRoundOverride{Name: requester.Login, ID: requester.ID, Value: *requester.RequiredCleanRounds}
		view.Requesters = append(view.Requesters, row)
	}
	for _, repo := range cfg.Repositories {
		row := adminRoundOverride{Name: repo.Name, ID: repo.ID, Set: repo.RequiredCleanRounds != nil}
		if row.Set {
			row.Value = *repo.RequiredCleanRounds
		}
		view.Repositories = append(view.Repositories, row)
	}
	return view
}

var adminPolicyHTML = template.Must(template.New("admin policy").Parse(headHTML + adminNavHTML + `{{template "head.html" $}}<title>AI policy · Minion admin</title></head>
<body>{{template "admin-nav" $}}<main class="container py-4">
  <div class="border-bottom mb-4"><h1>AI policy</h1><p class="text-body-secondary">Clean critic reviews required before a fix branch and reply can be published. Repository overrides take priority over requester defaults, then the global setting.</p></div>
  {{with .Dot}}
  {{if .RestartRequired}}<p role="status" class="alert alert-info">Saved configuration requires a Minion restart. Use <a href="/admin/config">Configuration</a> to stage further edits.</p>{{end}}
  {{if .Pending}}<p role="status" class="alert alert-info">A configuration change is pending activation.</p>{{end}}
  {{if .Error}}<p role="alert" class="alert alert-danger">Latest change failed: {{.Error}}</p>{{end}}
  {{if not .Critic}}<p class="alert alert-warning">No critic is configured. Configure one on the <a href="/admin/config">Configuration</a> page and restart Minion to enable clean-review requirements.</p>{{end}}
  <fieldset class="border-0 p-0 m-0 w-100" {{if .RestartRequired}}disabled{{end}}>
  <section class="border rounded p-3 mb-3"><h2 class="h5">AI authentication</h2><p class="small text-body-secondary">Set a provider API key for new actions. An empty key uses the executor host's saved CLI login or API-key environment variable. Existing keys are never shown here.</p>
    {{range .Providers}}<div class="border-top py-3"><h3 class="h6">{{.Name}}</h3><p>{{if .Configured}}Minion API key configured.{{else}}Using saved CLI login or host API-key environment.{{end}}</p>
      <form method="post" action="/admin/policy/{{.Provider}}/key" class="d-flex flex-wrap align-items-end gap-2">
        <label class="d-grid gap-1">{{.Name}} API key<input class="form-control" type="password" name="api_key" autocomplete="new-password" maxlength="{{.MaxBytes}}" required><small>Up to {{.MaxBytes}} bytes per key.</small></label>
        <button class="btn btn-primary" type="submit">Set {{.Name}} API key</button>
      </form>
      {{if .Configured}}<form method="post" action="/admin/policy/{{.Provider}}/key/clear" class="mt-2"><button class="btn btn-outline-light" type="submit">Use saved CLI login or host API-key environment</button></form>{{end}}
    </div>{{end}}
  </section>
  <section class="border rounded p-3 mb-3"><h2 class="h5">Global default</h2>
    <form method="post" action="/admin/policy/global" class="d-flex flex-wrap align-items-end gap-2">
      <label class="d-grid gap-1">Required clean reviews<input class="form-control" type="number" name="rounds" min="0" max="{{.Max}}" value="{{.Global}}" required></label>
      <button class="btn btn-primary" type="submit">Save default</button>
    </form><p class="small text-body-secondary mt-2 mb-0">Zero disables the additional clean-review gate. Proposal revision rounds keep their separate configured limit.</p>
  </section>
  <section class="border rounded p-3 mb-3"><h2 class="h5">Requester defaults</h2>
    {{if .Requesters}}<div class="table-responsive"><table class="table align-middle"><thead><tr><th scope="col">GitHub requester</th><th scope="col">Required reviews</th><th scope="col">Override</th></tr></thead><tbody>
      {{range .Requesters}}<tr><td>@{{.Name}} <small class="text-body-secondary">(ID {{.ID}})</small></td><td>{{.Value}}</td><td>
        <form method="post" action="/admin/policy/requester/remove"><input type="hidden" name="id" value="{{.ID}}"><button class="btn btn-sm btn-outline-danger" type="submit" aria-label="Use global default for @{{.Name}}">Use global default</button></form>
      </td></tr>{{end}}</tbody></table></div>{{else}}<p>No requester overrides. Requesters follow the global default.</p>{{end}}
    <form method="post" action="/admin/policy/requester" class="d-flex flex-wrap align-items-end gap-2">
      <label class="d-grid gap-1">Repository to verify access<select class="form-select" name="repository">{{range .Repositories}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select></label>
      <label class="d-grid gap-1">GitHub login<input class="form-control" name="login" maxlength="39" required></label>
      <label class="d-grid gap-1">Required reviews<input class="form-control" type="number" name="rounds" min="0" max="{{.Max}}" required></label>
      <button class="btn btn-primary" type="submit" {{if not .Repositories}}disabled{{end}}>Set requester default</button>
    </form>
  </section>
  <section class="border rounded p-3 mb-3"><h2 class="h5">Repository overrides</h2><p class="small text-body-secondary">An empty value follows each requester's default. An override applies to everyone in that repository.</p>
    {{range .Repositories}}<form method="post" action="/admin/policy/repository" class="d-flex flex-wrap align-items-end gap-2 border-top py-3">
      <input type="hidden" name="repository" value="{{.Name}}"><strong class="me-auto">{{.Name}}</strong>
      <label class="d-grid gap-1">Required reviews<input class="form-control" type="number" name="rounds" min="0" max="{{$.Dot.Max}}" {{if .Set}}value="{{.Value}}"{{end}} placeholder="Inherit"></label>
      <button class="btn btn-outline-light" type="submit" aria-label="Save required reviews for {{.Name}}">Save</button>
    </form>{{end}}
  </section>
  <section class="border rounded p-3"><h2 class="h5">Global AI instructions</h2><p class="small text-body-secondary">This text replaces Minion's editable built-in working guidance for new actions. Fixed action limits and output rules still apply.</p>
    <p>{{if .Custom}}Custom instructions are active.{{else}}Using the built-in default.{{end}}</p>
    <form method="post" action="/admin/policy/instructions"><label class="d-grid gap-1">Instructions<textarea class="form-control font-monospace" name="instructions" rows="12" maxlength="16384" required>{{.Instructions}}</textarea><small class="text-body-secondary">Limit: 16,384 UTF-8 bytes. Some characters use more than one byte.</small></label><button class="btn btn-primary mt-3" type="submit">Save instructions</button></form>
    <form method="post" action="/admin/policy/instructions/reset" class="mt-2"><button class="btn btn-outline-light" type="submit">Reset to built-in default</button></form>
  </section></fieldset>{{end}}
</main>{{$.TailHTML}}</body></html>`))

func adminPolicyHandler(jw *jaws.Jaws, snapshot func() adminConfigSnapshot) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		ui.Handler(jw, "admin policy", policyView(snapshot())).ServeHTTP(w, r)
	})
}
