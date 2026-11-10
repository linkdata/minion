package main

import (
	"context"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

var errUserActionDenied = errors.New("user action access denied")
var errUserActionBusy = errors.New("user action access busy")

const userActionVisiblePageSize = 4

type userActionPageKey struct{}

var userActionsHTML = template.Must(template.New("user actions").Funcs(utcTemplateFuncs).Parse(headHTML + `{{template "head.html" $}}<title>My Minion actions</title></head><body><main class="container py-4">
<h1>My actions</h1><p><a href="/auth/login">Account</a></p>
{{if .Dot.GitHubLinked}}
{{if .Dot.Entries}}<ol class="list-group">{{range .Dot.Entries}}<li class="list-group-item">
<p><a href="/auth/actions/{{.AttemptID}}">{{.Summary.Repository}} · {{.Summary.Action}} #{{.Summary.SubjectNumber}}</a></p>
<p>{{.Outcome}} · accepted {{utc .Summary.AcceptedAt}} · deadline {{utc .Deadline}}</p>
{{if .Progress}}<p>Progress: {{.Progress}}</p>{{end}}
<p>Command {{.Summary.CommandName}} · {{.Summary.SubjectType}} · requester {{.Summary.Requester}}</p>
<p>Main: {{.Summary.MainProvider}}{{with .Summary.MainSettings.Model}} / {{.}}{{end}}{{with .Summary.MainSettings.Effort}} / {{.}}{{end}}
{{if .Summary.CriticProvider}} · Critic: {{.Summary.CriticProvider}}{{with .Summary.CriticSettings.Model}} / {{.}}{{end}}{{with .Summary.CriticSettings.Effort}} / {{.}}{{end}}{{end}}</p>
</li>{{end}}</ol>{{else}}<p>No accessible actions in this batch. Older actions may exist.</p>{{end}}
{{if .Dot.Next}}<p><a href="/auth/actions?cursor={{.Dot.Next}}">Older actions</a></p>{{end}}
{{else}}{{if .Dot.GitHubAvailable}}<p>Link a GitHub account to see GitHub actions.</p>{{else}}<p>GitHub action history is unavailable on this server.</p>{{end}}{{end}}
</main>{{$.TailHTML}}</body></html>`))

type userActionsView struct {
	GitHubLinked, GitHubAvailable bool
	Entries                       []action.UserHistoryEntry
	Next                          string
	access                        *userActionAccess
}

func (v userActionsView) JawsConnect(rq *jaws.Request) error {
	if v.access == nil {
		return errUserActionDenied
	}
	ctx, cancel := context.WithTimeout(rq.Context(), 20*time.Second)
	defer cancel()
	return v.access.checkPage(ctx, rq.Session(), v.Entries)
}

type userActionAccess struct {
	oauth *oauthAccess
	owner *action.Orchestrator
}

func (a *userActionAccess) admitRead(ctx context.Context) bool {
	if a == nil || a.oauth == nil || ctx.Err() != nil {
		return false
	}
	select {
	case a.oauth.actionReads <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *userActionAccess) finishRead() { <-a.oauth.actionReads }

type repositoryProofKey struct {
	name                   string
	repo, owner, requester int64
}

type requesterProof struct {
	login string
	err   error
	ready bool
}

func (a *userActionAccess) checkRepositoryOnce(ctx context.Context, summary state.AcceptedSummary, cache map[repositoryProofKey]error, proof *requesterProof) error {
	key := repositoryProofKey{name: summary.Repository, repo: summary.RepositoryID,
		owner: summary.OwnerID, requester: summary.RequesterID}
	if result, ok := cache[key]; ok {
		return result
	}
	result := a.checkRepositoryWithProof(ctx, summary, proof)
	cache[key] = result
	return result
}

func (a *userActionAccess) requesterID(sess *jaws.Session) (int64, error) {
	account, _ := a.oauth.currentUser(sess)
	if account == "" {
		if err := a.oauth.links.storageError(); err != nil {
			return 0, err
		}
		return 0, errUserActionDenied
	}
	return a.oauth.links.githubID(account)
}

// checkRepository verifies original repository identity, then current private access.
// The bot token supplies the proof; the user's OAuth token is never broadened.
func (a *userActionAccess) checkRepository(ctx context.Context, summary state.AcceptedSummary) error {
	return a.checkRepositoryWithProof(ctx, summary, &requesterProof{})
}

func (a *userActionAccess) checkRepositoryWithProof(ctx context.Context, summary state.AcceptedSummary, proof *requesterProof) error {
	if a == nil || a.oauth == nil || a.owner == nil || a.owner.GitHub == nil || summary == (state.AcceptedSummary{}) {
		return errors.New("action access provider unavailable")
	}
	gate := a.oauth.usageGate
	if !gate.enter(ctx) {
		return errUserActionBusy
	}
	defer gate.leave()
	if !gate.take(ctx, summary.RequesterID) {
		return errUserActionBusy
	}
	repo, err := a.owner.GitHub.RepositoryByID(ctx, summary.RepositoryID)
	if err != nil {
		return repositoryAccessError(err)
	}
	if repo.ID != summary.RepositoryID || repo.Owner.ID != summary.OwnerID {
		return errUserActionDenied
	}
	if !repo.Private && repo.Visibility == "public" {
		return nil
	}
	if !repo.Private || repo.Visibility != "private" && repo.Visibility != "internal" {
		return errUserActionDenied // Missing or contradictory visibility cannot prove public access.
	}
	if !proof.ready {
		if !gate.take(ctx, summary.RequesterID) {
			return errUserActionBusy
		}
		user, err := a.owner.GitHub.UserByID(ctx, summary.RequesterID)
		proof.ready, proof.err = true, repositoryAccessError(err)
		if proof.err == nil {
			proof.login = user.Login
		}
	}
	if proof.err != nil {
		return proof.err
	}
	if !gate.take(ctx, summary.RequesterID) {
		return errUserActionBusy
	}
	permission, err := a.owner.GitHub.Permission(ctx, repo.FullName, proof.login)
	if err != nil {
		return repositoryAccessError(err)
	}
	if permission.User.ID != summary.RequesterID || !strings.EqualFold(permission.User.Login, proof.login) {
		return errUserActionDenied
	}
	switch permission.Permission {
	case "read", "write", "admin":
		// The repository name may have moved between the first lookup and the
		// collaborator check. Bind the granted permission to the same identity.
		if !gate.take(ctx, summary.RequesterID) {
			return errUserActionBusy
		}
		current, err := a.owner.GitHub.Repository(ctx, repo.FullName)
		if err != nil {
			return repositoryAccessError(err)
		}
		if current.ID != summary.RepositoryID || current.Owner.ID != summary.OwnerID {
			return errUserActionDenied
		}
		return nil
	default:
		return errUserActionDenied
	}
}

func repositoryAccessError(err error) error {
	var status *github.StatusError
	if errors.As(err, &status) && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusMovedPermanently) {
		return errUserActionDenied
	}
	return err
}

func (a *userActionAccess) checkPage(ctx context.Context, sess *jaws.Session, entries []action.UserHistoryEntry) error {
	id, err := a.requesterID(sess)
	if err != nil {
		return err
	}
	cache := make(map[repositoryProofKey]error)
	proof := &requesterProof{}
	for _, entry := range entries {
		if id == 0 || entry.Summary.RequesterID != id {
			return errUserActionDenied
		}
		if err := a.checkRepositoryOnce(ctx, entry.Summary, cache, proof); err != nil {
			return err
		}
	}
	return nil
}

// authorizeJaWSRequest guards the keyed JaWS path before tails or callbacks can run.
func (a *userActionAccess) authorizeJaWSRequest(parent context.Context, sess *jaws.Session, initial *http.Request) bool {
	if a.owner == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if initial.URL.Path == "/auth/actions" {
		entries, ok := initial.Context().Value(userActionPageKey{}).([]action.UserHistoryEntry)
		return ok && a.checkPage(ctx, sess, entries) == nil
	}
	id, err := a.requesterID(sess)
	if err != nil {
		return false
	}
	attempt, ok := strings.CutPrefix(initial.URL.Path, "/auth/actions/")
	if !ok || id == 0 || !workspace.ValidIdentitySegment(attempt) {
		return false
	}
	if !a.admitRead(ctx) {
		return false
	}
	_, summary, found, err := a.owner.UserActionIdentity(attempt, id)
	a.finishRead()
	return err == nil && found && a.checkRepository(ctx, summary) == nil
}

func parseActionCursorQuery(raw string) (string, bool) {
	if len(raw) > 512 {
		return "", false
	}
	values, err := url.ParseQuery(raw)
	if err != nil || len(values) > 1 {
		return "", false
	}
	for key, items := range values {
		if key != "cursor" || len(items) != 1 {
			return "", false
		}
		return items[0], true
	}
	return "", true
}

func (a *oauthAccess) actionRoutes(access *userActionAccess) error {
	if err := a.jaws.AddTemplateLookuper(userActionsHTML); err != nil {
		return err
	}
	owner := access.owner
	githubAvailable := false
	for _, provider := range a.providers {
		if provider.name == "github" {
			githubAvailable = true
			break
		}
	}
	a.mux.HandleFunc("GET /auth/actions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id, err := access.requesterID(a.jaws.GetSession(r))
		if err != nil {
			if errors.Is(err, errUserActionDenied) {
				a.browserError(w, r, http.StatusForbidden)
			} else {
				a.browserError(w, r, http.StatusServiceUnavailable)
			}
			return
		}
		cursor, ok := parseActionCursorQuery(r.URL.RawQuery)
		if !ok {
			a.browserError(w, r, http.StatusBadRequest)
			return
		}
		view := userActionsView{access: access, GitHubAvailable: githubAvailable}
		if id != 0 {
			if !access.admitRead(r.Context()) {
				a.browserError(w, r, http.StatusServiceUnavailable)
				return
			}
			page, err := owner.UserHistory(id, cursor)
			access.finishRead()
			if err != nil {
				if errors.Is(err, action.ErrInvalidHistoryCursor) {
					a.browserError(w, r, http.StatusBadRequest)
					return
				}
				a.browserError(w, r, http.StatusServiceUnavailable)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			view.GitHubLinked = true
			cache := make(map[repositoryProofKey]error)
			proof := &requesterProof{}
			for i, entry := range page.Entries {
				if err := access.checkRepositoryOnce(ctx, entry.Summary, cache, proof); err != nil {
					if errors.Is(err, errUserActionDenied) {
						continue
					}
					a.browserError(w, r, http.StatusServiceUnavailable)
					return
				}
				view.Entries = append(view.Entries, entry)
				if len(view.Entries) == userActionVisiblePageSize {
					if i+1 < len(page.Entries) || page.Next != "" {
						view.Next = base64.RawURLEncoding.EncodeToString([]byte(entry.Cursor))
					}
					break
				}
			}
			if len(view.Entries) < userActionVisiblePageSize {
				view.Next = page.Next
			}
		}
		// JaWS retains this initial request. Recheck only the rows it rendered.
		r = r.WithContext(context.WithValue(r.Context(), userActionPageKey{}, view.Entries))
		ui.Handler(a.jaws, "user actions", view).ServeHTTP(w, r)
	})
	a.mux.HandleFunc("GET /auth/actions/{attempt}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		attempt := r.PathValue("attempt")
		if !workspace.ValidIdentitySegment(attempt) {
			a.browserError(w, r, http.StatusBadRequest)
			return
		}
		id, err := access.requesterID(a.jaws.GetSession(r))
		if err != nil || id == 0 {
			if err != nil && !errors.Is(err, errUserActionDenied) {
				a.browserError(w, r, http.StatusServiceUnavailable)
			} else {
				a.browserError(w, r, http.StatusForbidden)
			}
			return
		}
		if owner == nil {
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		if !access.admitRead(r.Context()) {
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		_, summary, found, err := owner.UserActionIdentity(attempt, id)
		access.finishRead()
		if err != nil {
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		if !found {
			a.browserError(w, r, http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err := access.checkRepository(ctx, summary); err != nil {
			if errors.Is(err, errUserActionDenied) {
				a.browserError(w, r, http.StatusForbidden)
			} else {
				a.browserError(w, r, http.StatusServiceUnavailable)
			}
			return
		}
		detail, found, err := access.projectDetail(ctx, attempt, time.Now().UTC(), summary)
		if err != nil {
			a.browserError(w, r, http.StatusServiceUnavailable)
			return
		}
		if !found {
			a.browserError(w, r, http.StatusNotFound)
			return
		}
		view := actionDetailView{DashboardDetail: detail, AllowUser: true, access: access}
		ui.Handler(a.jaws, "admin action detail", view).ServeHTTP(w, r)
	})
	a.mux.HandleFunc("POST /auth/actions/{attempt}/abort", access.abort)
	return nil
}

// projectDetail limits concurrent private record reads for user detail pages.
func (a *userActionAccess) projectDetail(ctx context.Context, attempt string, now time.Time, summary state.AcceptedSummary) (action.DashboardDetail, bool, error) {
	if a == nil || a.oauth == nil || a.owner == nil || ctx.Err() != nil {
		return action.DashboardDetail{}, false, errUserActionBusy
	}
	select {
	case a.oauth.detailSlots <- struct{}{}:
		defer func() { <-a.oauth.detailSlots }()
		return a.owner.UserActionDetail(attempt, now, summary)
	default:
		return action.DashboardDetail{}, false, errUserActionBusy
	}
}

func (a *userActionAccess) abort(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	attempt := r.PathValue("attempt")
	if !workspace.ValidIdentitySegment(attempt) {
		a.oauth.browserError(w, r, http.StatusBadRequest)
		return
	}
	id, err := a.requesterID(a.oauth.jaws.GetSession(r))
	if err != nil || id == 0 {
		if err != nil && !errors.Is(err, errUserActionDenied) {
			a.oauth.browserError(w, r, http.StatusServiceUnavailable)
		} else {
			a.oauth.browserError(w, r, http.StatusForbidden)
		}
		return
	}
	if a.owner == nil || a.owner.Store == nil {
		a.oauth.browserError(w, r, http.StatusServiceUnavailable)
		return
	}
	if !a.admitRead(r.Context()) {
		a.oauth.browserError(w, r, http.StatusServiceUnavailable)
		return
	}
	jobID, summary, found, err := a.owner.UserActionIdentity(attempt, id)
	a.finishRead()
	if err != nil {
		a.oauth.browserError(w, r, http.StatusServiceUnavailable)
		return
	}
	if !found {
		a.oauth.browserError(w, r, http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := a.checkRepository(ctx, summary); err != nil {
		if errors.Is(err, errUserActionDenied) {
			a.oauth.browserError(w, r, http.StatusForbidden)
		} else {
			a.oauth.browserError(w, r, http.StatusServiceUnavailable)
		}
		return
	}
	_, err = a.owner.Store.RequestStop(jobID, attempt, "github-user:"+strconv.FormatInt(id, 10), time.Now().UTC())
	if errors.Is(err, state.ErrStopConflict) {
		a.oauth.browserError(w, r, http.StatusConflict)
		return
	}
	if err != nil {
		a.oauth.browserError(w, r, http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/auth/actions/"+attempt, http.StatusSeeOther)
}
