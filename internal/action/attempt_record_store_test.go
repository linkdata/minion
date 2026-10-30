package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func attemptRecordFixture(sequence uint64, kind, text string) attemptRecord {
	return attemptRecord{
		Identity: Identity{
			JobID: "job", AttemptID: "attempt", ExecutorID: "local", Repository: "owner/repo",
			RepositoryID: 1, OwnerID: 2, Requester: "writer", RequesterID: 3, Number: 7, SubjectType: "Issue",
		},
		SourceRef: strings.Repeat("a", 40), Sequence: sequence, Kind: kind, Text: text,
	}
}

func openAttemptFixture(t *testing.T, root string) *attemptRecordStore {
	t.Helper()
	entry := attemptRecordFixture(1, attemptRecordInput, "")
	store, err := openAttemptRecordStore(root, entry.Identity, entry.SourceRef)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAttemptRecordsAppendReopenWithoutResult(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	for i, kind := range []string{attemptRecordInput, attemptRecordDiagnostic, attemptRecordReviewReference, attemptRecordWorkerReference} {
		entry := attemptRecordFixture(uint64(i+1), kind, "")
		if i < 2 {
			entry.Text = "bounded private text"
		}
		if ack, err := store.Append(entry); err != nil || ack != uint64(i+1) {
			t.Fatalf("append %d: ack=%d error=%v", i+1, ack, err)
		}
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAttemptFixture(t, root)
	if ack, err := reopened.Append(attemptRecordFixture(2, attemptRecordDiagnostic, "bounded private text")); err != nil || ack != 4 {
		t.Fatalf("exact replay did not return highest contiguous sequence: ack=%d error=%v", ack, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("replay rewrote file: error=%v", err)
	}
	if ack, err := reopened.Append(attemptRecordFixture(5, attemptRecordReport, "final report")); err != nil || ack != 5 {
		t.Fatalf("append after reopen: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsReplayComparesRedactedRepresentation(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	secrets := []string{"fixture-secret-one", "fixture-secret-two"}
	first := attemptRecordFixture(1, attemptRecordInput, secrets[0])
	if ack, err := store.Append(first, secrets...); err != nil || ack != 1 {
		t.Fatalf("first append: ack=%d error=%v", ack, err)
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openAttemptFixture(t, root)
	equivalent := attemptRecordFixture(1, attemptRecordInput, secrets[1])
	if ack, err := store.Append(equivalent, secrets...); err != nil || ack != 1 {
		t.Fatalf("equivalent redacted replay: ack=%d error=%v", ack, err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("equivalent replay changed stored record: error=%v", err)
	}
	changed := attemptRecordFixture(1, attemptRecordInput, "visible change")
	if _, err := store.Append(changed, secrets...); !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("changed stored text did not conflict: %v", err)
	}
}

func TestAttemptRecordsRecoverIncompleteTailAndRejectCompleteCorruption(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	if _, err := store.Append(attemptRecordFixture(1, attemptRecordInput, "first")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"identity":`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAttemptFixture(t, root)
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, complete) {
		t.Fatalf("incomplete tail survived recovery: error=%v", err)
	}
	if ack, err := reopened.Append(attemptRecordFixture(2, attemptRecordDiscussion, "second")); err != nil || ack != 2 {
		t.Fatalf("append after tail recovery: ack=%d error=%v", ack, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{bad}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entry := attemptRecordFixture(1, attemptRecordInput, "")
	if _, err := openAttemptRecordStore(root, entry.Identity, entry.SourceRef); !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("complete corrupt line was accepted: %v", err)
	}
}

func TestAttemptRecordsRejectLinkedExistingFileWithoutChangingTarget(t *testing.T) {
	for _, test := range []struct {
		name string
		link func(string, string) error
	}{
		{"symlink", os.Symlink},
		{"hardlink", os.Link},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			reviews := filepath.Join(root, "reviews")
			records := filepath.Join(root, "attempt-records")
			if err := os.MkdirAll(reviews, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(records, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(reviews, "attempt.json")
			want := []byte(`{"saved":"review evidence"}`) // No final newline: old recovery truncated this file.
			if err := os.WriteFile(target, want, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(records, "attempt.jsonl")
			if err := test.link(target, path); err != nil {
				// Windows ERROR_PRIVILEGE_NOT_HELD (1314) can forbid symlink creation.
				if errors.Is(err, os.ErrPermission) || runtime.GOOS == "windows" && test.name == "symlink" && errors.Is(err, syscall.Errno(1314)) {
					t.Skipf("link creation requires host privilege: %v", err)
				}
				t.Fatal(err)
			}
			entry := attemptRecordFixture(1, attemptRecordInput, "")
			if _, err := openAttemptRecordStore(root, entry.Identity, entry.SourceRef); !errors.Is(err, errAttemptRecordConflict) {
				t.Fatalf("linked attempt record was accepted: %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("linked target changed: got=%q error=%v", got, err)
			}
		})
	}
}

func TestAttemptRecordsRejectReplayGapAndForeignBinding(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	first := attemptRecordFixture(1, attemptRecordInput, "first")
	if _, err := store.Append(first); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		entry attemptRecord
	}{
		{"changed replay", attemptRecordFixture(1, attemptRecordInput, "changed")},
		{"gap", attemptRecordFixture(3, attemptRecordInput, "third")},
		{"zero sequence", attemptRecordFixture(0, attemptRecordInput, "zero")},
		{"foreign attempt", func() attemptRecord {
			r := attemptRecordFixture(2, attemptRecordInput, "other")
			r.Identity.AttemptID = "other"
			return r
		}()},
		{"foreign source", func() attemptRecord {
			r := attemptRecordFixture(2, attemptRecordInput, "other")
			r.SourceRef = strings.Repeat("b", 40)
			return r
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if ack, err := store.Append(test.entry); ack != 0 || !errors.Is(err, errAttemptRecordConflict) {
				t.Fatalf("invalid entry accepted: ack=%d error=%v", ack, err)
			}
		})
	}
	for _, entry := range []attemptRecord{
		attemptRecordFixture(2, "unknown", ""),
		attemptRecordFixture(2, attemptRecordReviewReference, "copied review text"),
		attemptRecordFixture(2, attemptRecordDiagnostic, strings.Repeat("x", maxAttemptRecordTextBytes+1)),
	} {
		if ack, err := store.Append(entry); ack != 0 || err == nil {
			t.Fatalf("invalid kind/text accepted: ack=%d error=%v", ack, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	foreign := first
	foreign.Identity.AttemptID = "foreign"
	data, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openAttemptRecordStore(root, first.Identity, first.SourceRef); !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("foreign saved line was accepted: %v", err)
	}
	foreign.Identity = first.Identity
	foreign.SourceRef = strings.Repeat("b", 40)
	data, err = json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openAttemptRecordStore(root, first.Identity, first.SourceRef); !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("foreign saved source was accepted: %v", err)
	}
	if _, err := openAttemptRecordStore(root, first.Identity, "bad-sha"); err == nil {
		t.Fatal("invalid source accepted")
	}
}

func TestAttemptRecordsCapReservesStopAndCoverage(t *testing.T) {
	const capBytes = 8192
	root := t.TempDir()
	entry := attemptRecordFixture(1, attemptRecordDiscussion, "")
	store, err := openAttemptRecordStoreWithLimit(root, entry.Identity, entry.SourceRef, capBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var sequence uint64 = 1
	for {
		entry := attemptRecordFixture(sequence, attemptRecordDiscussion, strings.Repeat("x", 300))
		ack, err := store.Append(entry)
		if errors.Is(err, errAttemptRecordsFull) {
			if ack != 0 || sequence == 1 {
				t.Fatalf("full-store result: ack=%d sequence=%d", ack, sequence)
			}
			break
		}
		if err != nil || ack != sequence {
			t.Fatalf("append before cap: ack=%d error=%v", ack, err)
		}
		sequence++
	}
	markerText := strings.Repeat("\x01", maxAttemptMarkerTextBytes)
	if ack, err := store.Append(attemptRecordFixture(sequence, attemptRecordStop, markerText)); err != nil || ack != sequence {
		t.Fatalf("reserved stop marker failed: ack=%d error=%v", ack, err)
	}
	sequence++
	if ack, err := store.Append(attemptRecordFixture(sequence, attemptRecordCoverage, "")); err != nil || ack != sequence {
		t.Fatalf("reserved coverage marker failed: ack=%d error=%v", ack, err)
	}
	sequence++
	if _, err := store.Append(attemptRecordFixture(sequence, attemptRecordStop, "duplicate")); !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("second stop marker accepted: %v", err)
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	info, err := os.Stat(path)
	if err != nil || info.Size() > capBytes {
		t.Fatalf("encoded file exceeded cap: info=%v error=%v", info, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openAttemptRecordStoreWithLimit(root, entry.Identity, entry.SourceRef, capBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if ack, err := reopened.Append(attemptRecordFixture(sequence-1, attemptRecordCoverage, "")); err != nil || ack != sequence-1 {
		t.Fatalf("coverage replay after cap failed: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsRedactFreeText(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	const secret = "fixture-secret-value"
	entry := attemptRecordFixture(1, attemptRecordInput, "prompt "+secret+" finished")
	if ack, err := store.Append(entry, secret); err != nil || ack != 1 {
		t.Fatalf("redacted append: ack=%d error=%v", ack, err)
	}
	if ack, err := store.Append(entry, secret); err != nil || ack != 1 {
		t.Fatalf("redacted replay: ack=%d error=%v", ack, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "attempt-records", "attempt.jsonl"))
	if err != nil || bytes.Contains(data, []byte(secret)) || !bytes.Contains(data, []byte("[redacted]")) {
		t.Fatalf("free text was not redacted: data=%q error=%v", data, err)
	}
	encoded := attemptRecordFixture(2, attemptRecordInput, "fixture-&#x73;ecret-value")
	if ack, err := store.Append(encoded, secret); ack != 0 || err == nil {
		t.Fatalf("recoverable encoded credential was retained: ack=%d error=%v", ack, err)
	}
	jsonEscaped := attemptRecordFixture(2, attemptRecordInput, "<")
	if ack, err := store.Append(jsonEscaped, "u003c"); ack != 0 || err == nil {
		t.Fatalf("JSON encoding introduced configured credential bytes: ack=%d error=%v", ack, err)
	}
	if after, err := os.ReadFile(filepath.Join(root, "attempt-records", "attempt.jsonl")); err != nil || !bytes.Equal(data, after) {
		t.Fatalf("rejected credential changed the record: error=%v", err)
	}
	if _, err := store.Append(attemptRecordFixture(2, attemptRecordInput, "raw\x00text")); err == nil {
		t.Fatal("raw NUL was accepted")
	}
}

func TestAttemptRecordsKeepMarkersWhenRedactionExpands(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	entry := attemptRecordFixture(1, attemptRecordStop, strings.Repeat("zz", maxAttemptMarkerTextBytes/2))
	if ack, err := store.Append(entry, "zz"); err != nil || ack != 1 {
		t.Fatalf("reserved stop marker lost after redaction: ack=%d error=%v", ack, err)
	}
	if ack, err := store.Append(attemptRecordFixture(2, attemptRecordCoverage, "")); err != nil || ack != 2 {
		t.Fatalf("empty coverage marker failed: ack=%d error=%v", ack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAttemptFixture(t, root)
	if ack, err := reopened.Append(attemptRecordFixture(2, attemptRecordCoverage, ""), "zz"); err != nil || ack != 2 {
		t.Fatalf("coverage marker did not survive reopen: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsKeepMarkerWhenJSONEncodingWouldExposeKey(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	entry := attemptRecordFixture(1, attemptRecordStop, "<")
	if ack, err := store.Append(entry, "u003c"); err != nil || ack != 1 {
		t.Fatalf("stop marker lost when JSON encoding exposed key: ack=%d error=%v", ack, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "attempt-records", "attempt.jsonl"))
	if err != nil || bytes.Contains(data, []byte("u003c")) {
		t.Fatalf("encoded key was retained: data=%q error=%v", data, err)
	}
}

func TestAttemptRecordsCreationDirectorySyncFailureReconciles(t *testing.T) {
	original := syncPrivateRecordDirectories
	t.Cleanup(func() { syncPrivateRecordDirectories = original })
	root := t.TempDir()
	canonicalRoot, err := workspace.CanonicalPath(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	calls := 0
	syncPrivateRecordDirectories = func(dirs ...string) error {
		calls++
		if len(dirs) != 2 || dirs[0] != filepath.Join(canonicalRoot, "attempt-records") || dirs[1] != canonicalRoot {
			t.Errorf("wrong directories synced: %q", dirs)
		}
		if calls == 1 {
			return syscall.EIO
		}
		return original(dirs...)
	}
	entry := attemptRecordFixture(1, attemptRecordInput, "first")
	if _, err := openAttemptRecordStore(root, entry.Identity, entry.SourceRef); !errors.Is(err, syscall.EIO) {
		t.Fatalf("creation directory sync failure was ignored: %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("created file unavailable for reconciliation: info=%v error=%v", info, err)
	}
	store := openAttemptFixture(t, root)
	if calls != 2 {
		t.Fatalf("reopen skipped directory resync: %d calls", calls)
	}
	if ack, err := store.Append(entry); err != nil || ack != 1 {
		t.Fatalf("append after directory resync: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsUncertainWriteAndSyncRequireReopen(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*attemptRecordStore)
		want   error
		kept   bool
	}{
		{"partial write", func(s *attemptRecordStore) {
			s.writeAt = func(data []byte, offset int64) (int, error) {
				return s.file.WriteAt(data[:len(data)/2], offset)
			}
		}, io.ErrShortWrite, false},
		{"sync failure", func(s *attemptRecordStore) {
			s.syncFile = func() error { return syscall.EIO }
		}, syscall.EIO, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := openAttemptFixture(t, root)
			test.inject(store)
			entry := attemptRecordFixture(1, attemptRecordDiagnostic, "diagnostic")
			if ack, err := store.Append(entry); ack != 0 || !errors.Is(err, test.want) {
				t.Fatalf("uncertain append acknowledged: ack=%d error=%v", ack, err)
			}
			if ack, err := store.Append(entry); ack != 0 || !errors.Is(err, errAttemptRecordUncertain) {
				t.Fatalf("uncertain store accepted retry: ack=%d error=%v", ack, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openAttemptFixture(t, root)
			if len(reopened.lines) != 0 && !test.kept || len(reopened.lines) != 1 && test.kept {
				t.Fatalf("recovery sequence after uncertain write: %d", len(reopened.lines))
			}
			if ack, err := reopened.Append(entry); err != nil || ack != 1 {
				t.Fatalf("reconciled append/replay: ack=%d error=%v", ack, err)
			}
		})
	}

	root := t.TempDir()
	store := openAttemptFixture(t, root)
	entry := attemptRecordFixture(1, attemptRecordInput, "stable")
	if _, err := store.Append(entry); err != nil {
		t.Fatal(err)
	}
	store.syncFile = func() error { return syscall.EIO }
	if ack, err := store.Append(entry); ack != 1 || err != nil {
		t.Fatalf("exact replay called Sync: ack=%d error=%v", ack, err)
	}
	store.syncFile = store.file.Sync
	if ack, err := store.Append(attemptRecordFixture(2, attemptRecordInput, "next")); ack != 2 || err != nil {
		t.Fatalf("exact replay poisoned later append: ack=%d error=%v", ack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAttemptFixture(t, root)
	if ack, err := reopened.Append(entry); err != nil || ack != 2 {
		t.Fatalf("replay after reopen: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsMetadataAndTerminalCoverage(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	entry := attemptRecordFixture(1, attemptRecordDiscussion, "part one")
	entry.Call, entry.Agent, entry.Role, entry.EventSource = "call-1", "helper-2", "critic", "claude"
	if ack, err := store.Append(entry); err != nil || ack != 1 {
		t.Fatalf("metadata append: ack=%d error=%v", ack, err)
	}
	coverage := attemptRecordFixture(2, attemptRecordCoverage, "")
	if ack, err := store.Append(coverage); err != nil || ack != 2 {
		t.Fatalf("coverage append: ack=%d error=%v", ack, err)
	}
	if ack, err := store.Append(entry); err != nil || ack != 2 {
		t.Fatalf("replay before coverage: ack=%d error=%v", ack, err)
	}
	if ack, err := store.Append(attemptRecordFixture(3, attemptRecordDiscussion, "late")); ack != 0 || !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("appended after terminal coverage: ack=%d error=%v", ack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAttemptFixture(t, root)
	if ack, err := reopened.Append(coverage); err != nil || ack != 2 {
		t.Fatalf("coverage replay after reopen: ack=%d error=%v", ack, err)
	}
	if ack, err := reopened.Append(attemptRecordFixture(3, attemptRecordDiscussion, "late")); ack != 0 || !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("reopened store appended after coverage: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsRejectCredentialInMetadata(t *testing.T) {
	store := openAttemptFixture(t, t.TempDir())
	for _, value := range []string{"secret", "<"} {
		entry := attemptRecordFixture(1, attemptRecordDiscussion, "safe")
		entry.Call = value
		key := "secret"
		if value == "<" {
			key = "u003c" // json.Marshal escapes '<' into this text.
		}
		if ack, err := store.Append(entry, key); ack != 0 || err == nil {
			t.Fatalf("credential in metadata accepted: value=%q ack=%d error=%v", value, ack, err)
		}
	}
	entry := attemptRecordFixture(1, attemptRecordDiscussion, "safe")
	entry.Call = "safe"
	if ack, err := store.Append(entry, "call"); ack != 0 || err == nil {
		t.Fatalf("credential matching a metadata field name was retained: ack=%d error=%v", ack, err)
	}
	marker := attemptRecordFixture(1, attemptRecordCoverage, "")
	marker.Agent = "helper"
	if ack, err := store.Append(marker); ack != 0 || err == nil {
		t.Fatalf("marker metadata accepted: ack=%d error=%v", ack, err)
	}
	marker.Agent = ""
	marker.Text = "incomplete"
	if ack, err := store.Append(marker, "incomplete"); ack != 0 || err == nil {
		t.Fatalf("nonempty coverage was accepted or redacted to complete: ack=%d error=%v", ack, err)
	}
	reference := attemptRecordFixture(1, attemptRecordReviewReference, "")
	reference.More = true
	if ack, err := store.Append(reference); ack != 0 || err == nil {
		t.Fatalf("empty reference claimed a continuation: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsCoverageRequiresFinishedContinuation(t *testing.T) {
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	first := attemptRecordFixture(1, attemptRecordDiscussion, "first")
	first.Call, first.Agent, first.More = "call-1", "helper-1", true
	if ack, err := store.Append(first); err != nil || ack != 1 {
		t.Fatalf("first fragment: ack=%d error=%v", ack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openAttemptFixture(t, root)
	if ack, err := store.Append(attemptRecordFixture(2, attemptRecordCoverage, "")); ack != 0 || !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("unfinished fragment marked complete after reopen: ack=%d error=%v", ack, err)
	}
	other := attemptRecordFixture(2, attemptRecordDiscussion, "unrelated")
	if ack, err := store.Append(other); ack != 0 || !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("unrelated event ended continuation: ack=%d error=%v", ack, err)
	}
	last := attemptRecordFixture(2, attemptRecordDiscussion, "last")
	last.Call, last.Agent = first.Call, first.Agent
	if ack, err := store.Append(last); err != nil || ack != 2 {
		t.Fatalf("last fragment: ack=%d error=%v", ack, err)
	}
	if ack, err := store.Append(attemptRecordFixture(3, attemptRecordCoverage, "")); err != nil || ack != 3 {
		t.Fatalf("complete fragments lacked coverage: ack=%d error=%v", ack, err)
	}
}

func TestAttemptRecordsRejectCredentialJoinedAcrossFragments(t *testing.T) {
	for _, test := range []struct {
		name, secret, first, second string
	}{
		{"literal", "fixture-secret-123", "fixture-", "secret-123"},
		{"HTML encoded", "alpha&beta", "alpha&am", "p;beta"},
		{"JSON escaped", "u003cx", "<", "x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := openAttemptFixture(t, root)
			first := attemptRecordFixture(1, attemptRecordDiscussion, test.first)
			first.Call, first.More = "call-1", true
			if ack, err := store.Append(first, test.secret); err != nil || ack != 1 {
				t.Fatalf("first fragment: ack=%d error=%v", ack, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openAttemptFixture(t, root) // Recover the fragment tail from saved JSONL.
			second := attemptRecordFixture(2, attemptRecordDiscussion, test.second)
			second.Call = first.Call
			if ack, err := store.Append(second, test.secret); ack != 0 || err == nil {
				t.Fatalf("joined credential was acknowledged: ack=%d error=%v", ack, err)
			}
			path := filepath.Join(root, "attempt-records", "attempt.jsonl")
			if saved, err := os.ReadFile(path); err != nil || bytes.Count(saved, []byte{'\n'}) != 1 || bytes.Contains(saved, []byte(test.secret)) {
				t.Fatalf("rejected fragment was retained: data=%q error=%v", saved, err)
			}
			second.Text = "safe continuation"
			if ack, err := store.Append(second, test.secret); err != nil || ack != 2 {
				t.Fatalf("safe continuation after rejection: ack=%d error=%v", ack, err)
			}
		})
	}
}

func TestAttemptRecordsRejectLongEncodedCredentialAcrossFragments(t *testing.T) {
	encoded := "&#x" + strings.Repeat("0", 150000) + "41;&#x42;"
	if got := html.UnescapeString(encoded); got != "AB" {
		t.Fatalf("fixture does not decode to configured key: %q", got)
	}
	root := t.TempDir()
	store := openAttemptFixture(t, root)
	var sequence uint64 = 1
	for len(encoded) != 0 {
		part := encoded[:min(len(encoded), maxAttemptRecordTextBytes)]
		encoded = encoded[len(part):]
		record := attemptRecordFixture(sequence, attemptRecordDiscussion, part)
		record.Call, record.More = "call-1", len(encoded) != 0
		ack, err := store.Append(record, "AB")
		if len(encoded) == 0 {
			if ack != 0 || err == nil {
				t.Fatalf("completed encoded credential was acknowledged: ack=%d error=%v", ack, err)
			}
			break
		}
		if ack != sequence || err != nil {
			t.Fatalf("prefix fragment %d: ack=%d error=%v", sequence, ack, err)
		}
		if sequence == 3 {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openAttemptFixture(t, root)
		}
		sequence++
	}
	path := filepath.Join(root, "attempt-records", "attempt.jsonl")
	if saved, err := os.ReadFile(path); err != nil || bytes.Count(saved, []byte{'\n'}) != int(sequence-1) {
		t.Fatalf("rejected fragment changed durable highwater: bytes=%d error=%v", len(saved), err)
	}
}

func TestAttemptRecordsLogicalEventCapAcrossReopen(t *testing.T) {
	const capBytes = 1000
	for _, test := range []struct {
		name       string
		finalBytes int
		wantFull   bool
	}{
		{"exact cap", 500, false},
		{"one byte over", 501, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := openAttemptFixture(t, root)
			store.eventLimit = capBytes
			first := attemptRecordFixture(1, attemptRecordDiscussion, strings.Repeat("x", 500))
			first.Call, first.More = "call-1", true
			if ack, err := store.Append(first); err != nil || ack != 1 {
				t.Fatalf("first fragment: ack=%d error=%v", ack, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openAttemptFixture(t, root)
			store.eventLimit = capBytes
			last := attemptRecordFixture(2, attemptRecordDiscussion, strings.Repeat("y", test.finalBytes))
			last.Call = first.Call
			ack, err := store.Append(last)
			if test.wantFull {
				if ack != 0 || !errors.Is(err, errAttemptRecordsFull) {
					t.Fatalf("over-cap fragment: ack=%d error=%v", ack, err)
				}
			} else if ack != 2 || err != nil {
				t.Fatalf("exact-cap fragment: ack=%d error=%v", ack, err)
			}
			path := filepath.Join(root, "attempt-records", "attempt.jsonl")
			wantLines := 2
			if test.wantFull {
				wantLines = 1
			}
			if saved, err := os.ReadFile(path); err != nil || bytes.Count(saved, []byte{'\n'}) != wantLines {
				t.Fatalf("durable highwater after cap check: lines=%d error=%v", bytes.Count(saved, []byte{'\n'}), err)
			}
		})
	}
}

func TestAttemptRecordsMaximumValidLineFits(t *testing.T) {
	root := t.TempDir()
	record := attemptRecordFixture(1, attemptRecordDiscussion, strings.Repeat("\x01", maxAttemptRecordTextBytes))
	record.Identity.JobID = strings.Repeat("j", 100)
	record.Identity.AttemptID = strings.Repeat("a", 100)
	record.Identity.ExecutorID = strings.Repeat("e", 100)
	record.Identity.Repository = strings.Repeat("o", 100) + "/" + strings.Repeat("r", 100)
	record.Identity.RepositoryID = math.MaxInt64
	record.Identity.OwnerID = math.MaxInt64
	record.Identity.Requester = strings.Repeat("u", 100) + "[bot]"
	record.Identity.RequesterID = math.MaxInt64
	record.Identity.Number = math.MaxInt
	record.Identity.SubjectType = "PullRequest"
	record.Call = strings.Repeat("\x01", 128)
	record.Agent = strings.Repeat("\x01", 128)
	record.Role = strings.Repeat("\x01", 128)
	record.EventSource = strings.Repeat("\x01", 128)
	record.More = true
	store, err := openAttemptRecordStore(root, record.Identity, record.SourceRef)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if ack, err := store.Append(record); err != nil || ack != 1 {
		t.Fatalf("maximum first fragment: ack=%d error=%v", ack, err)
	}
	record.Sequence = 999999 // Conservative six-digit upper bound under the 64 MiB store cap.
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 > maxAttemptRecordLineBytes {
		t.Fatalf("maximum valid line = %d, cap = %d", len(encoded)+1, maxAttemptRecordLineBytes)
	}
}
