package main

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
)

var adminDashboardDirty = new(int)

const adminDashboardHTML = `{{define "dashboard"}}<!doctype html>
<html lang="en" data-bs-theme="dark">
<head>
  {{template "head.html" $}}<title>Minion admin</title>
</head>
<body>
  <header class="d-flex flex-wrap align-items-center gap-3 px-4 py-3 border-bottom bg-body-tertiary">
    <div><strong>MINION</strong><span class="text-body-secondary"> / operator</span></div>
    <nav><a href="/admin/usage">Usage</a></nav>
    <form class="ms-auto" method="post" action="/admin/logout"><button class="btn btn-outline-light" type="submit">Sign out</button></form>
  </header>
  <main class="container-fluid py-4">
    <div class="border-bottom mb-4">
      <p class="text-body-secondary small fw-semibold">ORCHESTRATOR</p><h1>Live actions</h1>
      <p class="text-body-secondary">Worker and action state is checked every two seconds while this page is connected.</p>
    </div>
    <form class="d-flex flex-wrap align-items-end gap-3 mb-4" method="get" action="/">
      <label class="d-grid gap-1">Repository<input class="form-control" name="repo" value="{{.Dot.Filter.Repo}}" maxlength="201" placeholder="owner/repo"></label>
      <label class="d-grid gap-1">Requester<input class="form-control" name="requester" value="{{.Dot.Filter.Requester}}" maxlength="105" placeholder="login"></label>
      <label class="d-grid gap-1">Action<select class="form-select" name="action">
        <option value=""{{if eq .Dot.Filter.Action ""}} selected{{end}}>All actions</option>
        <option value="review"{{if eq .Dot.Filter.Action "review"}} selected{{end}}>Review</option>
        <option value="fix"{{if eq .Dot.Filter.Action "fix"}} selected{{end}}>Fix</option>
        <option value="comment"{{if eq .Dot.Filter.Action "comment"}} selected{{end}}>Comment</option>
        <option value="security-audit"{{if eq .Dot.Filter.Action "security-audit"}} selected{{end}}>Security audit</option>
      </select></label>
      <button class="btn btn-primary" type="submit">Apply filters</button>
    </form>
    {{$.Template "p" "dashboard-status" .Dot "role=status aria-live=polite class=visually-hidden"}}
    {{$.Template "p" "dashboard-stop-status" .Dot "role=status aria-live=polite class=visually-hidden"}}
    {{$.Template "section" "dashboard-live" .Dot}}
    {{$.Template "section" "dashboard-action" .Dot "class=\"border rounded p-3 mb-3 overflow-auto\" tabindex=\"0\" aria-label=\"Current action\""}}
    {{$.Template "div" "dashboard-stop-control" .Dot}}
  </main>{{$.TailHTML}}
</body>
</html>{{end}}
{{define "dashboard-status"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}{{if .Unavailable}}Live state unavailable.{{else}}Worker readiness {{.Snapshot.Worker.Readiness}}. Reported state {{.Snapshot.Worker.State}}. Worker phase {{if .Snapshot.Worker.Phase}}{{.Snapshot.Worker.Phase}}{{if .Snapshot.Worker.Round}} / round {{.Snapshot.Worker.Round}}{{end}}{{if .Snapshot.Worker.ReviewPhase}} / {{.Snapshot.Worker.ReviewPhase}}{{end}}{{else}}not observed{{end}}. Tracked accepted actions {{.Snapshot.TrackedAccepted}}. In progress {{.Snapshot.InProgress}}.{{if .Action}} Current {{.Action.Action}} action in {{.Action.Repository}} for {{.Action.Requester}}, accepted {{utc .Action.AcceptedAt}}, phase {{.Action.Phase}}.{{end}}{{end}}{{end}}{{else}}Admin session ended.{{end}}{{end}}
{{define "dashboard-stop-status"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}{{if .Snapshot.LastStop}}Latest stop request for attempt {{.Snapshot.LastStop.AttemptID}}: {{.Snapshot.LastStop.Status}}.{{end}}{{end}}{{end}}{{end}}
{{define "dashboard-live"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}
{{if .Unavailable}}<p role="alert" class="alert alert-danger">Live state is unavailable. Check the orchestrator state file.</p>{{else}}
<div class="row row-cols-1 row-cols-md-2 g-3 mb-3">
  <div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">Tracked accepted actions</h2><p class="display-6 font-monospace">{{.Snapshot.TrackedAccepted}}</p></article></div>
  <div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">In progress</h2><p class="display-6 font-monospace">{{.Snapshot.InProgress}}</p>
    <small class="text-body-secondary">Includes preparation through publication and terminal cleanup.</small></article></div>
</div>
<div class="row row-cols-1 row-cols-xl-2 g-3 mb-3">
  <div class="col"><section class="h-100 border rounded p-3"><h2 class="h5">Selected worker</h2><dl>
    <dt>Location</dt><dd>{{if .Snapshot.Worker.Kind}}{{.Snapshot.Worker.Kind}}{{else}}Not attached{{end}}</dd>
    <dt>Readiness</dt><dd>{{.Snapshot.Worker.Readiness}}</dd>
    <dt>Reported state</dt><dd>{{.Snapshot.Worker.State}}</dd>
    <dt>Phase</dt><dd>{{if .Snapshot.Worker.Phase}}{{.Snapshot.Worker.Phase}}{{if .Snapshot.Worker.Round}} / round {{.Snapshot.Worker.Round}}{{end}}{{if .Snapshot.Worker.ReviewPhase}} / {{.Snapshot.Worker.ReviewPhase}}{{end}}{{else}}Not observed{{end}}</dd>
    <dt>Last worker status received</dt><dd>{{if .Snapshot.Worker.LastContact.IsZero}}Not observed{{else}}{{utc .Snapshot.Worker.LastContact}}{{end}}</dd>
    <dt>Worker-reported progress age at last status</dt><dd>{{if .Snapshot.Worker.LastContact.IsZero}}Not observed{{else}}{{.Snapshot.Worker.ReportedProgressAge}}{{end}}</dd>
  </dl></section></div>
  <div class="col"><section class="h-100 border rounded p-3"><h2 class="h5">Configured limits</h2><dl>
    <dt>Hard action runtime</dt><dd>{{.Snapshot.MaxRuntime}}</dd>
    <dt>Executor inactivity</dt><dd>{{.Snapshot.InactivityTimeout}}</dd>
  </dl><p class="text-body-secondary small">A worker status records contact; it does not reset the inactivity limit or prove readiness.</p></section></div>
</div>
{{if .Snapshot.LastStop}}<section class="border rounded p-3 mb-3"><h2 class="h5">Latest stop request</h2>
  <p><strong>{{.Snapshot.LastStop.Status}}</strong> · {{.Snapshot.LastStop.JobID}} / attempt {{.Snapshot.LastStop.AttemptID}}</p>
  <p class="text-body-secondary small">Requested by {{.Snapshot.LastStop.RequestedBy}} at {{utc .Snapshot.LastStop.RequestedAt}}.
  {{if eq .Snapshot.LastStop.Status "requested"}}Waiting for the stop to be verified.{{else if eq .Snapshot.LastStop.Status "confirmed"}}The attempt stopped cleanly.{{else}}The stop could not be verified; inspect worker readiness before reuse.{{end}}</p>
</section>{{end}}
{{end}}{{end}}{{else}}<p role="alert" class="alert alert-warning">Admin session ended. Sign in again to see live state.</p>{{end}}{{end}}
{{define "dashboard-action"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}
{{if .Unavailable}}<p>Live state is unavailable.</p>{{else}}
<h2 class="h5">Current action</h2>
  {{if .Action}}<table class="table table-striped align-middle"><thead><tr>
    <th scope="col">Repository</th><th scope="col">Requester</th><th scope="col">Action</th>
    <th scope="col">Phase</th><th scope="col">Accepted</th><th scope="col">Original deadline</th>
  </tr></thead><tbody><tr>
    <td>{{.Action.Repository}}</td><td>{{.Action.Requester}}</td><td>{{.Action.Action}}</td>
    <td>{{.Action.Phase}}</td><td>{{utc .Action.AcceptedAt}}</td><td>{{utc .Action.Deadline}}</td>
  </tr></tbody></table><p class="text-body-secondary small"><a href="/admin/actions/{{.Action.AttemptID}}">Open attempt {{.Action.AttemptID}} details</a></p>
  {{else if .Snapshot.InProgress}}<p>No current action matches these filters.</p>
  {{else}}<p>No action is in progress.</p>{{end}}
{{end}}{{end}}{{else}}<p role="alert" class="alert alert-warning">Admin session ended. Sign in again to see live state.</p>{{end}}{{end}}
{{define "dashboard-stop-control"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}{{if .Action}}{{if .Action.Stoppable}}<form class="border rounded p-3 mt-3" method="post" action="/admin/stop"><input type="hidden" name="job_id" value="{{.Action.JobID}}"><input type="hidden" name="attempt_id" value="{{.Action.AttemptID}}"><button class="btn btn-danger" type="submit">Force stop this attempt</button></form>{{end}}{{end}}{{end}}{{end}}{{end}}`

