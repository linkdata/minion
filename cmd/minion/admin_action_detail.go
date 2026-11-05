package main

import (
	"context"
	"html/template"
	"net/http"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/workspace"
)

var adminActionDetailTemplate = template.Must(template.New("admin action detail").Funcs(template.FuncMap{
	"utc": func(at time.Time) string { return at.UTC().Format(time.RFC3339) },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Minion action detail</title>{{$.HeadHTML}}</head><body>{{if or .Auth.IsAdmin .Dot.AllowUser}}{{with .Dot}}
{{if .AllowUser}}<header class="topbar"><div><strong>MINION</strong><span> / my actions</span></div><form method="post" action="/auth/logout"><button type="submit">Sign out</button></form></header>
<main class="detail"><p><a href="/auth/actions">← My actions</a></p>{{else}}<header class="topbar"><div><strong>MINION</strong><span> / operator</span></div><form method="post" action="/admin/logout"><button type="submit">Sign out</button></form></header>
<main class="detail"><p><a href="/">← Live actions</a></p>{{end}}<div class="heading"><p class="eyebrow">ACTION DETAIL</p><h1>{{.AttemptID}}</h1></div>
<section class="panel"><h2>Action and deadline</h2><dl>
<dt>Repository</dt><dd>{{if .Repository}}{{.Repository}}{{else}}Unavailable in retained records{{end}}</dd>
<dt>Requester</dt><dd>{{if .Requester}}{{.Requester}}{{else}}Unavailable in retained records{{end}}</dd>
<dt>Action</dt><dd>{{if .Action}}{{.Action}}{{else}}Unavailable after action cleanup{{end}}</dd>
{{if .Summary.CommandName}}<dt>Command</dt><dd>{{.Summary.CommandName}}</dd>{{end}}
{{if .Summary.MainProvider}}<dt>Main model</dt><dd>{{.Summary.MainProvider}}{{with .Summary.MainSettings.Model}} / {{.}}{{end}}{{with .Summary.MainSettings.Effort}} / {{.}}{{end}}</dd>{{end}}
{{if .Summary.CriticProvider}}<dt>Critic model</dt><dd>{{.Summary.CriticProvider}}{{with .Summary.CriticSettings.Model}} / {{.}}{{end}}{{with .Summary.CriticSettings.Effort}} / {{.}}{{end}}</dd>{{end}}
<dt>Outcome</dt><dd>{{.Outcome}}</dd>
{{if .Progress}}<dt>Progress</dt><dd>{{.Progress}}</dd>{{end}}
<dt>Original deadline</dt><dd>{{if .Deadline.IsZero}}Unavailable in older saved decision{{else}}{{utc .Deadline}}{{end}}</dd>
<dt>Remaining budget</dt><dd>{{if or (eq .Outcome "In progress") (eq .Outcome "Retry pending")}}{{.Remaining}}{{else}}No longer running{{end}}</dd>
<dt>Publication</dt><dd>{{.Publication}}</dd>{{if .PushPublication}}<dt>Fix push</dt><dd>{{.PushPublication}}</dd>{{end}}
</dl></section>
<section class="panel"><h2>Critic evidence</h2><dl><dt>Actual full-review verdict</dt><dd>{{if .CriticVerdict}}{{.CriticVerdict}}{{else}}Unavailable{{end}}</dd>
<dt>Stopping reason</dt><dd>{{if .CriticStop}}{{.CriticStop}}{{else}}Unavailable{{end}}</dd></dl><p class="hint">{{.ReviewNotice}}</p>
{{if .Refutations}}<h3>Main-model refutations</h3>{{range .Refutations}}<article class="detail-entry"><p>{{.Meta}}{{if .Truncated}} · text truncated{{end}}</p><pre>{{.Text}}</pre></article>{{end}}{{end}}</section>
<section class="panel"><h2>Received private discussion, reports and diagnostics</h2><p class="hint">{{.RecordsNotice}}</p>
{{if .Records}}{{range .Records}}<article class="detail-entry"><p>{{.Kind}}{{if .Meta}} · {{.Meta}}{{end}}{{if .Truncated}} · text truncated{{end}}</p><pre>{{.Text}}</pre></article>{{end}}{{else}}<p>No received text shown.</p>{{end}}</section>
<section class="panel"><h2>Worker diagnostics</h2><p class="hint">{{.WorkerNotice}}</p>{{if .WorkerFailure}}<article class="detail-entry"><p>Worker-reported failure</p><pre>{{.WorkerFailure}}</pre></article>{{end}}{{if .WorkerObservedFailure}}<article class="detail-entry"><p>Worker-observed failure</p><pre>{{.WorkerObservedFailure}}</pre></article>{{end}}
{{range .Diagnostics}}<article class="detail-entry"><p>{{.Kind}}{{if .Meta}} · {{.Meta}}{{end}}{{if .Truncated}} · text truncated{{end}}</p><pre>{{.Text}}</pre></article>{{end}}</section>
</main>{{end}}{{else}}<p role="alert">Admin session ended.</p>{{end}}{{$.TailHTML}}</body></html>`))

type actionDetailView struct {
	action.DashboardDetail
	AllowUser bool
	access    *userActionAccess
}

func (v actionDetailView) JawsConnect(rq *jaws.Request) error {
	if !v.AllowUser {
		return nil
	}
	if v.access == nil {
		return errUserActionDenied
	}
	id, err := v.access.requesterID(rq.Session())
	if err != nil || id == 0 || id != v.Summary.RequesterID {
		return errUserActionDenied
	}
	ctx, cancel := context.WithTimeout(rq.Context(), 20*time.Second)
	defer cancel()
	if !v.access.admitRead(ctx) {
		return errUserActionBusy
	}
	current, found, err := v.access.owner.UserActionIdentity(v.AttemptID, id)
	v.access.finishRead()
	if err != nil || !found || current != v.Summary {
		return errUserActionDenied
	}
	if err := v.access.checkRepository(ctx, v.Summary); err != nil {
		return errUserActionDenied
	}
	return nil
}

func adminActionDetailHandler(jw *jaws.Jaws, owner *action.Orchestrator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		if !workspace.ValidIdentitySegment(r.PathValue("attempt")) {
			http.Error(w, "Invalid attempt ID", http.StatusBadRequest)
			return
		}
		if owner == nil {
			http.Error(w, "Action detail unavailable", http.StatusServiceUnavailable)
			return
		}
		detail, found, err := owner.DashboardActionDetail(r.PathValue("attempt"), time.Now().UTC())
		if err != nil {
			http.Error(w, "Action detail unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		ui.Handler(jw, "admin action detail", actionDetailView{DashboardDetail: detail}).ServeHTTP(w, r)
	})
}
