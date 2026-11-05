package main

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/linkdata/jaws/lib/ui"
)

var adminNoticeHTML = template.Must(template.New("admin notice").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Minion admin request error</title>{{$.HeadHTML}}</head>
<body data-bs-theme="dark"><main class="container py-4"><h1>Request could not be completed</h1><p role="alert" class="alert alert-danger">{{.Dot.Status}}</p>
{{if .Auth.IsAdmin}}<a href="/">Dashboard</a>{{else if .Dot.AdminLogin}}<a href="/admin/login">Sign in</a>{{end}}
</main>{{$.TailHTML}}</body></html>`))

type adminNoticeView struct {
	Status     string
	AdminLogin bool
}

func adminBrowserRoute(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/admin/usage.json", "/admin/login.js":
		return false
	}
	return r.URL.Path == "/" || r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/")
}

type adminErrorWriter struct {
	http.ResponseWriter
	status int
}

func (w *adminErrorWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status < http.StatusBadRequest {
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *adminErrorWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= http.StatusBadRequest {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func (w *adminErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type adminNoticeWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	failed bool
}

func (w *adminNoticeWriter) WriteHeader(status int) {
	if w.wrote || w.failed {
		return
	}
	if status >= http.StatusBadRequest {
		w.failed = true
		return
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.wrote = true
}

func (w *adminNoticeWriter) Write(body []byte) (int, error) {
	if w.failed {
		return len(body), nil
	}
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *adminNoticeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *adminAccess) browserErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !adminBrowserRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		capture := &adminErrorWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		if capture.status < http.StatusBadRequest {
			return
		}
		w.Header().Del("Content-Type")
		w.Header().Del("Content-Length")
		w.Header().Set("Cache-Control", "no-store")
		// Serialize the slot check with login pages and other guest notices.
		a.sessionAdmissionMu.Lock()
		defer a.sessionAdmissionMu.Unlock()
		sess := a.jaws.GetSession(r)
		// Leave one request slot for a guest to return to the sign-in page.
		if sess == nil || (a.sessionUsername(sess) == "" && len(sess.Requests()) >= maxGuestLoginRequests-1) {
			w.WriteHeader(capture.status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := &adminNoticeWriter{ResponseWriter: w, status: capture.status}
		ui.Handler(a.jaws, "admin notice", adminNoticeView{Status: http.StatusText(capture.status), AdminLogin: len(a.accounts) != 0}).ServeHTTP(page, r)
		if !page.wrote {
			w.Header().Del("Content-Type")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
}
