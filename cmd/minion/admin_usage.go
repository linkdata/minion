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

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/usage"
)

var adminUsageHTML = template.Must(template.New("admin usage").Parse(headHTML + usageEstimatedCostTemplate + `{{template "head.html" $}}<title>Usage · Minion admin</title>
</head>
<body>{{with .Dot}}
  <header class="d-flex flex-wrap align-items-center gap-3 px-4 py-3 border-bottom bg-body-tertiary">
    <div><strong>MINION</strong><span class="text-body-secondary"> / usage</span></div>
    <nav><a href="/">Live actions</a></nav>
    <form class="ms-auto" method="post" action="/admin/logout"><button class="btn btn-outline-light" type="submit">Sign out</button></form>
  </header>
  <main class="container-fluid py-4">
    <div class="border-bottom mb-4">
      <p class="text-body-secondary small fw-semibold">ACCOUNTING</p><h1>Usage and billing</h1>
      <p>Date filters use attempt start times in UTC. The To boundary is exclusive.</p>
    </div>
    <form class="d-flex flex-wrap align-items-end gap-3 mb-4" method="get" action="/admin/usage">
      <label class="d-grid gap-1">From (UTC)<input class="form-control" name="from" value="{{.From}}" maxlength="35" placeholder="YYYY-MM-DD"></label>
      <label class="d-grid gap-1">To, exclusive (UTC)<input class="form-control" name="to" value="{{.To}}" maxlength="35" placeholder="YYYY-MM-DD"></label>
      <label class="d-grid gap-1">Repository<input class="form-control" name="repo" value="{{.Repo}}" maxlength="201" placeholder="owner/repo"></label>
      <label class="d-grid gap-1">Repository ID<input class="form-control" name="repository_id" value="{{.RepositoryID}}" inputmode="numeric" maxlength="20"></label>
      <label class="d-grid gap-1">Requester<input class="form-control" name="user" value="{{.User}}" maxlength="105" placeholder="GitHub login"></label>
      <label class="d-grid gap-1">Requester ID<input class="form-control" name="requester_id" value="{{.RequesterID}}" inputmode="numeric" maxlength="20"></label>
      <label class="d-grid gap-1">Service<select class="form-select" name="service">
        <option value=""{{if eq .Service ""}} selected{{end}}>All services</option>
        <option value="codex"{{if eq .Service "codex"}} selected{{end}}>Codex</option>
        <option value="claude"{{if eq .Service "claude"}} selected{{end}}>Claude</option>
        <option value="unknown"{{if eq .Service "unknown"}} selected{{end}}>Unknown</option>
      </select></label>
      <button class="btn btn-primary" type="submit">Apply filters</button>
    </form>
    <p><a href="{{.ExportURL}}">Download filtered JSON</a>. With a service filter, token counts in <code>totals</code> and <code>selected_usage</code> reflect that service; outcome counts remain attempt-level. Each attempt's <code>tokens</code> and <code>total_tokens</code> retain all services.</p>
    <div class="row row-cols-1 row-cols-md-2 g-3 mb-3">
      <div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">Attempts</h2><p class="display-6 font-monospace">{{.Report.Totals.Attempts}}</p></article></div>
      <div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">Reported tokens</h2><p class="display-6 font-monospace">{{.Report.Totals.TotalTokens}}</p></article></div>
      <div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">Unallocated tokens</h2><p class="display-6 font-monospace">{{.Report.Totals.UnallocatedTokens.Total}}</p></article></div>
      <div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">Attempts with incomplete usage or still running</h2><p class="display-6 font-monospace">{{.Report.Totals.IncompleteUsage}}</p></article></div>
    </div>
    <p>Estimated cost and currency: {{template "estimatedCost" .Report.Totals}}. Billed cost and credits: Unknown.</p>
    <section class="border rounded p-3 mb-3 overflow-auto" aria-labelledby="usage-groups-heading" tabindex="0"><h2 id="usage-groups-heading" class="h5">Usage by repository, requester, and service</h2>
      {{if .Report.Groups}}<table class="table table-striped align-middle"><thead><tr>
        <th scope="col">Repository</th><th scope="col">Repository ID</th>
        <th scope="col">Requester</th><th scope="col">Requester ID</th><th scope="col">Service</th>
        <th scope="col">Attempts</th><th scope="col">Total tokens</th>
        <th scope="col">Unallocated tokens</th><th scope="col">Attempts with incomplete usage or still running</th>
        <th scope="col">Estimated cost and currency</th>
      </tr></thead><tbody>{{range .Report.Groups}}<tr>
        <td>{{.Repo}}</td><td>{{if .RepositoryID}}{{.RepositoryID}}{{else}}Unknown{{end}}</td>
        <td>{{.User}}</td><td>{{if .RequesterID}}{{.RequesterID}}{{else}}Unknown{{end}}</td><td>{{.Service}}</td>
        <td class="text-end font-monospace">{{.Attempts}}</td><td class="text-end font-monospace">{{.TotalTokens}}</td>
        <td class="text-end font-monospace">{{.UnallocatedTokens.Total}}</td><td class="text-end font-monospace">{{.IncompleteUsage}}</td>
        <td>{{template "estimatedCost" .}}</td>
      </tr>{{end}}</tbody></table>{{else}}<p>No usage records match these filters.</p>{{end}}
    </section>
    <p class="text-body-secondary small">Action coverage: {{.Report.ActionCoverage}}.
      Actions without usage files are excluded.
      Repository and requester ID filters exclude older usage records without the corresponding numeric IDs.
      Tokens and USD estimates are executor-reported; incomplete usage can omit unknown amounts.
      An attempt using multiple services can appear in more than one service group,
      while the totals above count it once.</p>
  </main>{{end}}{{$.TailHTML}}
</body>
</html>`))

