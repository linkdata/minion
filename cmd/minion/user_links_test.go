package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestUserIdentityKeyPreservesProviderSubject(t *testing.T) {
	for _, id := range []userIdentity{
		{Provider: "github", Host: "github.com", Subject: "9007199254740993"},
		{Provider: "gitlab", Host: "gitlab.example:8443", Subject: "team/user:42"},
		{Provider: "gitlab", Host: "[::1]:8443", Subject: "team/user:42"},
	} {
		if key := id.key(); !validUserKey(key) {
			t.Fatalf("valid subject rejected: %q", key)
		}
	}
	for _, key := range []string{
		"github:github.com:0", "github:github.com:01", "github:github.com:1.0",
		"github:other.example:42", "gitlab:gitlab.example:%00", "gitlab:gitlab.example:%FF",
		"gitlab:gitlab.example:" + strings.Repeat("a", 256),
	} {
		if validUserKey(key) {
			t.Fatalf("invalid subject accepted: %q", key)
		}
	}
}

func TestUserLinksProvenAccountsAndHostIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.link")
	store, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	first := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	second := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "user:7"}).key()
	third := (userIdentity{Provider: "gitlab", Host: "other.example", Subject: "user:7"}).key()
	fourth := (userIdentity{Provider: "github", Host: "github.com", Subject: "8"}).key()
	fifth := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "other/subject"}).key()
	if store.account(first) == store.account(second) || store.account(second) == store.account(third) {
		t.Fatal("provider or host collision")
	}
	if err := store.link(first, second); err != nil {
		t.Fatal(err)
	}
	if err := store.link(third, fourth); err != nil {
		t.Fatal(err)
	}
	if err := store.link(second, fourth); err == nil {
		t.Fatal("two linked accounts were merged")
	}
	if err := store.link(second, first); err != nil {
		t.Fatalf("idempotent relink: %v", err)
	}
	if err := store.link(second, fifth); err != nil {
		t.Fatalf("link fresh identity: %v", err)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("link file permissions: info=%v error=%v", info, err)
	}
	for _, key := range []string{first, second, fifth} {
		if store.account(key) != first {
			t.Fatalf("account for %q = %q, want %q", key, store.account(key), first)
		}
	}
	if store.account(third) != third || store.account(fourth) != third {
		t.Fatal("rejected merge changed the other account")
	}
	reopened, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{first, second, fifth} {
		if reopened.account(key) != first {
			t.Fatalf("restarted account for %q = %q", key, reopened.account(key))
		}
	}
	if reopened.account(third) != third || reopened.account(fourth) != third {
		t.Fatal("restarted cross-account merge")
	}
}

func TestUserLinksRejectCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.link")
	for _, data := range []string{
		`{"version":1,"links":{"github:github.com:7":"gitlab:gitlab.example:7","gitlab:gitlab.example:7":"github:github.com:7"}}`,
		`{"version":1,"links":null}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openUserLinks(path); err == nil {
			t.Fatalf("corrupt links were accepted: %s", data)
		}
	}
}

func TestUserLinksRecoverAfterWriteFailure(t *testing.T) {
	first := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	second := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "8"}).key()
	third := (userIdentity{Provider: "github", Host: "github.com", Subject: "9"}).key()
	next := map[string]string{second: first}
	t.Run("before replacement", func(t *testing.T) {
		store, err := openUserLinks(filepath.Join(t.TempDir(), "links.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.recoverAfterWriteFailure(errors.New("synthetic write failure"), next); err == nil || store.account(first) != first {
			t.Fatalf("recover old state: error=%v account=%q", err, store.account(first))
		}
		if err := store.link(first, second); err != nil {
			t.Fatalf("later link failed after a recoverable write error: %v", err)
		}
	})
	t.Run("after replacement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "links.json")
		store, err := openUserLinks(path)
		if err != nil {
			t.Fatal(err)
		}
		data := []byte(`{"version":1,"links":{"` + second + `":"` + first + `"}}`)
		if err := workspace.WriteAtomic(path, data); err != nil {
			t.Fatal(err)
		}
		if err := store.recoverAfterWriteFailure(errors.New("synthetic post-rename failure"), next); err == nil || store.account(second) != first {
			t.Fatalf("recover visible replacement: error=%v account=%q", err, store.account(second))
		}
		if err := store.link(second, third); err != nil || store.account(third) != first {
			t.Fatalf("later link after reconciliation: error=%v account=%q", err, store.account(third))
		}
	})
	t.Run("unexpected replacement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "links.json")
		store, err := openUserLinks(path)
		if err != nil {
			t.Fatal(err)
		}
		data := []byte(`{"version":1,"links":{"` + third + `":"` + first + `"}}`)
		if err := workspace.WriteAtomic(path, data); err != nil {
			t.Fatal(err)
		}
		if err := store.recoverAfterWriteFailure(errors.New("synthetic unexpected replacement"), next); err == nil || store.account(first) != "" {
			t.Fatalf("unexpected visible map did not fail closed: error=%v account=%q", err, store.account(first))
		}
	})
}