type adminDashboardFilter struct{ Repo, Requester, Action string }

func (f adminDashboardFilter) matches(current *action.DashboardAction) bool {
	return current != nil &&
		(f.Repo == "" || strings.Contains(strings.ToLower(current.Repository), strings.ToLower(f.Repo))) &&
		(f.Requester == "" || strings.Contains(strings.ToLower(current.Requester), strings.ToLower(f.Requester))) &&
		(f.Action == "" || current.Action == f.Action)
}

func parseAdminDashboardFilter(rawQuery string) (adminDashboardFilter, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return adminDashboardFilter{}, err
	}
	for _, field := range []struct {
		name  string
		limit int
	}{{"repo", 201}, {"requester", 105}, {"action", 100}} {
		value := values.Get(field.name)
		if len(values[field.name]) > 1 || len(value) > field.limit || !utf8.ValidString(value) ||
			strings.ContainsAny(value, "\x00\r\n") {
			return adminDashboardFilter{}, fmt.Errorf("invalid %s filter", field.name)
		}
	}
	filter := adminDashboardFilter{Repo: strings.TrimSpace(values.Get("repo")), Requester: strings.TrimSpace(values.Get("requester")), Action: values.Get("action")}
	switch filter.Action {
	case "", "review", "fix", "comment", "security-audit":
		return filter, nil
	default:
		return adminDashboardFilter{}, errors.New("invalid action filter")
	}
}

