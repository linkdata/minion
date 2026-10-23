package usage

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testRecord() Record {
	started := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	return Record{
		ID: "0123456789abcdef0123456789abcdef", JobID: "github:123", Repo: "linkdata/minion", User: "example",
		Action: "review", Number: 1, Provider: "codex", Model: "example-model", StartedAt: started, FinishedAt: &finished,
		Status: "succeeded", CodexCalls: 1, Tokens: Tokens{Input: 100, CachedInput: 80, Output: 20}, UsageComplete: true,
		ExecutorID: "local", SourceRef: strings.Repeat("a", 40), UsageSource: "executor_reported",
	}
}

func TestStoreAtomicUpdates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	store := Store{Dir: dir}
	record := testRecord()
	record.Status, record.FinishedAt = "running", nil
	record.CodexCalls, record.Tokens = 0, Tokens{}
	record.UsageSource = ""
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, record.ID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("record permissions: info=%v err=%v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".record-interrupted"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 30; i++ {
			record.CodexCalls = i
			record.Tokens = Tokens{Input: int64(i), CachedInput: int64(i / 2), Output: int64(i * 2)}
			record.UsageSource = "executor_reported"
			if err := store.Save(record); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		records, err := ReadAll(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].Tokens.Input != int64(records[0].CodexCalls) || records[0].Tokens.Output != 2*records[0].Tokens.Input {
			t.Fatalf("partial or duplicate record: %+v", records)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			records, err := ReadAll(dir)
			if err != nil || len(records) != 1 || records[0].CodexCalls != 30 {
				t.Fatalf("final record=%+v err=%v", records, err)
			}
			return
		default:
		}
	}
}

func TestStoreAllowsInitialAndPendingUsage(t *testing.T) {
	for _, phase := range []string{"initial", "pending", "failed pending"} {
		t.Run(phase, func(t *testing.T) {
			record := testRecord()
			record.Status, record.FinishedAt = "running", nil
			record.CodexCalls, record.Tokens = 0, Tokens{}
			record.ExecutorID, record.SourceRef, record.UsageSource = "", "", ""
			if phase != "initial" {
				record.CodexCalls = 1
				record.ExecutorID, record.SourceRef = "local", strings.Repeat("a", 40)
				record.UsageComplete = false
			}
			if phase == "failed pending" {
				record.Status = "failed"
				finished := record.StartedAt.Add(time.Second)
				record.FinishedAt = &finished
			}
			dir := t.TempDir()
			if err := (Store{Dir: dir}).Save(record); err != nil {
				t.Fatal(err)
			}
			got, err := ReadAll(dir)
			if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], record) {
				t.Fatalf("usage state changed: records=%+v err=%v", got, err)
			}
		})
	}
}

func TestReadAllMissingDirectory(t *testing.T) {
	records, err := ReadAll(filepath.Join(t.TempDir(), "missing"))
	if err != nil || records == nil || len(records) != 0 {
		t.Fatalf("records=%v err=%v", records, err)
	}
}

func TestSaveEncodeAndRenameFailuresKeepDirectoryClean(t *testing.T) {
	for _, failure := range []string{"no write on encode error", "rename"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			record := testRecord()
			if failure == "no write on encode error" {
				dir = filepath.Join(dir, "not-created")
				record.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				record.Status, record.FinishedAt = "running", nil
			} else {
				if err := os.Mkdir(filepath.Join(dir, record.ID+".json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := (Store{Dir: dir}).Save(record)
			var marshalErr *json.MarshalerError
			var renameErr *os.LinkError
			if failure == "no write on encode error" && !errors.As(err, &marshalErr) || failure == "rename" && !errors.As(err, &renameErr) {
				t.Fatalf("Save() = %v; want %s error", err, failure)
			}
			if failure == "no write on encode error" {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("usage directory created before encoding: %v", err)
				}
				return
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("temporary usage file retained: %v, %v", files, err)
			}
		})
	}
}

