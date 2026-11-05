package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
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
		!strings.Contains(page.Body.String(), `aria-labelledby="usage-groups-heading" tabindex="0"`) ||
		!strings.Contains(page.Body.String(), `<h2 id="usage-groups-heading">`) ||
		!strings.Contains(page.Body.String(), "To, exclusive (UTC)") ||
		!strings.Contains(page.Body.String(), "attempt start times in UTC") ||
		!strings.Contains(page.Body.String(), `<th scope="col">Unallocated tokens</th>`) ||
		!strings.Contains(page.Body.String(), `<div class="col"><article class="h-100 border rounded p-3"><h2 class="h5">Attempts with incomplete usage or still running</h2>`) ||
		!strings.Contains(page.Body.String(), "token counts in <code>totals</code> and <code>selected_usage</code> reflect that service") ||
		!strings.Contains(page.Body.String(), "outcome counts remain attempt-level") ||
		!strings.Contains(page.Body.String(), "<code>tokens</code> and <code>total_tokens</code> retain all services") ||
		!strings.Contains(page.Body.String(), "Estimated cost and currency") ||
		strings.Count(page.Body.String(), "Unknown (calls with any reported amount: 0; calls with missing or incomplete estimates: 5)") != 2 ||
		!strings.Contains(page.Body.String(), "Billed cost and credits: Unknown") ||
		!strings.Contains(page.Body.String(), "Actions without usage files are excluded") ||
		!strings.Contains(page.Body.String(), "/jaws/") ||
		!strings.Contains(page.Body.String(), "example-owner/example-repo") ||
		strings.Contains(page.Body.String(), "example-owner/second-repo") {
		t.Fatalf("filtered usage page: status=%d body=%s", page.Code, page.Body.String())
	}
	_, href, found := strings.Cut(page.Body.String(), `href="/admin/usage.json?`)
	if !found {
		t.Fatalf("filtered export link missing: %s", page.Body.String())
	}
	encoded, _, found := strings.Cut(href, `"`)
	if !found {
		t.Fatalf("filtered export link is malformed: %s", href)
	}
	exportPath := "/admin/usage.json?" + html.UnescapeString(encoded)
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
		"-user", "ALICE", "-service", "unknown", "-group-by", "repo,repository_id,user,requester_id,service")
	if got.Totals != want.Totals || !reflect.DeepEqual(got.Groups, want.Groups) ||
		got.Filters != want.Filters || got.ActionCoverage != "unknown" || got.TokenNote != usageTokenNote ||
		!strings.Contains(got.TokenNote, "Status counts describe each matching attempt's overall outcome") ||
		len(got.Attempts) != 1 || got.Attempts[0].SelectedUsage == nil || got.Attempts[0].SelectedUsage.TotalTokens != got.Totals.TotalTokens {
		t.Fatalf("admin export differs from CLI report: got=%+v want=%+v", got, want)
	}
	if got.Totals.IncompleteUsage != 1 || got.Totals.UnallocatedTokens.Total() != 55 {
		t.Fatalf("incomplete/unallocated labels lost: %+v", got.Totals)
	}
	unfiltered := adminResponse(handler, http.MethodGet, "/admin/usage", nil, cookie, "")
	if unfiltered.Code != http.StatusOK || !strings.Contains(unfiltered.Body.String(), "example-owner/second-repo") ||
		!strings.Contains(unfiltered.Body.String(), `<th scope="col">Attempts with incomplete usage or still running</th>`) ||
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

func TestAdminUsageConcurrentCLIAndAdminReads(t *testing.T) {
	handler, _, _, owner := dashboardTestServer(t)
	dir := t.TempDir()
	saveReportFixture(t, dir)
	owner.Usage = &usage.Store{Dir: dir}
	cookie := dashboardTestCookie(t, handler)
	record, found, err := owner.Usage.Load("00000000000000000000000000000001")
	if err != nil || !found {
		t.Fatalf("load fixture: found=%t err=%v", found, err)
	}
	firstTotal := reportJSON(t, "-dir", dir).Totals.TotalTokens + 100
	firstSaved := make(chan error, 2)
	firstReads := make(chan struct{}, 2)
	done := make(chan error, 3)
	go func() {
		for i := int64(0); i < 20; i++ {
			next := record
			next.Tokens.Input = record.Tokens.Input + 100 + i
			err := owner.Usage.Save(next)
			if i == 0 {
				firstSaved <- err
				firstSaved <- err
			}
			if err != nil {
				done <- err
				return
			}
			if i == 0 {
				<-firstReads
				<-firstReads
			}
		}
		done <- nil
	}()
	go func() {
		if err := <-firstSaved; err != nil {
			done <- err
			return
		}
		for i := 0; i < 20; i++ {
			path := "/admin/usage.json"
			if i%2 != 0 {
				path = "/admin/usage"
			}
			response := adminResponse(handler, http.MethodGet, path, nil, cookie, "")
			if i == 0 {
				firstReads <- struct{}{}
			}
			if response.Code != http.StatusOK {
				done <- fmt.Errorf("admin read %s: status=%d", path, response.Code)
				return
			}
			if path == "/admin/usage.json" {
				var report usageOutput
				if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil || report.Totals.Attempts != 4 || i == 0 && report.Totals.TotalTokens != firstTotal {
					done <- fmt.Errorf("admin export: attempts=%d tokens=%d: %v", report.Totals.Attempts, report.Totals.TotalTokens, err)
					return
				}
			}
		}
		done <- nil
	}()
	go func() {
		if err := <-firstSaved; err != nil {
			done <- err
			return
		}
		for i := 0; i < 20; i++ {
			var output, diagnostic bytes.Buffer
			status := runUsageWithOutput([]string{"-dir", dir, "-json"}, &output, &diagnostic)
			if i == 0 {
				firstReads <- struct{}{}
			}
			if status != 0 {
				done <- fmt.Errorf("CLI read: status=%d: %s", status, diagnostic.String())
				return
			}
			var report usageOutput
			if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.Totals.Attempts != 4 || i == 0 && report.Totals.TotalTokens != firstTotal {
				done <- fmt.Errorf("CLI report: attempts=%d tokens=%d: %v", report.Totals.Attempts, report.Totals.TotalTokens, err)
				return
			}
		}
		done <- nil
	}()
	var firstErr error
	for range 3 {
		if err := <-done; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		t.Fatal(firstErr)
	}
}

