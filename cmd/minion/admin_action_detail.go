package main

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/workspace"
)

func evidenceRows(text string) int {
	return min(8, max(2, strings.Count(text, "\n")+1))
}

var adminActionDetailTemplate = template.Must(template.New("admin action detail").Funcs(utcTemplateFuncs).Funcs(template.FuncMap{
	"evidenceRows": evidenceRows,
}).Parse(headHTML + `{{template "head.html" $}}<title>Minion action detail</title></head><body>{{if or .Auth.IsAdmin .Dot.AllowUser}}{{with .Dot}}
<header class="d-flex flex-wrap align-items-center gap-3 px-4 py-3 border-bottom bg-body-tertiary"><div><strong>MINION</strong><span class="text-body-secondary"> / {{if .AllowUser}}my actions{{else}}operator{{end}}</span></div><form class="ms-auto" method="post" action="{{if .AllowUser}}/auth/logout{{else}}/admin/logout{{end}}"><button class="btn btn-outline-light" type="submit">Sign out</button></form></header>
<main class="container py-4"><p><a href="{{if .AllowUser}}/auth/actions{{else}}/{{end}}">← {{if .AllowUser}}My actions{{else}}Live actions{{end}}</a></p><div class="border-bottom mb-4"><p class="text-body-secondary small fw-semibold">ACTION DETAIL</p><h1>{{.AttemptID}}</h1></div>
<section class="border rounded p-3 mb-3"><h2 class="h5">Action and deadline</h2><dl class="text-break">
<dt>Repository</dt><dd>{{if .Repository}}{{.Repository}}{{else}}Unavailable in retained records{{end}}</dd>
<dt>Requester</dt><dd>{{if .Requester}}{{.Requester}}{{else}}Unavailable in retained records{{end}}</dd>
<dt>Action</dt><dd>{{if .Action}}{{.Action}}{{else}}Unavailable after action cleanup{{end}}</dd>
{{if .Summary.CommandName}}<dt>Command</dt><dd>{{.Summary.CommandName}}</dd>{{end}}
{{if .Summary.MainProvider}}<dt>Main model</dt><dd>{{.Summary.MainProvider}}{{with .Summary.MainSettings.Model}} / {{.}}{{end}}{{with .Summary.MainSettings.Effort}} / {{.}}{{end}}</dd>{{end}}
{{if .Summary.CriticProvider}}<dt>Critic model</dt><dd>{{.Summary.CriticProvider}}{{with .Summary.CriticSettings.Model}} / {{.}}{{end}}{{with .Summary.CriticSettings.Effort}} / {{.}}{{end}}</dd>{{end}}
<dt>Outcome</dt><dd>{{.Outcome}}</dd>
{{if .StopRequest.Status}}<dt>Stop status</dt><dd>{{if eq .StopRequest.Status "requested"}}Termination requested; waiting for stop verification.{{else if eq .StopRequest.Status "confirmed"}}Termination confirmed.{{else}}Termination could not be verified; {{if .AllowUser}}contact an operator to check executor readiness before reuse.{{else}}check executor readiness before reuse.{{end}}{{end}}</dd>{{end}}
{{if .Progress}}<dt>Progress</dt><dd>{{.Progress}}</dd>{{end}}
<dt>Original deadline</dt><dd>{{if .Deadline.IsZero}}Unavailable in older saved decision{{else}}{{utc .Deadline}}{{end}}</dd>
<dt>Remaining budget</dt><dd>{{if or (eq .Outcome "In progress") (eq .Outcome "Retry pending")}}{{.Remaining}}{{else}}No longer running{{end}}</dd>
<dt>Publication</dt><dd>{{.Publication}}</dd>{{if .PushPublication}}<dt>Fix push</dt><dd>{{.PushPublication}}</dd>{{end}}
</dl>{{if and .AllowUser .Stoppable}}<form method="post" action="/auth/actions/{{.AttemptID}}/abort"><button class="btn btn-warning" type="submit">Abort this action</button></form>{{end}}</section>
<section class="border rounded p-3 mb-3"><h2 class="h5">Critic evidence</h2><dl class="text-break"><dt>Actual full-review verdict</dt><dd>{{if .CriticVerdict}}{{.CriticVerdict}}{{else}}Unavailable{{end}}</dd>
<dt>Stopping reason</dt><dd>{{if .CriticStop}}{{.CriticStop}}{{else}}Unavailable{{end}}</dd></dl><p class="text-body-secondary small">{{.ReviewNotice}}</p>
{{if .Refutations}}<h3 class="h6">Main-model refutations</h3>{{range $index, $item := .Refutations}}<article class="border-top pt-2 mt-2"><p id="refutation-caption-{{$index}}" class="text-body-secondary small">{{.Kind}}{{if .Meta}} · {{.Meta}}{{end}}{{if .Truncated}} · text truncated{{end}}</p><textarea class="form-control font-monospace" rows="{{evidenceRows .Text}}" readonly aria-labelledby="refutation-caption-{{$index}}">
{{.Text}}</textarea></article>{{end}}{{end}}</section>
<section class="border rounded p-3 mb-3"><h2 class="h5">Received private discussion, reports and diagnostics</h2><p class="text-body-secondary small">{{.RecordsNotice}}</p>
{{if .Records}}{{range $index, $item := .Records}}<article class="border-top pt-2 mt-2"><p id="record-caption-{{$index}}" class="text-body-secondary small">Received {{.Kind}}{{if .Meta}} · {{.Meta}}{{end}}{{if .Truncated}} · text truncated{{end}}</p><textarea class="form-control font-monospace" rows="{{evidenceRows .Text}}" readonly aria-labelledby="record-caption-{{$index}}">
{{.Text}}</textarea></article>{{end}}{{else}}<p>No received text shown.</p>{{end}}</section>
<section class="border rounded p-3 mb-3"><h2 class="h5">Worker diagnostics</h2><p class="text-body-secondary small">{{.WorkerNotice}}</p>{{if .WorkerFailure}}<article class="border-top pt-2 mt-2"><p class="text-body-secondary small">Worker-reported failure</p><textarea class="form-control font-monospace" rows="{{evidenceRows .WorkerFailure}}" readonly aria-label="Worker-reported failure">
{{.WorkerFailure}}</textarea></article>{{end}}{{if .WorkerObservedFailure}}<article class="border-top pt-2 mt-2"><p class="text-body-secondary small">Worker-observed failure</p><textarea class="form-control font-monospace" rows="{{evidenceRows .WorkerObservedFailure}}" readonly aria-label="Worker-observed failure">
{{.WorkerObservedFailure}}</textarea></article>{{end}}
{{range $index, $item := .Diagnostics}}<article class="border-top pt-2 mt-2"><p id="diagnostic-caption-{{$index}}" class="text-body-secondary small">Worker diagnostic · {{.Kind}}{{if .Meta}} · {{.Meta}}{{end}}{{if .Truncated}} · text truncated{{end}}</p><textarea class="form-control font-monospace" rows="{{evidenceRows .Text}}" readonly aria-labelledby="diagnostic-caption-{{$index}}">
{{.Text}}</textarea></article>{{end}}</section>
</main>{{end}}{{else}}<p role="alert" class="alert alert-warning">Admin session ended.</p>{{end}}{{$.TailHTML}}</body></html>`))

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
	_, current, found, err := v.access.owner.UserActionIdentity(v.AttemptID, id)
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
