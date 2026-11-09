package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

var detailTestSource = strings.Repeat("a", 40)

func TestEvidenceRows(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"short", 2},
		{"\nleading line", 2},
		{"one\ntwo\nthree", 3},
		{strings.Repeat("line\n", 8) + "last", 8},
	} {
		if got := evidenceRows(tc.text); got != tc.want {
			t.Errorf("evidenceRows(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func detailTestIdentity() map[string]any {
	return map[string]any{"job_id": "job-1", "attempt_id": "attempt-1", "executor_id": "local",
		"repository": "owner/repo", "repository_id": 1, "owner_id": 2, "requester": "writer",
		"requester_id": 3, "number": 7, "subject_type": "Issue"}
}

func detailTestFile(t *testing.T, root, directory, name string, value any) {
	t.Helper()
	path := filepath.Join(root, directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func detailTestRecordFile(t *testing.T, root string, records ...map[string]any) {
	t.Helper()
	path := filepath.Join(root, "attempt-records", "attempt-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func detailTestRecords(t *testing.T, root string, text string, covered bool) {
	t.Helper()
	records := []map[string]any{{"identity": detailTestIdentity(), "source_ref": detailTestSource,
		"sequence": 1, "kind": "report", "text": text, "role": "main", "provider": "codex"}}
	if covered {
		records = append(records, map[string]any{"identity": detailTestIdentity(), "source_ref": detailTestSource,
			"sequence": 2, "kind": "coverage"})
	}
	detailTestRecordFile(t, root, records...)
}

func detailTestReview(t *testing.T, root, stop string, call map[string]any) {
	t.Helper()
	maxRounds, nonblockingRounds := 3, 0
	if stop == "round_cap" {
		maxRounds = 1
	}
	if stop == "nonblocking" {
		nonblockingRounds = 1
	}
	detailTestFile(t, root, "reviews", "attempt-1.json", map[string]any{
		"identity": detailTestIdentity(), "source_ref": detailTestSource,
		"review_claim_origin": "executor_reported", "critic_stop_reason": stop,
		"critic_max_rounds": maxRounds, "critic_nonblocking_rounds": nonblockingRounds,
		"calls": []any{detailTestMain(1), call}, "refutations": []any{},
	})
}

func detailTestMain(round int) map[string]any {
	return map[string]any{"provider": "codex", "role": "main", "round": round, "complete": true}
}

func detailTestCall(approved, noBlockers, hasBlockers, complete bool) map[string]any {
	return map[string]any{"provider": "claude", "role": "critic", "round": 1,
		"review_phase": "full", "approved": approved, "no_blockers": noBlockers,
		"has_blockers": hasBlockers, "review_complete": complete, "complete": true}
}

func detailTestActive(t *testing.T) (http.Handler, string, *http.Cookie) {
	t.Helper()
	handler, _, store, owner := dashboardTestServer(t)
	owner.WorkRoot = t.TempDir()
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	return handler, owner.WorkRoot, dashboardTestCookie(t, handler)
}

func detailTestPublication(status string, at time.Time) state.PublicationWrite {
	write := state.PublicationWrite{AttemptID: "attempt-1", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
		Repository: "owner/repo", Number: 7, Marker: "<!-- linkdataminion:job-1 -->",
		Since: at, StartedAt: at, Deadline: at.Add(time.Minute), Status: status}
	if status != state.PublicationWritePending {
		write.ResolvedAt = at.Add(time.Second)
	}
	return write
}

func TestAdminActionDetailAuthorizedEscapedAndBounded(t *testing.T) {
	handler, _, store, owner := dashboardTestServer(t)
	owner.WorkRoot = t.TempDir()
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	hostile := `</textarea><script>alert("owned")</script><img src=x onerror=alert(1)>`
	detailTestRecords(t, owner.WorkRoot, hostile+strings.Repeat("x", 15<<10), true)
	detailTestReview(t, owner.WorkRoot, "approved", detailTestCall(true, true, false, true))
	detailTestFile(t, owner.WorkRoot, "worker-diagnostics", "attempt-1.json", map[string]any{
		"identity": detailTestIdentity(), "source_ref": detailTestSource,
		"worker_reported_diagnostics": []any{map[string]any{"code": "worker", "message": hostile + "\nsecond line"}},
	})
	if denied := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous detail status=%d", denied.Code)
	}
	cookie := dashboardTestCookie(t, handler)
	dashboard := adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(dashboard.Body.String(), `/admin/actions/attempt-1`) {
		t.Fatalf("selected action has no detail link: %s", dashboard.Body.String())
	}
	view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	body := view.Body.String()
	for _, want := range []string{"Critic approved; no blockers reported", "Stopped after approval", "Received record coverage marker is present", "text truncated", "Worker diagnostics are executor-reported", "Original deadline", "Remaining budget", "&lt;script&gt;", `<dl`, `<textarea`, `action="/admin/logout"`, `href="/">← Live actions`, `readonly aria-labelledby="record-caption-0"`, `id="record-caption-0"`, `>Received report`, `id="diagnostic-caption-0"`, `>Worker diagnostic`, `aria-labelledby="diagnostic-caption-0"`, "\nsecond line"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail missing %q: %s", want, body)
		}
	}
	if view.Code != http.StatusOK || view.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(body, `<meta name="jawsKey"`) || !strings.Contains(body, `/jaws/.tail/`) ||
		strings.Contains(body, `<script>alert`) || strings.Contains(body, "<img src=x") || len(body) > 20<<10 {
		t.Fatalf("detail not safe and bounded: status=%d bytes=%d body=%s", view.Code, len(body), body)
	}
}

func TestAdminActionDetailPreservesInitialEvidenceNewline(t *testing.T) {
	handler, root, cookie := detailTestActive(t)
	const evidence = "\nleading line"
	detailTestRecords(t, root, evidence, true)
	detailTestFile(t, root, "reviews", "attempt-1.json", map[string]any{
		"identity": detailTestIdentity(), "source_ref": detailTestSource, "review_claim_origin": "executor_reported",
		"critic_max_rounds": 3,
		"calls":             []any{detailTestMain(1), detailTestCall(false, false, true, true), detailTestMain(2)},
		"refutations":       []any{map[string]any{"round": 2, "text": evidence}},
	})
	detailTestFile(t, root, "worker-diagnostics", "attempt-1.json", map[string]any{
		"identity": detailTestIdentity(), "source_ref": detailTestSource,
		"worker_reported_failure":     map[string]any{"message": evidence, "observed_message": evidence},
		"worker_reported_diagnostics": []any{map[string]any{"code": "worker", "message": evidence}},
	})
	view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	if view.Code != http.StatusOK {
		t.Fatalf("detail status=%d: %s", view.Code, view.Body.String())
	}
	for _, label := range []string{
		`aria-labelledby="refutation-caption-0"`, `aria-labelledby="record-caption-0"`,
		`aria-label="Worker-reported failure"`, `aria-label="Worker-observed failure"`,
		`aria-labelledby="diagnostic-caption-0"`,
	} {
		if want := `rows="2" readonly ` + label + ">\n\nleading line</textarea>"; !strings.Contains(view.Body.String(), want) {
			t.Errorf("detail missing initial evidence newline after %s: %s", label, view.Body.String())
		}
	}
}

func TestAdminActionDetailConnectedPageRevoked(t *testing.T) {
	for _, revoke := range []string{"logout", "replace"} {
		t.Run(revoke, func(t *testing.T) {
			store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			owner := &action.Orchestrator{Store: store, WorkRoot: t.TempDir()}
			server, err := startAdminServer(t.Context(), config.AdminConfig{
				Listen: "127.0.0.1:0", Administrators: []config.AdminAccount{testAdminAccount()},
			}, owner, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			baseURL := "http://" + server.listener.Addr().String()
			client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}}
			send := func(method, path string, form url.Values, cookie *http.Cookie) (int, string, []*http.Cookie) {
				var body io.Reader
				if form != nil {
					body = strings.NewReader(form.Encode())
				}
				request, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, body)
				if err != nil {
					t.Fatal(err)
				}
				if form != nil {
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				if method == http.MethodPost {
					request.Header.Set("Origin", baseURL)
				}
				if cookie != nil {
					request.AddCookie(cookie)
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				return response.StatusCode, string(data), response.Cookies()
			}
			status, _, cookies := send(http.MethodGet, "/admin/login", nil, nil)
			if status != http.StatusOK || len(cookies) != 1 {
				t.Fatalf("guest login: status=%d cookies=%d", status, len(cookies))
			}
			form := url.Values{"password": {"correct-password"}}
			status, _, cookies = send(http.MethodPost, "/admin/login", form, cookies[0])
			if status != http.StatusSeeOther || len(cookies) != 1 {
				t.Fatalf("admin login: status=%d cookies=%d", status, len(cookies))
			}
			adminCookie := cookies[0]
			status, page, _ := send(http.MethodGet, "/admin/actions/attempt-1", nil, adminCookie)
			if status != http.StatusOK || !strings.Contains(page, "owner/repo") {
				t.Fatalf("action detail: status=%d page=%q", status, page)
			}
			key := adminJaWSKey(t, page)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			headers := http.Header{}
			headers.Set("Cookie", adminCookie.Name+"="+adminCookie.Value)
			headers.Set("Origin", baseURL)
			conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(baseURL, "http")+"/jaws/"+key,
				&websocket.DialOptions{HTTPHeader: headers})
			if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("detail WebSocket: response=%v error=%v", response, err)
			}
			defer conn.CloseNow()
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				for {
					if _, _, err := conn.Read(ctx); err != nil {
						return
					}
				}
			}()
			if err := conn.Ping(ctx); err != nil {
				t.Fatalf("detail WebSocket closed before revocation: %v", err)
			}
			if revoke == "logout" {
				status, _, _ = send(http.MethodPost, "/admin/logout", nil, adminCookie)
			} else {
				status, _, cookies = send(http.MethodPost, "/admin/login", form, adminCookie)
				if len(cookies) != 1 {
					t.Fatalf("replacement cookies=%d", len(cookies))
				}
			}
			if status != http.StatusSeeOther {
				t.Fatalf("revoke status=%d", status)
			}
			if status, _, _ := send(http.MethodGet, "/jaws/.tail/"+key, nil, adminCookie); status != http.StatusForbidden {
				t.Fatalf("old detail key status=%d", status)
			}
			if revoke == "replace" {
				if status, _, _ := send(http.MethodGet, "/jaws/.tail/"+key, nil, cookies[0]); status != http.StatusForbidden {
					t.Fatalf("new session claimed old detail key: status=%d", status)
				}
			}
			select {
			case <-readDone:
			case <-ctx.Done():
				t.Fatal("revoked detail WebSocket stayed connected")
			}
		})
	}
}

