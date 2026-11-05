package main

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

const userUsageSnapshotTTL = 10 * time.Second
const userUsageAPIBurst = 256
const userUsageAPIInterval = 5 * time.Second
const userUsageAccountBurst = 128
const userUsageAccountInterval = 10 * time.Second
const userUsagePageSize = 4
const userUsagePageCallLimit = 30

// Four-call reserve for one uncached linked identity granting private access:
// repository by ID, user by ID, permission, repository by name for ID recheck.
const userUsageMinPrivateCalls = 4

type userUsageAccountBudget struct {
	tokens   int
	refillAt time.Time
	lastUsed time.Time
}

type userUsageGate struct {
	reports             chan struct{}
	scan                chan struct{}
	mu                  sync.Mutex
	records             []usage.Record
	readAt              time.Time
	readErr             error
	apiTokens           int
	apiBurst            int
	apiRefillAt         time.Time
	accounts            map[string]userUsageAccountBudget
	accountBurst        int
	lastPrune           time.Time
	verificationTimeout time.Duration
}

func newUserUsageGate() *userUsageGate {
	return &userUsageGate{reports: make(chan struct{}, 2), scan: make(chan struct{}, 1),
		apiTokens: userUsageAPIBurst, apiBurst: userUsageAPIBurst,
		accounts: make(map[string]userUsageAccountBudget), accountBurst: userUsageAccountBurst,
		verificationTimeout: 8 * time.Second}
}

func (g *userUsageGate) read(dir string) ([]usage.Record, bool, error) {
	g.mu.Lock()
	if !g.readAt.IsZero() && time.Since(g.readAt) < userUsageSnapshotTTL {
		records := g.records
		err := g.readErr
		g.mu.Unlock()
		return records, false, err
	}
	g.mu.Unlock()
	select {
	case g.scan <- struct{}{}:
		defer func() { <-g.scan }()
	default:
		return nil, true, nil
	}
	g.mu.Lock()
	if !g.readAt.IsZero() && time.Since(g.readAt) < userUsageSnapshotTTL {
		records := g.records
		err := g.readErr
		g.mu.Unlock()
		return records, false, err
	}
	g.mu.Unlock()
	// shortcut: this snapshot caps scan frequency, not per-scan work. Add a
	// durable requester index if history outgrows memory or scans delay reports.
	records, err := usage.ReadAll(dir)
	g.mu.Lock()
	g.records, g.readAt, g.readErr = records, time.Now(), err
	g.mu.Unlock()
	return records, false, err
}

func (g *userUsageGate) gitHubCallBudget(ctx context.Context, account string, need int, debit bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx.Err() != nil || need <= 0 {
		return false
	}
	now := time.Now()
	if g.apiRefillAt.IsZero() {
		g.apiRefillAt = now
	}
	if elapsed := int(now.Sub(g.apiRefillAt) / userUsageAPIInterval); elapsed > 0 {
		g.apiTokens = min(g.apiBurst, g.apiTokens+elapsed)
		g.apiRefillAt = g.apiRefillAt.Add(time.Duration(elapsed) * userUsageAPIInterval)
	}
	if g.lastPrune.IsZero() || now.Sub(g.lastPrune) >= time.Hour {
		for key, budget := range g.accounts {
			if now.Sub(budget.lastUsed) >= time.Hour {
				delete(g.accounts, key)
			}
		}
		g.lastPrune = now
	}
	budget, ok := g.accounts[account]
	if !ok {
		budget = userUsageAccountBudget{tokens: g.accountBurst, refillAt: now}
	}
	if elapsed := int(now.Sub(budget.refillAt) / userUsageAccountInterval); elapsed > 0 {
		budget.tokens = min(g.accountBurst, budget.tokens+elapsed)
		budget.refillAt = budget.refillAt.Add(time.Duration(elapsed) * userUsageAccountInterval)
	}
	budget.lastUsed = now
	g.accounts[account] = budget
	if g.apiTokens < need || budget.tokens < need {
		return false
	}
	if debit {
		g.apiTokens--
		budget.tokens--
		g.accounts[account] = budget
	}
	return true
}

func (g *userUsageGate) readyGitHubCalls(ctx context.Context, account string, need int) bool {
	return g.gitHubCallBudget(ctx, account, need, false)
}

func (g *userUsageGate) takeGitHubCall(ctx context.Context, account string) bool {
	return g.gitHubCallBudget(ctx, account, 1, true)
}