func TestSaveRejectsInvalidRecords(t *testing.T) {
	for name, modify := range map[string]func(*Record){
		"unsafe ID":        func(r *Record) { r.ID = "../elsewhere" },
		"uppercase ID":     func(r *Record) { r.ID = strings.ToUpper(r.ID) },
		"missing metadata": func(r *Record) { r.User = "" },
		"missing start":    func(r *Record) { r.StartedAt = time.Time{} },
		"invalid status":   func(r *Record) { r.Status = "complete" },
		"missing finish":   func(r *Record) { r.FinishedAt = nil },
		"early finish": func(r *Record) {
			finish := r.StartedAt.Add(-time.Second)
			r.FinishedAt = &finish
		},
		"running finish":          func(r *Record) { r.Status = "running" },
		"negative calls":          func(r *Record) { r.CodexCalls = -1 },
		"negative Claude calls":   func(r *Record) { r.ClaudeCalls = -1 },
		"missing main provider":   func(r *Record) { r.Provider = "" },
		"invalid main provider":   func(r *Record) { r.Provider = "unknown" },
		"invalid critic provider": func(r *Record) { r.CriticProvider = "unknown" },
		"invalid executor": func(r *Record) {
			r.ExecutorID, r.SourceRef = "../foreign", strings.Repeat("a", 40)
		},
		"invalid source ref": func(r *Record) {
			r.ExecutorID, r.SourceRef = "local", "foreign"
		},
		"invalid usage source":                  func(r *Record) { r.UsageSource = "verified_billing" },
		"unattributed claim":                    func(r *Record) { r.ExecutorID, r.SourceRef = "", "" },
		"missing claim origin":                  func(r *Record) { r.UsageSource = "" },
		"incomplete token claim without origin": func(r *Record) { r.UsageComplete, r.UsageSource = false, "" },
		"unpaired executor":                     func(r *Record) { r.SourceRef = "" },
		"tokens without a call":                 func(r *Record) { r.CodexCalls = 0 },
		"claim origin without a call": func(r *Record) {
			r.CodexCalls, r.Tokens = 0, Tokens{}
		},
		"successful incomplete call without origin": func(r *Record) {
			r.Tokens, r.UsageComplete, r.UsageSource = Tokens{}, false, ""
		},
		"unbound pending call": func(r *Record) {
			r.Status, r.FinishedAt, r.UsageComplete, r.Tokens, r.UsageSource, r.ExecutorID, r.SourceRef = "running", nil, false, Tokens{}, "", "", ""
		},
		"unattributed zero-token return": func(r *Record) { r.Tokens, r.UsageSource = Tokens{}, "" },
		"negative input":                 func(r *Record) { r.Tokens.Input = -1 },
		"negative cache":                 func(r *Record) { r.Tokens.CachedInput = -1 },
		"negative output":                func(r *Record) { r.Tokens.Output = -1 },
		"cache exceeds input":            func(r *Record) { r.Tokens.CachedInput = r.Tokens.Input + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "usage")
			record := testRecord()
			modify(&record)
			if err := (Store{Dir: dir}).Save(record); err == nil {
				t.Fatal("expected validation failure")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid record created directory: %v", err)
			}
		})
	}
}