func TestAdminActionDetailRetainedTerminalVerdictAndPublication(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, stop, publication, wantVerdict, wantStop, wantPublication string
		call                                                                     map[string]any
	}{
		{"capped but published", "succeeded", "round_cap", state.PublicationWriteObserved,
			"Critic reported blockers", "Critic round limit reached (1)", "Observed on GitHub", detailTestCall(false, false, true, true)},
		{"failed publication", "failed", "", state.PublicationWriteUncertain,
			"Critic approved; no blockers reported", "No final stopping decision saved", "Uncertain; publication not confirmed", detailTestCall(true, true, false, true)},
		{"rejected", "failed", "", "", "Critic reported blockers", "No final stopping decision saved", "No confirmed publication checkpoint", detailTestCall(false, false, true, true)},
		{"incomplete", "failed", "", "", "Last full critic review incomplete", "No final stopping decision saved", "No confirmed publication checkpoint", detailTestCall(false, false, false, false)},
		{"normal no-blocker stop", "succeeded", "nonblocking", state.PublicationWriteObserved,
			"Critic reported no blockers without approval", "Stopped after configured no-blocker rounds", "Observed on GitHub", detailTestCall(false, true, false, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, store, owner := dashboardTestServer(t)
			owner.WorkRoot = t.TempDir()
			at := time.Now().UTC().Truncate(time.Second)
			store.Data.Accepted = dashboardTestAccepted(at)
			original := store.Data.Accepted.Deadline
			if _, err := store.DecideTerminalLocked("job-1", "attempt-1", tc.outcome, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			store.Data.Accepted = nil
			if tc.publication != "" {
				store.Data.PublicationWrites["job-1"] = detailTestPublication(tc.publication, at)
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			detailTestRecords(t, owner.WorkRoot, "received report", true)
			detailTestReview(t, owner.WorkRoot, tc.stop, tc.call)
			cookie := dashboardTestCookie(t, handler)
			view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
			for _, want := range []string{tc.wantVerdict, tc.wantStop, tc.wantPublication, tc.outcome,
				original.Format(time.RFC3339), "No longer running"} {
				if !strings.Contains(view.Body.String(), want) {
					t.Errorf("terminal detail missing %q: %s", want, view.Body.String())
				}
			}
			if view.Code != http.StatusOK {
				t.Fatalf("terminal detail status=%d", view.Code)
			}
		})
	}
}

func TestAdminActionDetailBindsRetainedEvidenceAfterAcceptedCleanup(t *testing.T) {
	_, _, store, owner := dashboardTestServer(t)
	owner.WorkRoot = t.TempDir()
	at := time.Now().UTC().Truncate(time.Second)
	store.Data.Accepted = dashboardTestAccepted(at)
	if _, err := store.DecideTerminalLocked("job-1", "attempt-1", "failed", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	writeEvidence := func(identity map[string]any) {
		detailTestRecordFile(t, owner.WorkRoot, map[string]any{
			"identity": identity, "source_ref": detailTestSource, "sequence": 1,
			"kind": "report", "text": "PRIVATE_RECORD"})
		detailTestFile(t, owner.WorkRoot, "reviews", "attempt-1.json", map[string]any{
			"identity": identity, "source_ref": detailTestSource, "review_claim_origin": "executor_reported",
			"critic_stop_reason": "approved", "critic_max_rounds": 3,
			"calls": []any{detailTestMain(1), detailTestCall(true, true, false, true)}, "refutations": []any{},
		})
		detailTestFile(t, owner.WorkRoot, "worker-diagnostics", "attempt-1.json", map[string]any{
			"identity": identity, "source_ref": detailTestSource,
			"worker_reported_diagnostics": []any{map[string]any{"code": "worker", "message": "PRIVATE_WORKER"}},
		})
	}
	caseChanged := detailTestIdentity()
	caseChanged["requester"] = "WRITER"
	writeEvidence(caseChanged)
	for _, cleared := range []bool{false, true} {
		if cleared {
			store.Data.Accepted = nil
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
		}
		got, found, err := owner.DashboardActionDetail("attempt-1", at)
		if err != nil || !found || len(got.Records) != 1 || got.CriticVerdict == "" || len(got.Diagnostics) != 1 {
			t.Fatalf("case-only requester change rejected with accepted cleared=%t: %+v found=%t err=%v", cleared, got, found, err)
		}
	}
	writeEvidence(detailTestIdentity())
	detail, found, err := owner.DashboardActionDetail("attempt-1", at)
	if err != nil || !found || detail.JobID != "job-1" || detail.Summary.RequesterID != 3 || detail.Summary.RepositoryID != 1 ||
		detail.Summary.OwnerID != 2 || detail.Repository != "owner/repo" || detail.Requester != "writer" ||
		len(detail.Records) != 1 || detail.Records[0].Text != "PRIVATE_RECORD" ||
		!strings.Contains(detail.CriticVerdict, "Critic approved") || len(detail.Diagnostics) != 1 {
		t.Fatalf("retained summary did not bind valid evidence: %+v found=%t err=%v", detail, found, err)
	}
	for _, test := range []struct {
		name  string
		value any
	}{
		{"requester_id", 44}, {"repository_id", 99}, {"owner_id", 99},
		{"requester", "other"}, {"repository", "other/repo"}, {"executor_id", "foreign"},
		{"subject_type", "PullRequest"}, {"number", 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := detailTestIdentity()
			identity[test.name] = test.value
			writeEvidence(identity)
			got, found, err := owner.DashboardActionDetail("attempt-1", at)
			if err != nil || !found || got.Summary.RequesterID != 3 || got.Summary.RepositoryID != 1 ||
				len(got.Records) != 0 || got.CriticVerdict != "" || len(got.Diagnostics) != 0 ||
				!strings.Contains(got.RecordsNotice, "invalid") ||
				!strings.Contains(got.ReviewNotice, "invalid") || !strings.Contains(got.WorkerNotice, "invalid") {
				t.Fatalf("mismatched retained evidence was projected: %+v found=%t err=%v", got, found, err)
			}
		})
	}
}

func TestAdminActionDetailMissingPartialAndLegacyEvidence(t *testing.T) {
	handler, _, store, owner := dashboardTestServer(t)
	owner.WorkRoot = t.TempDir()
	store.Data.TerminalDecisions["job-1"] = state.TerminalDecision{AttemptID: "attempt-1", Outcome: "failed", DecidedAt: time.Now().UTC()}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	detailTestRecords(t, owner.WorkRoot, "received prefix", false)
	cookie := dashboardTestCookie(t, handler)
	view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	for _, want := range []string{"Unavailable in older saved decision", "coverage is unconfirmed", "received prefix", "No saved record available", "No confirmed publication checkpoint"} {
		if !strings.Contains(view.Body.String(), want) {
			t.Errorf("partial detail missing %q: %s", want, view.Body.String())
		}
	}
	if view.Code != http.StatusOK {
		t.Fatalf("partial detail status=%d", view.Code)
	}
	if missing := adminResponse(handler, http.MethodGet, "/admin/actions/unknown", nil, cookie, ""); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown attempt status=%d", missing.Code)
	}
	foreign := detailTestIdentity()
	foreign["job_id"] = "another-job"
	detailTestFile(t, owner.WorkRoot, "worker-diagnostics", "attempt-1.json", map[string]any{
		"identity": foreign, "source_ref": detailTestSource,
		"worker_reported_diagnostics": []any{map[string]any{"code": "secret", "message": "FOREIGN PRIVATE TEXT"}},
	})
	if err := os.MkdirAll(filepath.Join(owner.WorkRoot, "reviews"), 0700); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(owner.WorkRoot, "reviews", "attempt-1.json")
	if err := os.WriteFile(oversize, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversize, (64<<20)+1); err != nil {
		t.Fatal(err)
	}
	view = adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	if !strings.Contains(view.Body.String(), "64 MiB read limit") ||
		!strings.Contains(view.Body.String(), "Saved worker diagnostics unavailable or invalid") ||
		strings.Contains(view.Body.String(), "FOREIGN PRIVATE TEXT") {
		t.Fatalf("oversized or mismatched private record was exposed: %s", view.Body.String())
	}
}

