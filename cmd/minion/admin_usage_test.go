package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/usage"
)

func TestAdminUsagePageAndJSONReuseCLIAccounting(t *testing.T) {
	handler, _, _, owner := dashboardTestServer(t)
	dir := t.TempDir()
	saveReportFixture(t, dir)
	owner.Usage = &usage.Store{Dir: dir}
	path := "/admin/usage?from=2026-10-09&to=2026-10-10&repo=EXAMPLE-OWNER%2FEXAMPLE-REPO&user=ALICE&service=unknown"
	if denied := adminResponse(handler, http.MethodGet, path, nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous usage page status=%d", denied.Code)
	}
	if denied := adminResponse(handler, http.MethodGet, strings.Replace(path, "/admin/usage?", "/admin/usage.json?", 1), nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous usage export status=%d", denied.Code)
	}
	cookie := dashboardTestCookie(t, handler)
	page := adminResponse(handler, http.MethodGet, path, nil, cookie, "")
	if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" ||
		page.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(page.Body.String(), "To, exclusive (UTC)") ||
		!strings.Contains(page.Body.String(), "Unallocated tokens") ||
		!strings.Contains(page.Body.String(), "Incomplete usage") ||
		!strings.Contains(page.Body.String(), "Billed credits: unknown") ||
		!strings.Contains(page.Body.String(), "example-owner/example-repo") ||
		strings.Contains(page.Body.String(), "example-owner/second-repo") {
		t.Fatalf("filtered usage page: status=%d body=%s", page.Code, page.Body.String())
	}
	exportPath := strings.Replace(path, "/admin/usage?", "/admin/usage.json?", 1)
	export := adminResponse(handler, http.MethodGet, exportPath, nil, cookie, "")
	if export.Code != http.StatusOK || export.Header().Get("Cache-Control") != "no-store" ||
		export.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		export.Header().Get("Content-Disposition") != `attachment; filename="minion-usage.json"` {
		t.Fatalf("usage JSON export: status=%d headers=%v body=%s", export.Code, export.Header(), export.Body.String())
	}
	var got usageOutput
	if err := json.Unmarshal(export.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := reportJSON(t, "-dir", dir, "-from", "2026-10-09", "-to", "2026-10-10", "-repo", "EXAMPLE-OWNER/EXAMPLE-REPO",
		"-user", "ALICE", "-service", "unknown", "-group-by", "repo,user,service")
	if got.Totals != want.Totals || !reflect.DeepEqual(got.Groups, want.Groups) ||
		got.Filters != want.Filters || got.ActionCoverage != "unknown" || got.TokenNote != usageTokenNote ||
		len(got.Attempts) != 1 || got.Attempts[0].SelectedUsage == nil || got.Attempts[0].SelectedUsage.TotalTokens != got.Totals.TotalTokens {
		t.Fatalf("admin export differs from CLI report: got=%+v want=%+v", got, want)
	}
	if got.Totals.IncompleteUsage != 1 || got.Totals.UnallocatedTokens.Total() != 55 {
		t.Fatalf("incomplete/unallocated labels lost: %+v", got.Totals)
	}
	unfiltered := adminResponse(handler, http.MethodGet, "/admin/usage", nil, cookie, "")
	if unfiltered.Code != http.StatusOK || !strings.Contains(unfiltered.Body.String(), "example-owner/second-repo") ||
		strings.Contains(unfiltered.Body.String(), `name="user" value="ALICE"`) {
		t.Fatalf("usage filter leaked to another request: status=%d body=%s", unfiltered.Code, unfiltered.Body.String())
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.Code)
	}
	for _, path := range []string{"/admin/usage", "/admin/usage.json"} {
		if denied := adminResponse(handler, http.MethodGet, path, nil, cookie, ""); denied.Code != http.StatusForbidden {
			t.Fatalf("revoked %s status=%d", path, denied.Code)
		}
	}
}

func TestAdminUsageRejectsBadFiltersAndDamagedRecords(t *testing.T) {
	handler, _, _, owner := dashboardTestServer(t)
	cookie := dashboardTestCookie(t, handler)
	if escaped := adminResponse(handler, http.MethodGet, "/admin/usage?user=%3Cscript%3E", nil, cookie, ""); escaped.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured usage status=%d", escaped.Code)
	}
	owner.Usage = &usage.Store{Dir: t.TempDir()}
	if escaped := adminResponse(handler, http.MethodGet, "/admin/usage?user=%3Cscript%3E", nil, cookie, ""); escaped.Code != http.StatusOK || strings.Contains(escaped.Body.String(), "<script>") ||
		!strings.Contains(escaped.Body.String(), "&lt;script&gt;") {
		t.Fatalf("unescaped usage filter: status=%d body=%s", escaped.Code, escaped.Body.String())
	}
	for _, query := range []string{
		"from=2026-10-10&to=2026-10-09", "from=tomorrow", "service=other", "requester_id=0",
		"repo=a&repo=b", "other=1", "user=%FF", "repo=%00",
	} {
		for _, route := range []string{"/admin/usage?", "/admin/usage.json?"} {
			response := adminResponse(handler, http.MethodGet, route+query, nil, cookie, "")
			if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s%s status=%d", route, query, response.Code)
			}
		}
	}
	dir := t.TempDir()
	owner.Usage = &usage.Store{Dir: dir}
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("a", 32)+".json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/admin/usage", "/admin/usage.json"} {
		response := adminResponse(handler, http.MethodGet, route, nil, cookie, "")
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), dir) {
			t.Fatalf("damaged usage response: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestAdminUsageReadsConcurrentAtomicSaves(t *testing.T) {
	handler, _, _, owner := dashboardTestServer(t)
	dir := t.TempDir()
	saveReportFixture(t, dir)
	owner.Usage = &usage.Store{Dir: dir}
	cookie := dashboardTestCookie(t, handler)
	store := *owner.Usage
	record, found, err := store.Load("00000000000000000000000000000001")
	if err != nil || !found {
		t.Fatalf("load fixture: found=%t err=%v", found, err)
	}
	done := make(chan error, 1)
	go func() {
		for i := int64(0); i < 40; i++ {
			next := record
			next.Tokens.Input = 100 + i
			if err := store.Save(next); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := range 40 {
		for _, route := range []string{"/admin/usage", "/admin/usage.json"} {
			response := adminResponse(handler, http.MethodGet, route, nil, cookie, "")
			if response.Code != http.StatusOK {
				t.Fatalf("concurrent %s status=%d body=%s", route, response.Code, response.Body.String())
			}
		}
		if i%10 == 0 {
			_ = reportJSON(t, "-dir", dir)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