func TestStoreProviderMetadata(t *testing.T) {
	for _, dual := range []bool{false, true} {
		record := testRecord()
		if dual {
			record.Provider, record.Model, record.Effort = "claude", "default", "medium"
			record.CriticProvider, record.CriticModel, record.CriticEffort = "codex", "critic-model", "high"
			record.ClaudeCalls = 2
		}
		dir := t.TempDir()
		if err := (Store{Dir: dir}).Save(record); err != nil {
			t.Fatal(err)
		}
		if !dual {
			data, err := os.ReadFile(filepath.Join(dir, record.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"provider":"codex"`) {
				t.Fatalf("record is missing its main provider: %s", data)
			}
			for _, field := range []string{"critic_provider", "critic_model", "critic_effort", "claude_calls"} {
				if strings.Contains(string(data), `"`+field+`"`) {
					t.Fatalf("single-provider record contains unexpected field %s: %s", field, data)
				}
			}
		}
		records, err := ReadAll(dir)
		if err != nil || len(records) != 1 || !reflect.DeepEqual(records[0], record) {
			t.Fatalf("provider metadata changed: got=%+v want=%+v err=%v", records, record, err)
		}
	}
}

func TestReadAllRejectsMalformedRecords(t *testing.T) {
	record := testRecord()
	valid, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"bad JSON":             "{",
		"missing fields":       "{}",
		"missing provider":     strings.Replace(string(valid), `"provider":"codex",`, "", 1),
		"missing claim origin": strings.Replace(string(valid), `"usage_source":"executor_reported",`, "", 1),
		"unattributed usage":   strings.Replace(strings.Replace(string(valid), `"executor_id":"local",`, "", 1), `"source_ref":"`+strings.Repeat("a", 40)+`",`, "", 1),
		"trailing data":        string(valid) + " {}",
		"unknown field":        strings.TrimSuffix(string(valid), "}") + `,"typo":1}`,
		"wrong ID":             strings.ReplaceAll(string(valid), record.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"invalid counters":     strings.Replace(string(valid), `"input_tokens":100`, `"input_tokens":-1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, record.ID+".json"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadAll(dir); err == nil {
				t.Fatal("expected error instead of incomplete report")
			}
		})
	}
	for _, name := range []string{"wrong-name.json", record.ID + ".json"} {
		t.Run("invalid file "+name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadAll(dir); err == nil {
				t.Fatal("expected invalid file error")
			}
		})
	}
}