var adminUsageGroups = []string{"repo", "repository_id", "user", "requester_id", "service"}

type adminUsageQuery struct {
	From, To, Repo, RepositoryID, User, RequesterID, Service, ExportURL string
	Report                                                              usageOutput
}

func parseAdminUsageQuery(raw string) (adminUsageQuery, usage.Filter, error) {
	if len(raw) > 2048 {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
	}
	limits := map[string]int{"from": 35, "to": 35, "repo": 201, "repository_id": 20, "user": 105, "requester_id": 20, "service": 7}
	for name, entries := range values {
		limit, allowed := limits[name]
		if !allowed || len(entries) != 1 || len(entries[0]) > limit || !utf8.ValidString(entries[0]) || strings.ContainsAny(entries[0], "\x00\r\n") {
			return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
		}
	}
	query := adminUsageQuery{From: values.Get("from"), To: values.Get("to"), Repo: strings.TrimSpace(values.Get("repo")),
		User: strings.TrimSpace(values.Get("user")), Service: strings.TrimSpace(values.Get("service")), ExportURL: "/admin/usage.json"}
	if raw != "" {
		query.ExportURL += "?" + raw
	}
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
	var repositoryID int64
	if value := values.Get("repository_id"); value != "" {
		repositoryID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || repositoryID <= 0 {
			return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
		}
		query.RepositoryID = value
	}
	var requesterID int64
	if value := values.Get("requester_id"); value != "" {
		requesterID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || requesterID <= 0 {
			return adminUsageQuery{}, usage.Filter{}, errAdminUsageFilter
		}
		query.RequesterID = value
	}
	return query, usage.Filter{Repo: query.Repo, RepositoryID: repositoryID, User: query.User, Service: query.Service, RequesterID: requesterID, Since: since, Until: until}, nil
}

var errAdminUsageFilter = errors.New("invalid admin usage filter")

func adminUsageHandler(jw *jaws.Jaws, owner *action.Orchestrator, export bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
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
		ui.Handler(jw, "admin usage", query).ServeHTTP(w, r)
	})
}
