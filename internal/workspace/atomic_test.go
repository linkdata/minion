package workspace

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicReplacesPrivateRecordAndCleansTemporaryFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "record.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := WriteAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Fatalf("new record=%q,%v", data, err)
	}
	if data, err := io.ReadAll(old); err != nil || string(data) != "old" {
		t.Fatalf("replacement changed open prior record=%q,%v", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("record permissions=%v,%v", info, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("temporary record leaked: %v,%v", entries, err)
	}
}

func TestWriteAtomicReportsSyncFailureAfterReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "record.json")
	if err := WriteAtomic(path, []byte("retained"), filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sync error lost: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "retained" {
		t.Fatalf("committed record lost after sync error=%q,%v", data, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("temporary record leaked: %v,%v", entries, err)
	}
}