func TestReadAllSortsChronologically(t *testing.T) {
	dir := t.TempDir()
	first := testRecord()
	second := testRecord()
	second.ID = "00000000000000000000000000000000"
	second.StartedAt = first.StartedAt.Add(time.Second)
	for _, record := range []Record{first, second} {
		if err := (Store{Dir: dir}).Save(record); err != nil {
			t.Fatal(err)
		}
	}
	records, err := ReadAll(dir)
	if err != nil || len(records) != 2 || records[0].ID != first.ID {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func TestFilter(t *testing.T) {
	record := testRecord()
	for name, test := range map[string]struct {
		filter Filter
		want   bool
	}{
		"no constraints":   {Filter{}, true},
		"case insensitive": {Filter{Repo: "LINKDATA/MINION", User: "EXAMPLE"}, true},
		"wrong repository": {Filter{Repo: "elsewhere/minion"}, false},
		"wrong user":       {Filter{User: "another-user"}, false},
		"since inclusive":  {Filter{Since: record.StartedAt}, true},
		"before since":     {Filter{Since: record.StartedAt.Add(time.Nanosecond)}, false},
		"until exclusive":  {Filter{Until: record.StartedAt}, false},
		"before until":     {Filter{Until: record.StartedAt.Add(time.Nanosecond)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.filter.Match(record); got != test.want {
				t.Fatalf("Match=%v want=%v", got, test.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	first := testRecord()
	second := first
	second.Repo, second.User = "LINKDATA/MINION", "EXAMPLE"
	second.Status, second.UsageComplete = "failed", false
	second.CodexCalls, second.ClaudeCalls = 0, 2
	third := first
	third.Status, third.FinishedAt = "running", nil
	fourth := first
	fourth.Repo, fourth.User, fourth.Action = "another-owner/other", "another-user", "audit"
	fourth.StartedAt = time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	finishedAt := fourth.StartedAt.Add(time.Minute)
	fourth.FinishedAt = &finishedAt
	records := []Record{first, second, third, fourth}
	groups, err := Summarize(records, Filter{}, []string{"repo", "user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Repo != fourth.Repo || groups[1].Repo != first.Repo {
		t.Fatalf("incorrect order or grouping: %+v", groups)
	}
	got := groups[1]
	want := Summary{Repo: first.Repo, User: first.User, Attempts: 3, Succeeded: 1, Failed: 1, Running: 1, CodexCalls: 2, ClaudeCalls: 2,
		IncompleteUsage: 2, Tokens: Tokens{Input: 300, CachedInput: 240, Output: 60}, TotalTokens: 360}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary=%+v want=%+v", got, want)
	}
	groups, err = Summarize(records, Filter{User: "ANOTHER-USER"}, []string{"action", "day", "month"})
	if err != nil || len(groups) != 1 || groups[0].Day != "2026-10-31" || groups[0].Month != "2026-10" || groups[0].Action != "audit" {
		t.Fatalf("UTC grouping/filter: summary=%+v err=%v", groups, err)
	}
	groups, err = Summarize(records, Filter{}, nil)
	if err != nil || len(groups) != 1 || groups[0].Attempts != 4 || groups[0].CodexCalls != 3 || groups[0].ClaudeCalls != 2 || groups[0].TotalTokens != 480 || groups[0].Repo != "" {
		t.Fatalf("grand total=%+v err=%v", groups, err)
	}
	groups, err = Summarize(nil, Filter{}, nil)
	if err != nil || groups == nil || len(groups) != 0 {
		t.Fatalf("empty summary=%+v err=%v", groups, err)
	}
}

func TestSummarizeUsesRequestedSortOrder(t *testing.T) {
	first := testRecord()
	first.Repo, first.User = "a/repo", "z-user"
	second := testRecord()
	second.Repo, second.User = "z/repo", "a-user"
	groups, err := Summarize([]Record{first, second}, Filter{}, []string{"user", "repo"})
	if err != nil || len(groups) != 2 || groups[0].User != "a-user" {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
}

func TestSummarizeRejectsInvalidOptions(t *testing.T) {
	for _, groups := range [][]string{{"unknown"}, {"repo", "repo"}, {"none"}} {
		if _, err := Summarize(nil, Filter{}, groups); err == nil {
			t.Fatalf("groups=%v accepted", groups)
		}
	}
	now := time.Now()
	for _, until := range []time.Time{now, now.Add(-time.Second)} {
		if _, err := Summarize(nil, Filter{Since: now, Until: until}, nil); err == nil {
			t.Fatal("invalid date range accepted")
		}
	}
	record := testRecord()
	record.Status = "unknown"
	if _, err := Summarize([]Record{record}, Filter{}, nil); err == nil {
		t.Fatal("unknown status accepted")
	}
	record = testRecord()
	record.CodexCalls = -1
	if _, err := Summarize([]Record{record}, Filter{}, nil); err == nil {
		t.Fatal("negative call count accepted")
	}
}

func TestTokensValidBoundsAndComponentOperations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens Tokens
		valid  bool
	}{
		{"zero", Tokens{}, true},
		{"exact total", Tokens{Input: math.MaxInt64 - 1, CachedInput: 3, Output: 1}, true},
		{"total overflow", Tokens{Input: math.MaxInt64, Output: 1}, false},
		{"negative input", Tokens{Input: -1}, false},
		{"negative output", Tokens{Output: -1}, false},
		{"negative cache", Tokens{CachedInput: -1}, false},
		{"cache outside input", Tokens{Input: 1, CachedInput: 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tokens.Valid(); got != tc.valid {
				t.Fatalf("Valid=%v want=%v", got, tc.valid)
			}
		})
	}
	a, b := Tokens{Input: 100, CachedInput: 80, Output: 2}, Tokens{Input: 90, CachedInput: 70, Output: 30}
	if got := a.Sub(b); got != (Tokens{Input: 10, CachedInput: 10, Output: -28}) || got.Valid() {
		t.Fatalf("raw delta=%+v", got)
	}
	if got := a.Max(b); got != (Tokens{Input: 100, CachedInput: 80, Output: 30}) || !got.Valid() {
		t.Fatalf("component maximum=%+v", got)
	}
	// Individually valid snapshots can have a component maximum whose total overflows.
	if got := (Tokens{Input: math.MaxInt64 - 1, Output: 1}).Max(Tokens{Input: 1, Output: math.MaxInt64 - 1}); got.Valid() {
		t.Fatalf("overflowing maximum accepted: %+v", got)
	}
}

func TestTokensCheckedAdditionPreservesKnownUsage(t *testing.T) {
	base := Tokens{Input: 100, CachedInput: 80, Output: 20}
	for _, tc := range []struct {
		name  string
		other Tokens
		want  Tokens
		valid bool
	}{
		{"normal", Tokens{Input: 50, CachedInput: 10, Output: 30}, Tokens{Input: 150, CachedInput: 90, Output: 50}, true},
		{"exact total", Tokens{Input: math.MaxInt64 - 121, Output: 1}, Tokens{Input: math.MaxInt64 - 21, CachedInput: 80, Output: 21}, true},
		{"total overflow", Tokens{Input: math.MaxInt64 - 120, Output: 1}, base, false},
		{"component overflow", Tokens{Input: math.MaxInt64}, base, false},
		{"invalid cache", Tokens{Input: 1, CachedInput: 2}, base, false},
		{"negative", Tokens{Output: -1}, base, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := base.AddChecked(tc.other)
			if got != tc.want || ok != tc.valid {
				t.Fatalf("addition=%+v valid=%v want=%+v valid=%v", got, ok, tc.want, tc.valid)
			}
		})
	}
}

func TestSummarizeAggregateBounds(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		first, second Tokens
		codex, claude [2]int
		wantTokens    int64
		wantCodex     int
		wantClaude    int
		wantError     string
	}{
		{name: "exact token maximum", first: Tokens{Input: math.MaxInt64 / 2}, second: Tokens{Input: math.MaxInt64/2 + 1}, codex: [2]int{1, 1}, wantTokens: math.MaxInt64, wantCodex: 2},
		{name: "token maximum plus one", first: Tokens{Input: math.MaxInt64/2 + 1}, second: Tokens{Input: math.MaxInt64/2 + 1}, codex: [2]int{1, 1}, wantError: "token counts"},
		{name: "total overflow without component overflow", first: Tokens{Input: math.MaxInt64}, second: Tokens{Output: 1}, codex: [2]int{1, 1}, wantError: "token counts"},
		{name: "invalid tokens", second: Tokens{Input: -1}, codex: [2]int{1, 1}, wantError: "invalid counts"},
		{name: "exact codex call maximum", codex: [2]int{math.MaxInt / 2, math.MaxInt/2 + 1}, wantCodex: math.MaxInt},
		{name: "codex call overflow", codex: [2]int{math.MaxInt, 1}, wantError: "call counts"},
		{name: "exact claude call maximum", claude: [2]int{math.MaxInt / 2, math.MaxInt/2 + 1}, wantClaude: math.MaxInt},
		{name: "claude call overflow", claude: [2]int{math.MaxInt, 1}, wantError: "call counts"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			first, second := testRecord(), testRecord()
			first.Tokens, second.Tokens = scenario.first, scenario.second
			first.CodexCalls, second.CodexCalls = scenario.codex[0], scenario.codex[1]
			first.ClaudeCalls, second.ClaudeCalls = scenario.claude[0], scenario.claude[1]
			groups, err := Summarize([]Record{first, second}, Filter{}, nil)
			if scenario.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), scenario.wantError) || groups != nil {
					t.Fatalf("invalid aggregate emitted: groups=%+v err=%v", groups, err)
				}
				// A filtered-out record cannot poison an otherwise representable report.
				second.User = "excluded"
				groups, err = Summarize([]Record{first, second}, Filter{User: first.User}, nil)
				if err != nil || len(groups) != 1 || groups[0].Tokens != first.Tokens || groups[0].CodexCalls != first.CodexCalls || groups[0].ClaudeCalls != first.ClaudeCalls {
					t.Fatalf("excluded record poisoned report: groups=%+v err=%v", groups, err)
				}
				return
			}
			if err != nil || len(groups) != 1 || groups[0].TotalTokens != scenario.wantTokens || groups[0].CodexCalls != scenario.wantCodex || groups[0].ClaudeCalls != scenario.wantClaude || !groups[0].Tokens.Valid() {
				t.Fatalf("exact aggregate boundary failed: groups=%+v err=%v", groups, err)
			}
		})
	}
}
