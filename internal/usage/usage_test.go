package usage

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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

func TestCoverageFinalReportGaps(t *testing.T) {
	gaps := Coverage{Scope: "known_subset", MissingFinalReport: true, MalformedFinalReport: true}
	if gaps.ValidForCall(false) {
		t.Fatal("one call cannot both lack and have a malformed final report")
	}
	gaps.Scope = "attempt"
	if !gaps.ValidForAttempt(false) {
		t.Fatal("separate calls in one attempt can have different final-report gaps")
	}
}

func TestCallReceiverPersistsPendingAndDeduplicatesReceipts(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	record := testRecord()
	record.RepositoryID, record.RequesterID = 12, 34
	record.Status, record.FinishedAt = "running", nil
	record.CodexCalls, record.Tokens, record.UsageSource = 0, Tokens{}, ""
	record.UsageCoverage, record.UsageComplete = Coverage{Scope: "attempt"}, true
	start := record.StartedAt.Add(time.Second)
	call := Call{Ordinal: 1, StartedAt: start, Provider: "codex", Model: "model-a", Effort: "high", Role: "main", Round: 1}
	if added, err := record.BeginCall(call); err != nil || !added {
		t.Fatalf("begin call: added=%t error=%v", added, err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	pending, found, err := store.Load(record.ID)
	if err != nil || !found || len(pending.Calls) != 1 || pending.Calls[0].FinishedAt != nil ||
		pending.CodexCalls != 1 || pending.UsageComplete || !pending.UsageCoverage.PartialReport || pending.UsageSource != "" ||
		pending.RepositoryID != 12 || pending.RequesterID != 34 {
		t.Fatalf("pending checkpoint: %+v found=%t error=%v", pending, found, err)
	}
	replay := call
	replay.StartedAt = start.Add(time.Minute)
	if added, err := pending.BeginCall(replay); err != nil || added || !pending.Calls[0].StartedAt.Equal(start) {
		t.Fatalf("replayed start changed call: added=%t record=%+v error=%v", added, pending, err)
	}
	conflict := replay
	conflict.Model = "other-model"
	if _, err := pending.BeginCall(conflict); err == nil {
		t.Fatal("conflicting call identity was accepted")
	}
	finished := start.Add(2 * time.Minute)
	receipt := call
	receipt.FinishedAt = &finished
	receipt.Tokens = Tokens{Input: 100, CacheCreationInput: 10, CachedInput: 60, Output: 20}
	receipt.UsageCoverage = Coverage{Scope: "invocation"}
	receipt.Complete = true
	if added, err := pending.FinishCall(receipt); err != nil || !added {
		t.Fatalf("finish call: added=%t error=%v", added, err)
	}
	if err := store.Save(pending); err != nil {
		t.Fatal(err)
	}
	returned, found, err := store.Load(record.ID)
	if err != nil || !found || returned.Tokens != receipt.Tokens || !returned.UsageComplete || returned.UsageSource != "executor_reported" {
		t.Fatalf("returned checkpoint: %+v found=%t error=%v", returned, found, err)
	}
	receipt.FinishedAt = &replay.StartedAt
	if added, err := returned.FinishCall(receipt); err != nil || added || returned.Tokens != receipt.Tokens {
		t.Fatalf("replayed receipt changed totals: added=%t record=%+v error=%v", added, returned, err)
	}
	conflict = receipt
	conflict.Tokens.Input++
	if _, err := returned.FinishCall(conflict); err == nil || returned.Tokens != receipt.Tokens {
		t.Fatalf("conflicting receipt changed totals: record=%+v error=%v", returned, err)
	}
	if added, err := returned.BeginCall(call); err != nil || added {
		t.Fatalf("replayed start after receipt: added=%t error=%v", added, err)
	}
	if err := store.Save(returned); err != nil {
		t.Fatal(err)
	}
	if summary, err := Summarize([]Record{returned}, Filter{}, nil); err != nil || len(summary) != 1 || summary[0].CodexCalls != 1 || summary[0].Tokens != receipt.Tokens {
		t.Fatalf("duplicate usage entered summary: %+v error=%v", summary, err)
	}
}

func TestCallReceiverRetainsOverflowedReceiptAndIncompleteCoverage(t *testing.T) {
	record := testRecord()
	record.RepositoryID, record.RequesterID = 12, 34
	record.CriticProvider = "claude"
	record.Status, record.FinishedAt = "running", nil
	record.CodexCalls, record.Tokens, record.UsageSource = 0, Tokens{}, ""
	record.UsageCoverage, record.UsageComplete = Coverage{Scope: "attempt"}, true
	for ordinal, provider := range []string{"codex", "claude"} {
		start := record.StartedAt.Add(time.Duration(ordinal+1) * time.Second)
		call := Call{Ordinal: ordinal + 1, StartedAt: start, Provider: provider, Model: "default", Role: "main", Round: ordinal + 1}
		if ordinal == 1 {
			call.Role, call.ReviewPhase = "critic", "full"
		}
		if _, err := record.BeginCall(call); err != nil {
			t.Fatal(err)
		}
		finish := start.Add(time.Second)
		call.FinishedAt = &finish
		call.Tokens = Tokens{Input: math.MaxInt64/2 + 1}
		call.UsageCoverage = Coverage{Scope: "invocation"}
		call.Complete = true
		if _, err := record.FinishCall(call); err != nil {
			t.Fatal(err)
		}
	}
	if record.CodexCalls != 1 || record.ClaudeCalls != 1 || record.Tokens.Input != math.MaxInt64/2+1 ||
		record.UsageComplete || !record.UsageCoverage.PartialReport || len(record.Calls) != 2 || record.Calls[1].Tokens.Input != math.MaxInt64/2+1 {
		t.Fatalf("overflow lost per-call evidence or changed aggregate: %+v", record)
	}
	if err := (Store{Dir: t.TempDir()}).Save(record); err != nil {
		t.Fatalf("overflowed receipt should remain readable: %v", err)
	}
	broken := record
	broken.CodexCalls++
	if err := (Store{Dir: t.TempDir()}).Save(broken); err == nil {
		t.Fatal("inconsistent aggregate was saved")
	}
}

func TestCallReceiverDistinguishesPendingFromZeroTokenReturn(t *testing.T) {
	record := testRecord()
	record.RepositoryID, record.RequesterID = 12, 34
	record.Status, record.FinishedAt = "running", nil
	record.CodexCalls, record.Tokens, record.UsageSource = 0, Tokens{}, ""
	record.UsageCoverage, record.UsageComplete = Coverage{Scope: "attempt"}, true
	call := Call{Ordinal: 1, StartedAt: record.StartedAt.Add(time.Second), Provider: "codex", Model: "default", Role: "main", Round: 1}
	if _, err := record.BeginCall(call); err != nil {
		t.Fatal(err)
	}
	if record.UsageComplete || record.UsageSource != "" || !record.UsageCoverage.PartialReport {
		t.Fatalf("pending call claimed a returned receipt: %+v", record)
	}
	finished := call.StartedAt.Add(time.Second)
	call.FinishedAt, call.Complete = &finished, true
	if _, err := record.FinishCall(call); err != nil {
		t.Fatal(err)
	}
	if !record.UsageComplete || record.UsageSource != "executor_reported" || record.UsageCoverage.PartialReport ||
		record.Calls[0].FinishedAt == nil || record.Tokens != (Tokens{}) {
		t.Fatalf("zero-token receipt remained pending: %+v", record)
	}
	if err := (Store{Dir: t.TempDir()}).Save(record); err != nil {
		t.Fatalf("zero-token receipt could not be saved: %v", err)
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
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("record permissions: info=%v err=%v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".record-interrupted"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan struct{})
	firstSaved := make(chan error, 1)
	readerProgress := make(chan struct{}, 2)
	var writerErr error
	go func() {
		defer close(writerDone)
		for i := 1; i <= 30; i++ {
			record.CodexCalls = i
			record.Tokens = Tokens{Input: int64(i), CachedInput: int64(i / 2), Output: int64(i * 2)}
			record.UsageSource = "executor_reported"
			err := store.Save(record)
			if i == 1 {
				firstSaved <- err
				if err == nil {
					<-readerProgress
					<-readerProgress // Both readers query before the writer can finish.
				}
			}
			if err != nil {
				writerErr = err
				return
			}
		}
	}()
	if err := <-firstSaved; err != nil {
		t.Fatal(err)
	}
	checkRead := func() {
		records, err := ReadAll(dir)
		if err != nil {
			t.Error(err)
			return
		}
		if len(records) != 1 || records[0].Tokens.Input != int64(records[0].CodexCalls) || records[0].Tokens.Output != 2*records[0].Tokens.Input {
			t.Errorf("partial or duplicate record: %+v", records)
		}
		query, err := store.Query(Filter{}, nil, true)
		if err != nil || len(query.Attempts) != 1 || query.Totals.Tokens != query.Attempts[0].Record.Tokens ||
			query.Attempts[0].Usage.Tokens != query.Attempts[0].Record.Tokens {
			t.Errorf("query totals and details disagree: %+v, error=%v", query, err)
		}
	}
	readersReady := make(chan struct{}, 2)
	startReaders := make(chan struct{})
	readersDone := make(chan struct{}, 2)
	for range 2 {
		go func() {
			defer func() { readersDone <- struct{}{} }()
			readersReady <- struct{}{}
			<-startReaders
			checkRead()
			readerProgress <- struct{}{}
			for range 30 {
				select {
				case <-writerDone:
					return
				default:
					checkRead()
				}
			}
		}()
	}
	<-readersReady
	<-readersReady
	close(startReaders)
	<-writerDone
	<-readersDone
	<-readersDone
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	records, err := ReadAll(dir)
	if err != nil || len(records) != 1 || records[0].CodexCalls != 30 {
		t.Fatalf("final record=%+v err=%v", records, err)
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

func TestStoreLoadOneAttempt(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "usage")}
	record := testRecord()
	if _, found, err := store.Load(record.ID); err != nil || found {
		t.Fatalf("missing directory: found=%t err=%v", found, err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	if got, found, err := store.Load(record.ID); err != nil || !found || !reflect.DeepEqual(got, record) {
		t.Fatalf("record=%+v found=%t err=%v", got, found, err)
	}
	if _, found, err := store.Load(strings.Repeat("b", 32)); err != nil || found {
		t.Fatalf("missing attempt: found=%t err=%v", found, err)
	}
	if _, _, err := store.Load("../outside"); err == nil {
		t.Fatal("accepted invalid attempt ID")
	}
	if err := os.WriteFile(filepath.Join(store.Dir, record.ID+".json"), []byte(`{"id":"`+record.ID+`","typo":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(record.ID); err == nil {
		t.Fatal("accepted malformed stored record")
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
		"invalid usage scope":                   func(r *Record) { r.UsageCoverage.Scope = "foreign" },
		"call scope in attempt":                 func(r *Record) { r.UsageCoverage.Scope = "invocation" },
		"complete partial attempt":              func(r *Record) { r.UsageCoverage.PartialReport = true },
		"complete unverified helper attempt":    func(r *Record) { r.UsageCoverage.UnverifiedHelpers = true },
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
	record.RepositoryID = 12
	for name, test := range map[string]struct {
		filter Filter
		want   bool
	}{
		"no constraints":   {Filter{}, true},
		"case insensitive": {Filter{Repo: "LINKDATA/MINION", User: "EXAMPLE"}, true},
		"wrong repository": {Filter{Repo: "elsewhere/minion"}, false},
		"repository ID":    {Filter{RepositoryID: 12}, true},
		"wrong repo ID":    {Filter{RepositoryID: 13}, false},
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
	want := Summary{Repo: first.Repo, User: first.User, Attempts: 3, UsageRecordedActions: 1, Succeeded: 1, Failed: 1, Running: 1, CodexCalls: 2, ClaudeCalls: 2, EstimatedCostUnknownCalls: 4,
		IncompleteUsage: 2, Tokens: Tokens{Input: 300, CachedInput: 240, Output: 60},
		UnallocatedTokens: Tokens{Input: 300, CachedInput: 240, Output: 60}, TotalTokens: 360}
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
		{"negative cache creation", Tokens{CacheCreationInput: -1}, false},
		{"cache creation outside input", Tokens{Input: 1, CacheCreationInput: 2}, false},
		{"disjoint cache categories", Tokens{Input: 10, CachedInput: 6, CacheCreationInput: 4}, true},
		{"overlapping cache categories", Tokens{Input: 10, CachedInput: 6, CacheCreationInput: 5}, false},
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
	// A component floor may conflict across disjoint cache categories. It must
	// remain invalid; raising Input to make it valid would invent tokens.
	created := Tokens{Input: 10, CacheCreationInput: 10}
	read := Tokens{Input: 10, CachedInput: 10}
	if got := created.Max(read); got.Valid() || got.Input != 10 || got.CacheCreationInput != 10 || got.CachedInput != 10 {
		t.Fatalf("conflicting component maximum accepted: %+v", got)
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
		{"cache categories", Tokens{Input: 50, CacheCreationInput: 20, CachedInput: 10, Output: 30}, Tokens{Input: 150, CacheCreationInput: 20, CachedInput: 90, Output: 50}, true},
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

func testMixedServiceRecord(t *testing.T) Record {
	t.Helper()
	record := testRecord()
	record.RepositoryID, record.RequesterID = 12, 7
	record.CriticProvider = "claude"
	record.CodexCalls, record.Tokens, record.UsageSource = 0, Tokens{}, ""
	record.UsageCoverage, record.UsageComplete = Coverage{Scope: "attempt"}, true
	for ordinal, spec := range []struct {
		provider, role, phase string
		tokens                Tokens
	}{
		{"codex", "main", "", Tokens{Input: 100, CachedInput: 25, Output: 10}},
		{"claude", "critic", "full", Tokens{Input: 30, Output: 5}},
	} {
		started := record.StartedAt.Add(time.Duration(ordinal+1) * time.Second)
		call := Call{Ordinal: ordinal + 1, StartedAt: started, Provider: spec.provider, Model: "default",
			Role: spec.role, Round: 1, ReviewPhase: spec.phase}
		if _, err := record.BeginCall(call); err != nil {
			t.Fatal(err)
		}
		finished := started.Add(time.Second)
		call.FinishedAt, call.Tokens = &finished, spec.tokens
		call.UsageCoverage, call.Complete = Coverage{Scope: "invocation"}, true
		if spec.provider == "claude" {
			amount := 1.25
			call.EstimatedCostUSD = &amount
			call.EstimatedCostComplete = true
		}
		if _, err := record.FinishCall(call); err != nil {
			t.Fatal(err)
		}
		if spec.provider == "claude" {
			*call.EstimatedCostUSD = 9.25
			if *record.Calls[ordinal].EstimatedCostUSD != 1.25 {
				t.Fatal("finished cost receipt retained caller-owned pointer")
			}
		}
	}
	return record
}

func TestEstimatedCostReceiptReplayAndValidation(t *testing.T) {
	record := testMixedServiceRecord(t)
	store := &Store{Dir: t.TempDir()}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.Load(record.ID)
	if err != nil || !found {
		t.Fatalf("reload estimated-cost receipt: found=%t error=%v", found, err)
	}
	record = loaded
	before := record.Calls[1]
	replay := before
	amount := *before.EstimatedCostUSD
	replay.EstimatedCostUSD = &amount
	if added, err := record.FinishCall(replay); err != nil || added {
		t.Fatalf("identical estimated-cost receipt was not idempotent: added=%t error=%v", added, err)
	}
	other := before
	changed := 2.5
	other.EstimatedCostUSD = &changed
	if _, err := record.FinishCall(other); err == nil || *record.Calls[1].EstimatedCostUSD != 1.25 {
		t.Fatalf("conflicting estimate replaced the saved receipt: record=%+v error=%v", record, err)
	}
	other = before
	other.EstimatedCostComplete = false
	if _, err := record.FinishCall(other); err == nil || !record.Calls[1].EstimatedCostComplete {
		t.Fatalf("conflicting cost coverage replaced the saved receipt: record=%+v error=%v", record, err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Record)
	}{
		{"negative", func(r *Record) { amount := -1.0; r.Calls[1].EstimatedCostUSD = &amount }},
		{"non-finite", func(r *Record) { amount := math.NaN(); r.Calls[1].EstimatedCostUSD = &amount }},
		{"wrong provider", func(r *Record) { amount := 1.0; r.Calls[0].EstimatedCostUSD = &amount }},
		{"complete without estimate", func(r *Record) { r.Calls[1].EstimatedCostUSD = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := record
			invalid.Calls = slices.Clone(record.Calls)
			tc.edit(&invalid)
			if err := (Store{Dir: t.TempDir()}).Save(invalid); err == nil {
				t.Fatal("invalid cost claim was saved")
			}
		})
	}
}

func TestEstimatedCostCoverageIndependentOfTokenCoverage(t *testing.T) {
	record := testMixedServiceRecord(t)
	record.Calls[1].EstimatedCostComplete = false // A later final result omitted cost after a known partial snapshot.
	summaries, err := Summarize([]Record{record}, Filter{}, nil)
	if err != nil || len(summaries) != 1 || summaries[0].EstimatedCostUSD != 1.25 ||
		summaries[0].EstimatedCostKnownCalls != 1 || summaries[0].EstimatedCostUnknownCalls != 2 ||
		summaries[0].IncompleteUsage != 0 {
		t.Fatalf("partial cost was treated as complete with complete tokens: %+v, %v", summaries, err)
	}
	record = testMixedServiceRecord(t)
	record.Calls[1].Complete = false // Native cost is complete even though tokens are only a known subset.
	record.Calls[1].UsageCoverage = Coverage{Scope: "known_subset", PartialReport: true}
	if err := record.reconcileCalls(); err != nil {
		t.Fatal(err)
	}
	summaries, err = Summarize([]Record{record}, Filter{}, nil)
	if err != nil || len(summaries) != 1 || summaries[0].EstimatedCostKnownCalls != 1 ||
		summaries[0].EstimatedCostUnknownCalls != 1 || summaries[0].IncompleteUsage != 1 {
		t.Fatalf("complete cost was marked unknown with incomplete tokens: %+v, %v", summaries, err)
	}
}

func TestLegacyEstimatedCostCoverageRejectsCallCountOverflow(t *testing.T) {
	legacy := testRecord()
	legacy.Status, legacy.CodexCalls, legacy.ClaudeCalls = "failed", math.MaxInt, 1
	legacy.Tokens, legacy.UsageSource, legacy.UsageComplete = Tokens{}, "", false
	if _, err := Summarize([]Record{legacy}, Filter{}, nil); err == nil {
		t.Fatal("legacy call-count overflow entered estimated-cost coverage")
	}
}

func TestQueryPartitionsServicesAndKeepsActionsDistinct(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	mixed := testMixedServiceRecord(t)
	legacy := testRecord()
	legacy.ID = strings.Repeat("b", 32)
	legacy.RequesterID = 7
	legacy.Status, legacy.UsageComplete = "failed", false
	for _, record := range []Record{mixed, legacy} {
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	full, err := store.Query(Filter{}, []string{"service"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if full.ActionCoverage != "unknown" || full.Totals.Attempts != 2 || full.Totals.UsageRecordedActions != 1 || full.Totals.TotalTokens != 265 ||
		full.Totals.EstimatedCostUSD != 1.25 || full.Totals.EstimatedCostKnownCalls != 1 || full.Totals.EstimatedCostUnknownCalls != 2 ||
		full.Totals.UnallocatedTokens != legacy.Tokens || len(full.Groups) != 3 {
		t.Fatalf("grand totals or service groups: %+v", full)
	}
	want := map[string]struct {
		tokens        Tokens
		unallocated   Tokens
		codex, claude int
	}{
		"codex":   {Tokens{Input: 100, CachedInput: 25, Output: 10}, Tokens{}, 1, 0},
		"claude":  {Tokens{Input: 30, Output: 5}, Tokens{}, 0, 1},
		"unknown": {legacy.Tokens, legacy.Tokens, 1, 0},
	}
	for _, group := range full.Groups {
		expected, found := want[group.Service]
		if !found || group.Attempts != 1 || group.UsageRecordedActions != 1 || group.Tokens != expected.tokens ||
			group.UnallocatedTokens != expected.unallocated || group.CodexCalls != expected.codex || group.ClaudeCalls != expected.claude {
			t.Fatalf("service group %q: %+v", group.Service, group)
		}
		if group.Service == "claude" && (group.EstimatedCostUSD != 1.25 || group.EstimatedCostKnownCalls != 1 || group.EstimatedCostUnknownCalls != 0) {
			t.Fatalf("Claude cost estimate lost in service group: %+v", group)
		}
		delete(want, group.Service)
	}
	if len(want) != 0 {
		t.Fatalf("missing service groups: %+v", want)
	}
	selected, err := store.Query(Filter{Service: "claude", RequesterID: 7}, nil, true)
	if err != nil || selected.Totals.Attempts != 1 || selected.Totals.TotalTokens != 35 || selected.Totals.CodexCalls != 0 ||
		selected.Totals.EstimatedCostUSD != 1.25 || selected.Totals.EstimatedCostKnownCalls != 1 || selected.Totals.EstimatedCostUnknownCalls != 0 ||
		selected.Totals.ClaudeCalls != 1 || len(selected.Attempts) != 1 || selected.Attempts[0].Record.Tokens != mixed.Tokens ||
		selected.Attempts[0].Usage.Tokens != (Tokens{Input: 30, Output: 5}) || selected.Attempts[0].Usage.Service != "claude" {
		t.Fatalf("selected service projection: %+v, error=%v", selected, err)
	}
	unfiltered, err := store.Query(Filter{}, nil, true)
	if err != nil || len(unfiltered.Attempts) != 2 || unfiltered.Attempts[0].Usage.Tokens != mixed.Tokens {
		t.Fatalf("unfiltered attempt projection: %+v, error=%v", unfiltered, err)
	}
	withGroups, err := store.Query(Filter{}, []string{"service"}, true)
	if err != nil || len(withGroups.Groups) != 3 || len(withGroups.Attempts) != 2 || withGroups.Totals != full.Totals {
		t.Fatalf("combined groups and details: %+v, error=%v", withGroups, err)
	}
	unknown, err := store.Query(Filter{Service: "unknown"}, nil, true)
	if err != nil || unknown.Totals.Attempts != 1 || unknown.Totals.UnallocatedTokens != legacy.Tokens ||
		len(unknown.Attempts) != 1 || unknown.Attempts[0].Record.ID != legacy.ID {
		t.Fatalf("legacy service selection: %+v, error=%v", unknown, err)
	}
}

func TestQueryRequesterAndUTCBoundaries(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	first := testRecord()
	first.ID, first.JobID, first.RepositoryID, first.RequesterID = strings.Repeat("b", 32), "first", 10, 10
	first.StartedAt = time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	finish := first.StartedAt.Add(time.Minute)
	first.FinishedAt = &finish
	second := testRecord()
	second.ID, second.JobID, second.RepositoryID, second.RequesterID = strings.Repeat("c", 32), "second", 2, 2
	second.StartedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	secondFinish := second.StartedAt.Add(time.Minute)
	second.FinishedAt = &secondFinish
	for _, record := range []Record{first, second} {
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.Query(Filter{Since: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, []string{"requester_id", "day"}, false)
	if err != nil || result.Totals.Attempts != 1 || len(result.Groups) != 1 ||
		result.Groups[0].RequesterID != 10 || result.Groups[0].Day != "2026-10-31" {
		t.Fatalf("UTC boundary: %+v, error=%v", result, err)
	}
	result, err = store.Query(Filter{}, []string{"requester_id"}, false)
	if err != nil || len(result.Groups) != 2 || result.Groups[0].RequesterID != 2 || result.Groups[1].RequesterID != 10 {
		t.Fatalf("numeric requester order: %+v, error=%v", result, err)
	}
	result, err = store.Query(Filter{}, []string{"repository_id"}, false)
	if err != nil || len(result.Groups) != 2 || result.Groups[0].RepositoryID != 2 || result.Groups[1].RepositoryID != 10 {
		t.Fatalf("numeric repository order: %+v, error=%v", result, err)
	}
	for _, filter := range []Filter{{Service: "other"}, {RepositoryID: -1}, {RequesterID: -1}, {Since: second.StartedAt, Until: second.StartedAt}} {
		if _, err := store.Query(filter, nil, false); err == nil {
			t.Fatalf("invalid filter accepted: %+v", filter)
		}
	}
	if _, err := store.Query(Filter{}, []string{"service", "service"}, false); err == nil {
		t.Fatal("duplicate service group accepted")
	}
}

func TestQueryPreservesOverflowedCallAttribution(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	record := testMixedServiceRecord(t)
	record.Calls[0].Tokens = Tokens{Input: math.MaxInt64/2 + 1}
	record.Calls[1].Tokens = Tokens{Input: math.MaxInt64/2 + 1}
	if err := record.reconcileCalls(); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	result, err := store.Query(Filter{}, []string{"service"}, false)
	if err != nil || result.Totals.TotalTokens != math.MaxInt64/2+1 || len(result.Groups) != 2 ||
		result.Groups[0].Tokens != (Tokens{}) || result.Groups[1].Tokens != record.Tokens ||
		result.Groups[0].IncompleteUsage != 1 {
		t.Fatalf("overflowed service accounting: %+v, error=%v", result, err)
	}
}