func TestAdminUsageSeparatesReusedNamesByStableIDs(t *testing.T) {
	handler, _, _, owner := dashboardTestServer(t)
	sourceDir := t.TempDir()
	saveReportFixture(t, sourceDir)
	source, found, err := (usage.Store{Dir: sourceDir}).Load("00000000000000000000000000000001")
	if err != nil || !found {
		t.Fatalf("load source usage: found=%t err=%v", found, err)
	}
	owner.Usage = &usage.Store{Dir: t.TempDir()}
	for i, identity := range [][2]int64{{17, 7}, {17, 8}, {18, 7}} {
		record := source
		record.ID = fmt.Sprintf("%032x", i+5)
		record.JobID = fmt.Sprintf("reused-name-%d", i)
		record.RepositoryID, record.RequesterID = identity[0], identity[1]
		if err := owner.Usage.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	cookie := dashboardTestCookie(t, handler)
	page := adminResponse(handler, http.MethodGet, "/admin/usage", nil, cookie, "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Repository ID") ||
		!strings.Contains(page.Body.String(), "Requester ID") || !strings.Contains(page.Body.String(), "<td>18</td>") {
		t.Fatalf("stable identity columns missing: status=%d body=%s", page.Code, page.Body.String())
	}
	export := adminResponse(handler, http.MethodGet, "/admin/usage.json", nil, cookie, "")
	if export.Code != http.StatusOK {
		t.Fatalf("stable identity export: status=%d body=%s", export.Code, export.Body.String())
	}
	var got usageOutput
	if err := json.Unmarshal(export.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode stable identity export: %v", err)
	}
	want := reportJSON(t, "-dir", owner.Usage.Dir, "-group-by", "repo,repository_id,user,requester_id,service")
	if got.Totals != want.Totals || !reflect.DeepEqual(got.Groups, want.Groups) || len(got.Groups) != 3 {
		t.Fatalf("stable identity groups differ from CLI: got=%+v want=%+v", got.Groups, want.Groups)
	}
	pairs := map[[2]int64]bool{{17, 7}: true, {17, 8}: true, {18, 7}: true}
	for _, group := range got.Groups {
		pair := [2]int64{group.RepositoryID, group.RequesterID}
		if !pairs[pair] || group.Attempts != 1 || group.TotalTokens != source.Tokens.Total() {
			t.Fatalf("merged or unexpected usage group: %+v", group)
		}
		delete(pairs, pair)
	}
	if len(pairs) != 0 {
		t.Fatalf("missing identity groups: %v", pairs)
	}
	renamed := source
	renamed.ID, renamed.JobID = fmt.Sprintf("%032x", 9), "renamed-repository"
	renamed.Repo, renamed.RepositoryID, renamed.RequesterID = "example-owner/renamed-repo", 17, 7
	if err := owner.Usage.Save(renamed); err != nil {
		t.Fatal(err)
	}
	page = adminResponse(handler, http.MethodGet, "/admin/usage?repository_id=17", nil, cookie, "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="repository_id" value="17"`) ||
		strings.Contains(page.Body.String(), "<td>18</td>") || !strings.Contains(page.Body.String(), "renamed-repo") ||
		!strings.Contains(page.Body.String(), "Repository and requester ID filters exclude older usage records") {
		t.Fatalf("repository ID page filter: status=%d body=%s", page.Code, page.Body.String())
	}
	page = adminResponse(handler, http.MethodGet, "/admin/usage?requester_id=8", nil, cookie, "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="requester_id" value="8"`) ||
		!strings.Contains(page.Body.String(), "<td>8</td>") || strings.Contains(page.Body.String(), "<td>7</td>") {
		t.Fatalf("requester ID page filter: status=%d body=%s", page.Code, page.Body.String())
	}
	for _, test := range []struct {
		query string
		want  int
	}{
		{"repository_id=17", 3},
		{"repo=example-owner%2Fexample-repo&repository_id=17", 2},
		{"repo=example-owner%2Fexample-repo", 3},
		{"requester_id=7", 3},
		{"requester_id=8", 1},
	} {
		export := adminResponse(handler, http.MethodGet, "/admin/usage.json?"+test.query, nil, cookie, "")
		var selected usageOutput
		if export.Code != http.StatusOK || json.Unmarshal(export.Body.Bytes(), &selected) != nil || selected.Totals.Attempts != test.want {
			t.Fatalf("usage filter %q: status=%d report=%+v body=%s", test.query, export.Code, selected, export.Body.String())
		}
		if test.query == "repository_id=17" &&
			(selected.Filters.RepositoryID != 17 || !strings.Contains(selected.TokenNote, "Repository and requester ID filters exclude older records")) {
			t.Fatalf("repository ID omitted from export: %+v", selected.Filters)
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
		"from=2026-10-10&to=2026-10-09", "from=tomorrow", "to=0001-01-01", "from=0001-01-01T00:00:00Z", "service=other", "requester_id=0", "repository_id=0", "repository_id=bad",
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