const usageEstimatedCostTemplate = `{{define "estimatedCost"}}{{if .EstimatedCostKnownCalls}}USD {{printf "%.15g" .EstimatedCostUSD}} {{if .EstimatedCostUnknownCalls}}known estimated subtotal{{else}}reported estimated total{{end}}{{else}}Unknown{{end}}{{if or .EstimatedCostKnownCalls .EstimatedCostUnknownCalls}} (calls with any reported amount: {{.EstimatedCostKnownCalls}}; calls with missing or incomplete estimates: {{.EstimatedCostUnknownCalls}}{{if and .EstimatedCostKnownCalls .EstimatedCostUnknownCalls}}; counts may overlap{{end}}){{end}}{{end}}`

var userUsageHTML = template.Must(template.New("user usage").Parse(usageEstimatedCostTemplate + `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Your usage · Minion</title><link rel="stylesheet" href="/user/usage.css">{{$.HeadHTML}}
</head>
<body>{{with .Dot}}
  <header class="topbar">
    <div><strong>MINION</strong><span> / your usage</span></div>
    <nav><a href="/auth/login">Account</a></nav>
    <form method="post" action="/auth/logout"><button type="submit">Sign out</button></form>
  </header>
  <main>
    <div class="heading">
      <p class="eyebrow">ACCOUNTING</p><h1>Your usage</h1>
      <p>Totals include usage from your linked GitHub identity. Repository filters affect the table only.</p>
    </div>
    <div class="metrics">
      <article><h2>Reported tokens</h2><p class="metric">{{.Total.TotalTokens}}</p></article>
      <article><h2>Unallocated tokens</h2><p class="metric">{{.Total.UnallocatedTokens.Total}}</p></article>
      <article><h2>Usage-recorded actions</h2><p class="metric">{{.Total.UsageRecordedActions}}</p></article>
      <article><h2>Incomplete attempts</h2><p class="metric">{{.Total.IncompleteUsage}}</p></article>
    </div>
    <section class="panel"><h2>All attempts</h2><dl>
      <dt>Attempts</dt><dd>{{.Total.Attempts}} ({{.Total.Succeeded}} succeeded, {{.Total.Failed}} failed or stopped, {{.Total.Running}} running)</dd>
      <dt>Input tokens</dt><dd>{{.Total.Tokens.Input}}</dd>
      <dt>Cache creation input</dt><dd>{{.Total.Tokens.CacheCreationInput}}</dd>
      <dt>Cached input</dt><dd>{{.Total.Tokens.CachedInput}}</dd>
      <dt>Output tokens</dt><dd>{{.Total.Tokens.Output}}</dd>
      <dt>Call checkpoints</dt><dd>{{.Total.CodexCalls}} Codex, {{.Total.ClaudeCalls}} Claude</dd>
      <dt>Billed credits</dt><dd>Unknown</dd>
      <dt>Reported cost and currency</dt><dd>Unknown</dd>
      <dt>Estimated cost and currency</dt><dd>{{template "estimatedCost" .Total}}</dd>
      <dt>Billed cost and currency</dt><dd>Unknown</dd>
    </dl></section>
    <p class="hint">Reported tokens are input plus output. Cache creation and cached input are included in input tokens; unallocated tokens are included in the reported total but lack call attribution. Token counts and Claude CLI USD estimates are executor-reported claims, not verified billing. They include captured main, critic, and helper usage. Call checkpoints can precede provider invocation. Estimated cost is a known subtotal when any call lacks a complete estimate. Incomplete attempts can omit unknown amounts; actions without usage records and records without a stable requester ID are excluded.</p>
    <form class="filters" method="get" action="/user/usage">
      <label>Repository<input name="repo" value="{{.Repo}}" maxlength="201" placeholder="owner/repo"></label>
      <button type="submit">Filter repositories</button>
    </form>
    <section class="panel actions" aria-labelledby="accessible-repositories-heading" tabindex="0"><h2 id="accessible-repositories-heading">Accessible repositories</h2>
      {{if .Repo}}{{if not .Repositories}}{{if .RetryURL}}<p>Repository verification is incomplete at this position. Recheck{{if .ContinueURL}} or continue to later repositories{{end}}.</p>{{else if .VerificationUnavailable}}<p>Repository verification is unavailable right now. Retry this page later.</p>{{else if .ContinueURL}}<p>No matching accessible repository on this page. Continue checking.</p>{{else}}<p>No matching accessible repository was verified. Start over to check again.</p>{{end}}{{end}}{{else if .VerificationUnavailable}}<p>Some repository usage could not be verified right now. Refresh later.</p>{{end}}
      {{if .Repositories}}<table><thead><tr>
        <th scope="col">Repository</th><th scope="col">Attempts</th><th scope="col">Reported tokens</th>
        <th scope="col">Unallocated tokens</th><th scope="col">Incomplete attempts</th>
        <th scope="col">Billed credits</th><th scope="col">Reported cost and currency</th>
        <th scope="col">Estimated cost and currency</th><th scope="col">Billed cost and currency</th>
      </tr></thead><tbody>{{range .Repositories}}<tr>
        <th scope="row">{{.Repo}}</th><td class="number">{{.Attempts}}</td><td class="number">{{.TotalTokens}}</td>
        <td class="number">{{.UnallocatedTokens.Total}}</td><td class="number">{{.IncompleteUsage}}</td>
        <td>Unknown</td><td>Unknown</td><td>{{template "estimatedCost" .}}</td><td>Unknown</td>
      </tr>{{end}}</tbody></table>{{else if not .Repo}}{{if not .VerificationUnavailable}}<p>No accessible repository usage found in this check.</p>{{end}}{{end}}
      {{if .ContinueURL}}<a href="{{.ContinueURL}}">Continue checking repositories</a>{{end}}
      {{if .RetryURL}}<a href="{{.RetryURL}}">Recheck from this position</a>{{end}}
    </section>
    <p class="hint">Repository rows require current access and verified repository identity. Records without a stable repository ID appear only in your total. Your total remains available if repository access is lost. Totals use a short-lived snapshot and may lag recently saved usage. Continue and Recheck use positions; new usage or account-link changes can repeat or skip rows. Start over after changing linked accounts and for the latest view. Action coverage beyond saved usage records is unknown.</p>
  </main>{{end}}{{$.TailHTML}}
</body>
</html>`))

