package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

func TestSetupHomeRunsWithoutWorkerOrGitHub(t *testing.T) {
	server, err := startAdminServerWithSetup(t.Context(), config.AdminConfig{
		Listen:         "127.0.0.1:0",
		Administrators: []config.AdminAccount{{Username: "admin", PasswordHash: config.AdminPasswordHashPlaceholder}},
	}, nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close setup server: %v", err)
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/", "/admin/login"} {
		response, err := client.Get(server.entryURL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK ||
			!strings.Contains(string(body), "admin password is not set") {
			t.Fatalf("setup page %s: status=%d body=%q read=%v close=%v", path, response.StatusCode, body, readErr, closeErr)
		}
		if path == "/" && !strings.Contains(string(body), "Setup is incomplete") {
			t.Fatalf("setup home lacks next steps: %q", body)
		}
	}
}

func TestSetupAdminCannotOpenOperationalPages(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandlerWithSetupPath(jw, config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
	}, nil, nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	defer func() { jw.Close(); <-done }()
	page := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("setup login page: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"username": {"admin"}, "password": {"correct-password"},
	}, page.Result().Cookies()[0], "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("setup login: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	cookie := login.Result().Cookies()[0]
	for _, path := range []string{"/admin/usage", "/admin/actions/attempt-1", "/admin/status", "/auth/login"} {
		response := adminResponse(handler, http.MethodGet, path, nil, cookie, "")
		if response.Code != http.StatusForbidden {
			t.Errorf("setup %s: status=%d, want 403", path, response.Code)
		}
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), "Setup is incomplete") {
		t.Fatalf("signed-in setup home: status=%d body=%q", home.Code, home.Body.String())
	}
}