type adminDashboardPage struct {
	owner  *action.Orchestrator
	Filter adminDashboardFilter
}

func (*adminDashboardPage) JawsGetTag() any { return adminDashboardDirty }

type adminDashboardView struct {
	Snapshot    action.DashboardSnapshot
	Action      *action.DashboardAction
	Unavailable bool
}

func (page *adminDashboardPage) View() adminDashboardView {
	now := time.Now().UTC()
	snapshot, err := page.owner.DashboardSnapshot(now)
	if err != nil {
		return adminDashboardView{Unavailable: true}
	}
	view := adminDashboardView{Snapshot: snapshot}
	if page.Filter.matches(snapshot.Action) {
		view.Action = snapshot.Action
	}
	return view
}

func parseAdminDashboard() (*template.Template, error) {
	return template.New("dashboard").Funcs(template.FuncMap{
		"utc": func(at time.Time) string { return at.UTC().Format(time.RFC3339) },
	}).Parse(headHTML + adminDashboardHTML)
}

func registerAdminDashboard(jw *jaws.Jaws) error {
	pages, err := parseAdminDashboard()
	if err != nil {
		return fmt.Errorf("parse admin dashboard: %w", err)
	}
	if err := jw.AddTemplateLookuper(pages); err != nil {
		return fmt.Errorf("register admin dashboard: %w", err)
	}
	return nil
}

func adminDashboardHandler(jw *jaws.Jaws, owner *action.Orchestrator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		filter, err := parseAdminDashboardFilter(r.URL.RawQuery)
		if err != nil {
			http.Error(w, "Invalid dashboard filter", http.StatusBadRequest)
			return
		}
		ui.Handler(jw, "dashboard", &adminDashboardPage{owner: owner, Filter: filter}).ServeHTTP(w, r)
	})
}