type userUsagePage struct {
	Repo                    string
	Total                   usage.Summary
	Repositories            []usage.Summary
	VerificationUnavailable bool
	ContinueURL             string
	RetryURL                string
}

func parseUserUsageQuery(raw string) (string, int, string, bool) {
	if len(raw) > 512 {
		return "", 0, "", false
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", 0, "", false
	}
	for key, items := range values {
		if key != "repo" && key != "after" && key != "sig" || len(items) != 1 {
			return "", 0, "", false
		}
	}
	repo := strings.TrimSpace(values.Get("repo"))
	if repo != "" && !workspace.ValidRepository(repo) {
		return "", 0, "", false
	}
	after := 0
	if rawAfter, ok := values["after"]; ok {
		after, err = strconv.Atoi(rawAfter[0])
		if err != nil || after < 0 || after > 1000000 {
			return "", 0, "", false
		}
	}
	signature := values.Get("sig")
	if after == 0 && signature != "" {
		return "", 0, "", false
	}
	return repo, after, signature, true
}

func userUsageContinueURL(repo string, after int, signature string) string {
	values := url.Values{}
	if repo != "" {
		values.Set("repo", repo)
	}
	if after > 0 {
		values.Set("after", strconv.Itoa(after))
	}
	if signature != "" {
		values.Set("sig", signature)
	}
	if len(values) == 0 {
		return "/user/usage"
	}
	return "/user/usage?" + values.Encode()
}