func TestAdminActionDetailLabelsUnterminatedRecordTail(t *testing.T) {
	handler, root, cookie := detailTestActive(t)
	detailTestRecords(t, root, "received prefix", false)
	path := filepath.Join(root, "attempt-records", "attempt-1.jsonl")
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := json.Marshal(map[string]any{"identity": detailTestIdentity(), "source_ref": detailTestSource,
		"sequence": 2, "kind": "discussion", "text": "UNTERMINATED TAIL"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(prior, tail...), 0600); err != nil {
		t.Fatal(err)
	}
	view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	body := view.Body.String()
	if view.Code != http.StatusOK || !strings.Contains(body, "received prefix") ||
		!strings.Contains(body, "Saved record ends with an incomplete line; only an earlier received prefix is shown") ||
		strings.Contains(body, "UNTERMINATED TAIL") || strings.Contains(body, "display is truncated") {
		t.Fatalf("unterminated tail was mislabeled or exposed: status=%d body=%s", view.Code, body)
	}
}

func TestAdminActionDetailPreservesEarlierVerdictAndLabelsCaptureStop(t *testing.T) {
	firstFull := detailTestCall(false, true, false, true)
	for _, tc := range []struct {
		name  string
		calls []any
		want  []string
	}{
		{"later full review incomplete", []any{detailTestMain(1), firstFull, detailTestMain(2),
			map[string]any{"provider": "claude", "role": "critic", "round": 2,
				"review_phase": "verification", "no_blockers": true, "review_complete": true, "complete": true},
			map[string]any{"provider": "claude", "role": "critic", "round": 2,
				"review_phase": "full", "has_blockers": true, "complete": true}},
			[]string{"Round 1 full review: Critic reported no blockers without approval", "round 2 verification: Critic reported no blockers without approval", "later full review incomplete"}},
		{"blocking verification", []any{detailTestMain(1), firstFull, detailTestMain(2),
			map[string]any{"provider": "claude", "role": "critic", "round": 2,
				"review_phase": "verification", "has_blockers": true, "review_complete": true, "complete": true}},
			[]string{"round 2 verification: Critic reported blockers; later work has no completed full review"}},
		{"main revision", []any{detailTestMain(1), firstFull, detailTestMain(2)},
			[]string{"Round 1 full review: Critic reported no blockers without approval; later work has no completed full review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, root, cookie := detailTestActive(t)
			detailTestFile(t, root, "reviews", "attempt-1.json", map[string]any{
				"identity": detailTestIdentity(), "source_ref": detailTestSource,
				"review_claim_origin": "executor_reported", "critic_max_rounds": 3, "calls": tc.calls,
			})
			detailTestRecordFile(t, root,
				map[string]any{"identity": detailTestIdentity(), "source_ref": detailTestSource, "sequence": 1, "kind": "report", "text": "received prefix"},
				map[string]any{"identity": detailTestIdentity(), "source_ref": detailTestSource, "sequence": 2, "kind": "stop", "text": "capture_limit"},
			)
			view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
			for _, want := range append(tc.want, "Capture limit reached; received record is truncated", "received prefix") {
				if !strings.Contains(view.Body.String(), want) {
					t.Errorf("detail missing %q: %s", want, view.Body.String())
				}
			}
		})
	}
}

func TestAdminActionDetailReadsAdmittedSymlinkWorkRoot(t *testing.T) {
	handler, _, store, owner := dashboardTestServer(t)
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "work-root")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	owner.WorkRoot = link
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	detailTestRecords(t, root, "saved through canonical root", true)
	detailTestReview(t, root, "approved", detailTestCall(true, true, false, true))
	detailTestFile(t, root, "worker-diagnostics", "attempt-1.json", map[string]any{
		"identity": detailTestIdentity(), "source_ref": detailTestSource,
		"worker_reported_diagnostics": []any{map[string]any{"code": "worker", "message": "retained worker note"}},
	})
	cookie := dashboardTestCookie(t, handler)
	view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	for _, want := range []string{"saved through canonical root", "Critic approved; no blockers reported", "retained worker note"} {
		if !strings.Contains(view.Body.String(), want) {
			t.Errorf("symlink-root detail missing %q: %s", want, view.Body.String())
		}
	}
}

