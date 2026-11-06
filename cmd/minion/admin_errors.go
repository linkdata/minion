package main

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/linkdata/jaws/lib/ui"
)

var adminNoticeHTML = template.Must(template.New("admin notice").Parse(headHTML + `{{template "head.html" $}}<title>Minion admin request error</title></head>
<body><main class="container py-4"><h1>Request could not be completed</h1><p role="alert" class="alert alert-danger">{{.Dot.Status}}</p>
{{if .Dot.AdminLogin}}<a href="/admin/login">Admin sign in</a>{{end}}
<a href="/">Home</a>
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
	body   []byte
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
		if w.status == http.StatusConflict && len(w.body) < 512 {
			w.body = append(w.body, body[:min(len(body), 512-len(w.body))]...)
		}
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
		sess := a.jaws.GetSession(r)
		if sess == nil {
			w.WriteHeader(capture.status)
			return
		}
		// Leave one request slot for a guest to return to the sign-in page.
		if a.sessionUsername(sess) == "" {
			if account, _ := a.oauth.currentUser(sess); account == "" {
				// Keep guest admission and JaWS registration in one critical section.
				a.noticeMu.Lock()
				defer a.noticeMu.Unlock()
				if len(sess.Requests()) >= maxGuestPageRequests-1 {
					w.WriteHeader(capture.status)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		status := http.StatusText(capture.status)
		if capture.status == http.StatusConflict && r.Method == http.MethodPost && a.sessionUsername(sess) != "" {
			if reason := strings.TrimSpace(string(capture.body)); reason != "" {
				status = reason
			}
		}
		page := &adminNoticeWriter{ResponseWriter: w, status: capture.status}
		ui.Handler(a.jaws, "admin notice", adminNoticeView{Status: status, AdminLogin: len(a.accounts) != 0 && a.sessionUsername(sess) == ""}).ServeHTTP(page, r)
		if !page.wrote {
			w.Header().Del("Content-Type")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
}