func (a *oauthAccess) usageHandler(owner *action.Orchestrator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closeUnreadAdminBody(w, r)
		w.Header().Set("Cache-Control", "no-store")
		account := a.sessionAccount(r)
		if account == "" {
			if a.links.storageError() != nil {
				a.browserError(w, r, http.StatusServiceUnavailable)
				return
			}
			target := "/auth/login"
			if returnTo := userUsageReturnTarget(r.URL.RequestURI()); returnTo != "" {
				target += "?next=" + url.QueryEscape(returnTo)
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		repo, after, signature, ok := parseUserUsageQuery(r.URL.RawQuery)
		if !ok {
			a.browserError(w, r, http.StatusBadRequest)
			return
		}
		if !a.validUsageCursor(account, repo, after, signature) {
			a.browserError(w, r, http.StatusBadRequest)
			return
		}
		if owner == nil || owner.Usage == nil {
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		select {
		case a.usageGate.reports <- struct{}{}:
			defer func() { <-a.usageGate.reports }()
		default:
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		githubID, err := a.links.githubID(account)
		if err != nil {
			slog.Error("user links lookup", "error", err)
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		var records []usage.Record
		if githubID != 0 {
			var busy bool
			var err error
			records, busy, err = a.usageGate.read(owner.Usage.Dir)
			if busy {
				a.browserError(w, r, http.StatusServiceUnavailable)
				return
			}
			if err != nil {
				slog.Error("user usage query", "error", err)
				a.browserError(w, r, http.StatusInternalServerError)
				return
			}
		}
		var owned []usage.Record
		for _, record := range records {
			if record.RequesterID == githubID && githubID != 0 {
				owned = append(owned, record)
			}
		}
		totals, err := usage.Summarize(owned, usage.Filter{}, nil)
		if err != nil {
			slog.Error("user usage totals", "error", err)
			a.browserError(w, r, http.StatusInternalServerError)
			return
		}
		groups, err := usage.Summarize(owned, usage.Filter{}, []string{"repository_id"})
		if err != nil {
			slog.Error("user usage repositories", "error", err)
			a.browserError(w, r, http.StatusInternalServerError)
			return
		}
		page := userUsagePage{Repo: repo}
		currentLogin := ""
		if len(totals) != 0 {
			page.Total = totals[0]
		}
		verifyCtx, cancel := context.WithTimeout(r.Context(), a.usageGate.verificationTimeout)
		defer cancel()
		var keyed []usage.Summary
		for _, group := range groups {
			if group.RepositoryID > 0 {
				keyed = append(keyed, group)
			}
		}
		calls := 0
		index := min(after, len(keyed))
		for index < len(keyed) && len(page.Repositories) < userUsagePageSize {
			if verifyCtx.Err() != nil {
				page.VerificationUnavailable = true
				if page.RetryURL == "" {
					page.RetryURL = a.signedUserUsageURL(account, repo, index)
				}
				break
			}
			if calls+userUsageMinPrivateCalls > userUsagePageCallLimit {
				break
			}
			if !a.usageGate.readyGitHubCalls(verifyCtx, account, userUsageMinPrivateCalls) {
				page.VerificationUnavailable = true
				if page.RetryURL == "" {
					page.RetryURL = a.signedUserUsageURL(account, repo, index)
				}
				break
			}
			group := keyed[index]
			check := userUsageRepositoryAllowed(verifyCtx, owner, group.RepositoryID, githubID, &currentLogin, a.usageGate, account, &calls)
			page.VerificationUnavailable = page.VerificationUnavailable || check.unavailable
			if check.unavailable {
				if page.RetryURL == "" {
					page.RetryURL = a.signedUserUsageURL(account, repo, index)
				}
			}
			index++
			if check.allowed && (repo == "" || strings.EqualFold(check.name, repo)) {
				group.Repo = check.name
				page.Repositories = append(page.Repositories, group)
				if repo != "" {
					break
				}
			}
		}
		if index < len(keyed) && (repo == "" || len(page.Repositories) == 0) {
			next := a.signedUserUsageURL(account, repo, index)
			if page.RetryURL != next {
				page.ContinueURL = next
			}
		}
		if r.Context().Err() != nil {
			return
		}
		ui.Handler(a.jaws, "user usage", page).ServeHTTP(w, r)
	})
}

type userUsageCheck struct {
	name        string
	allowed     bool
	unavailable bool
}

func userUsageGitHubNotFound(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusNotFound
}

func userUsageTakeCall(ctx context.Context, gate *userUsageGate, account string, calls *int) bool {
	if *calls >= userUsagePageCallLimit || !gate.takeGitHubCall(ctx, account) {
		return false
	}
	*calls += 1
	return true
}

func userUsageRepositoryAllowed(ctx context.Context, owner *action.Orchestrator, repositoryID, githubID int64, currentLogin *string, gate *userUsageGate, account string, calls *int) userUsageCheck {
	if repositoryID <= 0 || githubID <= 0 {
		return userUsageCheck{}
	}
	unavailable := userUsageCheck{unavailable: true}
	if owner.GitHub == nil || !userUsageTakeCall(ctx, gate, account, calls) {
		return unavailable
	}
	details, err := owner.GitHub.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return unavailable
	}
	if !details.Private && details.Visibility == "public" {
		return userUsageCheck{name: details.FullName, allowed: true}
	}
	if *currentLogin == "" {
		if !userUsageTakeCall(ctx, gate, account, calls) {
			return unavailable
		}
		user, err := owner.GitHub.UserByID(ctx, githubID)
		if err != nil {
			if userUsageGitHubNotFound(err) {
				return userUsageCheck{}
			}
			return unavailable
		}
		*currentLogin = user.Login
	}
	if !userUsageTakeCall(ctx, gate, account, calls) {
		return unavailable
	}
	permission, err := owner.GitHub.Permission(ctx, details.FullName, *currentLogin)
	if err != nil {
		if userUsageGitHubNotFound(err) {
			return userUsageCheck{}
		}
		return unavailable
	}
	if permission.User.ID != githubID {
		return unavailable
	}
	switch permission.Permission {
	case "read", "write", "admin":
		if !userUsageTakeCall(ctx, gate, account, calls) {
			return unavailable
		}
		verified, err := owner.GitHub.Repository(ctx, details.FullName)
		if err != nil || verified.ID != repositoryID {
			return unavailable
		}
		return userUsageCheck{name: verified.FullName, allowed: true}
	}
	return userUsageCheck{}
}