func TestAdminActionDetailShowsKnownGapsAndBothTruncationLimits(t *testing.T) {
	for _, tc := range []struct{ kind, code, label string }{
		{"discussion", "content_missing", "Content missing from received event"},
		{"diagnostic", "diagnostics_truncated", "Provider diagnostics truncated before receipt"},
		{"diagnostic", "call.diagnostic_unavailable", "Provider call diagnostic unavailable"},
		{"diagnostic", "stderr_unavailable", "Provider stderr unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			handler, root, cookie := detailTestActive(t)
			detailTestRecordFile(t, root, map[string]any{
				"identity": detailTestIdentity(), "source_ref": detailTestSource, "sequence": 1,
				"kind": tc.kind, "provider": "codex", "call": "1", "agent": "native",
				"event_source": "codex." + tc.code,
			})
			view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
			if !strings.Contains(view.Body.String(), tc.label) || !strings.Contains(view.Body.String(), "coverage is unconfirmed") {
				t.Fatalf("known gap %s was hidden or claimed complete: %s", tc.code, view.Body.String())
			}
		})
	}
	t.Run("redacted prepared input", func(t *testing.T) {
		handler, root, cookie := detailTestActive(t)
		detailTestRecordFile(t, root, map[string]any{
			"identity": detailTestIdentity(), "source_ref": detailTestSource, "sequence": 1,
			"kind": "input", "provider": "codex", "call": "1", "event_source": "codex.input.prepared",
			"text": `{"representation":"redacted","stage":"prepared","available":false,"reason":"redaction_unavailable"}`,
		})
		view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
		if !strings.Contains(view.Body.String(), "redaction_unavailable") || !strings.Contains(view.Body.String(), "coverage is unconfirmed") {
			t.Fatalf("unavailable prepared input was hidden or claimed complete: %s", view.Body.String())
		}
	})
	t.Run("capture and display limits", func(t *testing.T) {
		handler, root, cookie := detailTestActive(t)
		records := make([]map[string]any, 0, 66)
		for i := 1; i <= 65; i++ {
			records = append(records, map[string]any{"identity": detailTestIdentity(), "source_ref": detailTestSource,
				"sequence": i, "kind": "discussion", "text": "received text"})
		}
		records = append(records, map[string]any{"identity": detailTestIdentity(), "source_ref": detailTestSource,
			"sequence": 66, "kind": "stop", "text": "capture_limit"})
		detailTestRecordFile(t, root, records...)
		view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
		if !strings.Contains(view.Body.String(), "Capture limit reached; received record is truncated; display is also truncated") {
			t.Fatalf("capture and view limits were collapsed: %s", view.Body.String())
		}
	})
}

