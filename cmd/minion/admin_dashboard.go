package main

import (
	_ "embed"
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

//go:embed admin_dashboard.css
var adminDashboardCSS string

var adminDashboardDirty = new(int)

const adminDashboardHTML = `{{define "dashboard"}}<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Minion admin</title>{{$.HeadHTML}}
</head>
<body>
  <header class="topbar">
    <div><strong>MINION</strong><span> / operator</span></div>
    <form method="post" action="/admin/logout"><button type="submit">Sign out</button></form>
  </header>
  <main>
    <div class="heading"><div>
      <p class="eyebrow">ORCHESTRATOR</p><h1>Live actions</h1>
      <p>Worker and action state is checked every two seconds while this page is connected.</p>
    </div></div>
    <form class="filters" method="get" action="/">
      <label>Repository<input name="repo" value="{{.Dot.Filter.Repo}}" maxlength="201" placeholder="owner/repo"></label>
      <label>Requester<input name="requester" value="{{.Dot.Filter.Requester}}" maxlength="105" placeholder="login"></label>
      <label>Action<select name="action">
        <option value=""{{if eq .Dot.Filter.Action ""}} selected{{end}}>All actions</option>
        <option value="review"{{if eq .Dot.Filter.Action "review"}} selected{{end}}>Review</option>
        <option value="fix"{{if eq .Dot.Filter.Action "fix"}} selected{{end}}>Fix</option>
        <option value="comment"{{if eq .Dot.Filter.Action "comment"}} selected{{end}}>Comment</option>
        <option value="security-audit"{{if eq .Dot.Filter.Action "security-audit"}} selected{{end}}>Security audit</option>
      </select></label>
      <button type="submit">Apply filters</button>
    </form>
    {{$.Template "p" "dashboard-status" .Dot "role=status aria-live=polite class=visually-hidden"}}
    {{$.Template "section" "dashboard-live" .Dot}}
    {{$.Template "section" "dashboard-action" .Dot "class=\"panel actions\""}}
  </main>{{$.TailHTML}}
</body>
</html>{{end}}
{{define "dashboard-status"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}{{if .Unavailable}}Live state unavailable.{{else}}Worker readiness {{.Snapshot.Worker.Readiness}}. Reported state {{.Snapshot.Worker.State}}. Worker phase {{if .Snapshot.Worker.Phase}}{{.Snapshot.Worker.Phase}}{{if .Snapshot.Worker.Round}} / round {{.Snapshot.Worker.Round}}{{end}}{{if .Snapshot.Worker.ReviewPhase}} / {{.Snapshot.Worker.ReviewPhase}}{{end}}{{else}}not observed{{end}}. In progress {{.Snapshot.InProgress}}.{{if .Action}} Current {{.Action.Action}} action in {{.Action.Repository}} for {{.Action.Requester}}, accepted {{utc .Action.AcceptedAt}}, phase {{.Action.Phase}}.{{end}}{{end}}{{end}}{{else}}Admin session ended.{{end}}{{end}}
{{define "dashboard-live"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}
{{if .Unavailable}}<p role="alert">Live state is unavailable. Check the orchestrator state file.</p>{{else}}
<div class="metrics">
  <article><h2>Tracked accepted actions</h2><p class="metric">{{.Snapshot.TrackedAccepted}}</p>
    <small>Counted since this dashboard counter was introduced.</small></article>
  <article><h2>In progress</h2><p class="metric">{{.Snapshot.InProgress}}</p>
    <small>Includes preparation through publication and terminal cleanup.</small></article>
</div>
<div class="panels">
  <section class="panel"><h2>Selected worker</h2><dl>
    <dt>Location</dt><dd>{{if .Snapshot.Worker.Kind}}{{.Snapshot.Worker.Kind}}{{else}}Not attached{{end}}</dd>
    <dt>Readiness</dt><dd>{{.Snapshot.Worker.Readiness}}</dd>
    <dt>Reported state</dt><dd>{{.Snapshot.Worker.State}}</dd>
    <dt>Phase</dt><dd>{{if .Snapshot.Worker.Phase}}{{.Snapshot.Worker.Phase}}{{if .Snapshot.Worker.Round}} / round {{.Snapshot.Worker.Round}}{{end}}{{if .Snapshot.Worker.ReviewPhase}} / {{.Snapshot.Worker.ReviewPhase}}{{end}}{{else}}Not observed{{end}}</dd>
    <dt>Last worker status received</dt><dd>{{if .Snapshot.Worker.LastContact.IsZero}}Not observed{{else}}{{utc .Snapshot.Worker.LastContact}}{{end}}</dd>
    <dt>Worker-reported progress age at last status</dt><dd>{{if .Snapshot.Worker.LastContact.IsZero}}Not observed{{else}}{{.Snapshot.Worker.ReportedProgressAge}}{{end}}</dd>
  </dl></section>
  <section class="panel"><h2>Configured limits</h2><dl>
    <dt>Hard action runtime</dt><dd>{{.Snapshot.MaxRuntime}}</dd>
    <dt>Executor inactivity</dt><dd>{{.Snapshot.InactivityTimeout}}</dd>
  </dl><p class="hint">A worker status records contact; it does not reset the inactivity limit or prove readiness.</p></section>
</div>
{{end}}{{end}}{{else}}<p role="alert">Admin session ended. Sign in again to see live state.</p>{{end}}{{end}}
{{define "dashboard-action"}}{{if .Auth.IsAdmin}}{{with .Dot.View}}
{{if .Unavailable}}<p>Live state is unavailable.</p>{{else}}
<h2>Current action</h2>
  {{if .Action}}<table><thead><tr>
    <th scope="col">Repository</th><th scope="col">Requester</th><th scope="col">Action</th>
    <th scope="col">Phase</th><th scope="col">Accepted</th><th scope="col">Original deadline</th>
  </tr></thead><tbody><tr>
    <td>{{.Action.Repository}}</td><td>{{.Action.Requester}}</td><td>{{.Action.Action}}</td>
    <td>{{.Action.Phase}}</td><td>{{utc .Action.AcceptedAt}}</td><td>{{utc .Action.Deadline}}</td>
  </tr></tbody></table>
  {{else if .Snapshot.InProgress}}<p>No current action matches these filters.</p>
  {{else}}<p>No action is in progress.</p>{{end}}
{{end}}{{end}}{{else}}<p role="alert">Admin session ended. Sign in again to see live state.</p>{{end}}{{end}}`

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
	}).Parse(adminDashboardHTML)
}

func registerAdminDashboard(jw *jaws.Jaws) error {
	pages, err := parseAdminDashboard()
	if err != nil {
		return fmt.Errorf("parse admin dashboard: %w", err)
	}
	if err := jw.AddTemplateLookuper(pages); err != nil {
		return fmt.Errorf("register admin dashboard: %w", err)
	}
	if err := jw.GenerateHeadHTML("/admin/dashboard.css"); err != nil {
		return fmt.Errorf("register admin stylesheet: %w", err)
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

func serveAdminDashboardCSS(w http.ResponseWriter, r *http.Request) {
	closeUnreadAdminBody(w, r)
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(adminDashboardCSS))
}
