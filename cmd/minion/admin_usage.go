package main

import (
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/usage"
)

var adminUsageHTML = template.Must(template.New("admin usage").Parse(`<!doctype html><html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Usage · Minion admin</title><link rel="stylesheet" href="/admin/dashboard.css"></head>
<body><header class="topbar"><div><strong>MINION</strong><span> / usage</span></div><nav><a href="/">Live actions</a></nav><form method="post" action="/admin/logout"><button type="submit">Sign out</button></form></header>
<main><div class="heading"><p class="eyebrow">ACCOUNTING</p><h1>Usage and billing</h1><p>All dates are UTC. The To boundary is exclusive.</p></div>
<form class="filters" method="get" action="/admin/usage">
<label>From (UTC)<input name="from" value="{{.From}}" maxlength="35" placeholder="YYYY-MM-DD"></label>
<label>To, exclusive (UTC)<input name="to" value="{{.To}}" maxlength="35" placeholder="YYYY-MM-DD"></label>
<label>Repository<input name="repo" value="{{.Repo}}" maxlength="201" placeholder="owner/repo"></label>
<label>Requester<input name="user" value="{{.User}}" maxlength="105" placeholder="GitHub login"></label>
<label>Requester ID<input name="requester_id" value="{{.RequesterID}}" inputmode="numeric" maxlength="20"></label>
<label>Service<select name="service"><option value=""{{if eq .Service ""}} selected{{end}}>All services</option><option value="codex"{{if eq .Service "codex"}} selected{{end}}>Codex</option><option value="claude"{{if eq .Service "claude"}} selected{{end}}>Claude</option><option value="unknown"{{if eq .Service "unknown"}} selected{{end}}>Unknown</option></select></label>
<button type="submit">Apply filters</button></form>
<p><a href="/admin/usage.json{{if .RawQuery}}?{{.RawQuery}}{{end}}">Download filtered JSON</a></p>
<div class="metrics"><article><h2>Attempts</h2><p class="metric">{{.Report.Totals.Attempts}}</p></article><article><h2>Reported tokens</h2><p class="metric">{{.Report.Totals.TotalTokens}}</p></article><article><h2>Unallocated tokens</h2><p class="metric">{{.Report.Totals.UnallocatedTokens.Total}}</p></article><article><h2>Incomplete usage</h2><p class="metric">{{.Report.Totals.IncompleteUsage}}</p></article></div>
<section class="panel actions"><h2>Usage by repository, requester, and service</h2>
{{if .Report.Groups}}<table><thead><tr><th scope="col">Repository</th><th scope="col">Requester</th><th scope="col">Service</th><th scope="col">Attempts</th><th scope="col">Total tokens</th><th scope="col">Unallocated</th><th scope="col">Incomplete</th></tr></thead><tbody>{{range .Report.Groups}}<tr><td>{{.Repo}}</td><td>{{.User}}</td><td>{{.Service}}</td><td class="number">{{.Attempts}}</td><td class="number">{{.TotalTokens}}</td><td class="number">{{.UnallocatedTokens.Total}}</td><td class="number">{{.IncompleteUsage}}</td></tr>{{end}}</tbody></table>{{else}}<p>No usage records match these filters.</p>{{end}}</section>
<p class="hint">Action coverage: {{.Report.ActionCoverage}}. Billed credits: unknown. Tokens are executor-reported; incomplete usage can omit unknown amounts. An attempt using multiple services can appear in more than one service group, while the totals above count it once.</p>
</main></body></html>`))

var adminUsageGroups = []string{"repo", "user", "service"}

type adminUsageQuery struct {
	From, To, Repo, User, RequesterID, Service, RawQuery string
	Report                                               usageOutput
}

func parseAdminUsageQuery(raw string) (adminUsageQuery, usage.Filter, error) {
	if len(raw) > 2048 {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	limits := map[string]int{"from": 35, "to": 35, "repo": 201, "user": 105, "requester_id": 20, "service": 7}
	for name, entries := range values {
		limit, allowed := limits[name]
		if !allowed || len(entries) != 1 || len(entries[0]) > limit || !utf8.ValidString(entries[0]) || strings.ContainsAny(entries[0], "\x00\r\n") {
			return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
		}
	}
	query := adminUsageQuery{From: values.Get("from"), To: values.Get("to"), Repo: strings.TrimSpace(values.Get("repo")),
		User: strings.TrimSpace(values.Get("user")), Service: strings.TrimSpace(values.Get("service")), RawQuery: raw}
	if query.Service != "" && query.Service != "codex" && query.Service != "claude" && query.Service != "unknown" {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	since, err := parseUsageTime(query.From)
	if err != nil {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	until, err := parseUsageTime(query.To)
	if err != nil || !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	var requesterID int64
	if value := values.Get("requester_id"); value != "" {
		requesterID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || requesterID <= 0 {
			return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
		}
		query.RequesterID = value
	}
	return query, usage.Filter{Repo: query.Repo, User: query.User, Service: query.Service, RequesterID: requesterID, Since: since, Until: until}, nil
}

var errAdminUsageFilter = errors.New("invalid admin usage filter")

func adminUsageHandler(owner *action.Orchestrator, export bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		query, filter, err := parseAdminUsageQuery(r.URL.RawQuery)
		if err != nil {
			http.Error(w, "Invalid usage filter", http.StatusBadRequest)
			return
		}
		if owner == nil || owner.Usage == nil {
			http.Error(w, "Usage report unavailable", http.StatusServiceUnavailable)
			return
		}
		report, err := owner.Usage.Query(filter, adminUsageGroups, export)
		if err != nil {
			slog.Error("admin usage query", "error", err)
			http.Error(w, "Usage report unavailable", http.StatusInternalServerError)
			return
		}
		query.Report = usageReport(filter, adminUsageGroups, report)
		if export {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="minion-usage.json"`)
			if err := json.NewEncoder(w).Encode(query.Report); err != nil {
				slog.Error("admin usage export", "error", err)
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := adminUsageHTML.Execute(w, query); err != nil {
			slog.Error("admin usage page", "error", err)
		}
	})
}