func TestAdminActionDetailKeepsProvenanceOnTruncatedEvidence(t *testing.T) {
	t.Run("metadata-only review and clipped worker note", func(t *testing.T) {
		handler, root, cookie := detailTestActive(t)
		detailTestFile(t, root, "reviews", "attempt-1.json", map[string]any{
			"identity": detailTestIdentity(), "source_ref": detailTestSource, "critic_max_rounds": 3, "calls": []any{},
		})
		detailTestFile(t, root, "worker-diagnostics", "attempt-1.json", map[string]any{
			"identity": detailTestIdentity(), "source_ref": detailTestSource,
			"worker_reported_diagnostics": []any{map[string]any{"code": "worker", "message": strings.Repeat("x", 5<<10)}},
		})
		view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
		for _, want := range []string{"No critic calls saved yet", "Worker diagnostics are executor-reported; display is truncated", "worker · worker-reported"} {
			if !strings.Contains(view.Body.String(), want) {
				t.Errorf("truncated detail missing %q: %s", want, view.Body.String())
			}
		}
	})
	t.Run("clipped refutation", func(t *testing.T) {
		handler, root, cookie := detailTestActive(t)
		detailTestFile(t, root, "reviews", "attempt-1.json", map[string]any{
			"identity": detailTestIdentity(), "source_ref": detailTestSource, "review_claim_origin": "executor_reported",
			"critic_max_rounds": 3,
			"calls":             []any{detailTestMain(1), detailTestCall(false, false, true, true), detailTestMain(2)},
			"refutations":       []any{map[string]any{"round": 2, "text": strings.Repeat("<", 5<<10)}},
		})
		view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
		if !strings.Contains(view.Body.String(), "Saved critic calls are executor-reported and validated at receipt; main-model refutations are truncated in this view") ||
			!strings.Contains(view.Body.String(), `id="refutation-caption-0"`) ||
			!strings.Contains(view.Body.String(), `>Main-model refutation`) ||
			!strings.Contains(view.Body.String(), `aria-labelledby="refutation-caption-0"`) {
			t.Fatalf("truncated refutation lost provenance: %s", view.Body.String())
		}
	})
	t.Run("large valid worker record", func(t *testing.T) {
		handler, root, cookie := detailTestActive(t)
		diagnostics := make([]any, 100)
		for i := range diagnostics {
			diagnostics[i] = map[string]any{"code": "worker", "message": strings.Repeat("<", 8<<10)}
		}
		detailTestFile(t, root, "worker-diagnostics", "attempt-1.json", map[string]any{
			"identity": detailTestIdentity(), "source_ref": detailTestSource,
			"worker_reported_diagnostics": diagnostics,
		})
		if info, err := os.Stat(filepath.Join(root, "worker-diagnostics", "attempt-1.json")); err != nil || info.Size() <= 4<<20 {
			t.Fatalf("large valid worker fixture did not cross old read cap: info=%v err=%v", info, err)
		}
		view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
		if !strings.Contains(view.Body.String(), "Worker diagnostics are executor-reported; display is truncated") ||
			strings.Contains(view.Body.String(), "Saved worker diagnostics unavailable") || len(view.Body.String()) > 400<<10 {
			t.Fatalf("large valid worker record was hidden or unbounded: bytes=%d", len(view.Body.String()))
		}
	})
}

