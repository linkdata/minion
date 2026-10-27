package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcceptedResultSidecarLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()

	if _, err := store.ReadAcceptedResult(1); err != os.ErrNotExist {
		t.Fatalf("missing sidecar: got %v, want os.ErrNotExist", err)
	}
	if found, err := store.HasAcceptedResult(); found || err != nil {
		t.Fatalf("missing sidecar exists=%t error=%v", found, err)
	}
	stateBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := bytes.Repeat([]byte("accepted result data"), 1024)
	if err := store.WithLock(func() error { return store.WriteAcceptedResult(result) }); err != nil {
		t.Fatal(err)
	}
	if saved, err := os.ReadFile(path + ".result"); err != nil || !bytes.Equal(saved, result) {
		t.Fatalf("sidecar data = %d bytes, %v; want %d bytes", len(saved), err, len(result))
	}
	if stateAfter, err := os.ReadFile(path); err != nil || !bytes.Equal(stateAfter, stateBefore) {
		t.Fatalf("writing sidecar changed state: %v", err)
	}
	if found, err := store.HasAcceptedResult(); !found || err != nil {
		t.Fatalf("written sidecar exists=%t error=%v", found, err)
	}
	if got, err := store.ReadAcceptedResult(int64(len(result) - 1)); err == nil || got != nil {
		t.Fatalf("oversized sidecar read = %d bytes, %v; want size error", len(got), err)
	}
	if got, err := store.ReadAcceptedResult(int64(len(result))); err != nil || !bytes.Equal(got, result) {
		t.Fatalf("bounded sidecar read = %d bytes, %v; want %d bytes", len(got), err, len(result))
	}
	if _, err := store.ReadAcceptedResult(-1); err == nil {
		t.Fatal("negative size limit accepted")
	}

	replacement := []byte("replacement")
	if err := store.WithLock(func() error { return store.WriteAcceptedResult(replacement) }); err != nil {
		t.Fatal(err)
	}
	store.Data.Processed["job"] = time.Now().UTC()
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadAcceptedResult(int64(len(replacement))); err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement after state Save = %q, %v", got, err)
	}
	if err := store.WithLock(store.ClearAcceptedResult); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadAcceptedResult(1); err != os.ErrNotExist {
		t.Fatalf("cleared sidecar: got %v, want os.ErrNotExist", err)
	}
	if found, err := store.HasAcceptedResult(); found || err != nil {
		t.Fatalf("cleared sidecar exists=%t error=%v", found, err)
	}
	if err := store.WithLock(store.ClearAcceptedResult); err != nil {
		t.Fatalf("repeat clear: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 2 {
		t.Fatalf("sidecar or temporary files retained: %v, %v", entries, err)
	}
}