func TestAdminActionDetailReadsLargeValidReview(t *testing.T) {
	handler, _, store, owner := dashboardTestServer(t)
	owner.WorkRoot = t.TempDir()
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	calls := []any{
		map[string]any{"provider": "codex", "role": "main", "round": 1, "complete": true},
		detailTestCall(false, false, true, true),
	}
	refutations := make([]any, 0, 19)
	for round := 2; round <= 20; round++ {
		calls = append(calls,
			map[string]any{"provider": "codex", "role": "main", "round": round, "complete": true},
			map[string]any{"provider": "claude", "role": "critic", "round": round, "review_phase": "verification", "approved": true, "no_blockers": true, "review_complete": true, "complete": true},
			map[string]any{"provider": "claude", "role": "critic", "round": round, "review_phase": "full", "approved": round == 20, "no_blockers": round == 20, "has_blockers": round < 20, "review_complete": true, "complete": true},
		)
		refutations = append(refutations, map[string]any{"round": round, "text": strings.Repeat("<", 58<<10)})
	}
	detailTestFile(t, owner.WorkRoot, "reviews", "attempt-1.json", map[string]any{
		"identity": detailTestIdentity(), "source_ref": detailTestSource, "review_claim_origin": "executor_reported",
		"critic_stop_reason": "approved", "critic_max_rounds": 20, "calls": calls, "refutations": refutations,
	})
	if info, err := os.Stat(filepath.Join(owner.WorkRoot, "reviews", "attempt-1.json")); err != nil || info.Size() <= 4<<20 {
		t.Fatalf("large valid review fixture did not cross old read cap: info=%v err=%v", info, err)
	}
	cookie := dashboardTestCookie(t, handler)
	view := adminResponse(handler, http.MethodGet, "/admin/actions/attempt-1", nil, cookie, "")
	if !strings.Contains(view.Body.String(), "Round 20 full review: Critic approved; no blockers reported") ||
		!strings.Contains(view.Body.String(), "Saved critic calls are executor-reported and validated at receipt; main-model refutations are truncated in this view") ||
		strings.Contains(view.Body.String(), "Saved critic evidence unavailable") || len(view.Body.String()) > 400<<10 {
		t.Fatalf("large valid review was hidden or unbounded: bytes=%d", len(view.Body.String()))
	}
}
