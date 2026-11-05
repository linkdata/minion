package main

import (
	"bytes"
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
		{Provider: "github", Host: "github.com", Subject: "9223372036854775807"},
		{Provider: "gitlab", Host: "gitlab.example:8443", Subject: "team/user:42"},
		{Provider: "gitlab", Host: "[::1]:8443", Subject: "team/user:42"},
	} {
		if key := id.key(); !validUserKey(key) {
			t.Fatalf("valid subject rejected: %q", key)
		}
	}
	for _, key := range []string{
		"github:github.com:0", "github:github.com:01", "github:github.com:1.0",
		"github:github.com:9223372036854775808",
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
	if err := store.link(first, fourth); !errors.Is(err, errUserLinkConflict) {
		t.Fatalf("direct same-provider link: %v", err)
	}
	if err := store.link(first, second); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{fourth, fifth, third} {
		if err := store.link(second, key); !errors.Is(err, errUserLinkConflict) {
			t.Fatalf("second identity for provider of %q: %v", key, err)
		}
		if store.account(key) != key {
			t.Fatalf("rejected identity %q was linked", key)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("rejected links changed durable state: error=%v", err)
	}
	if err := store.link(third, fourth); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{third, fourth} {
		if err := store.link(second, key); !errors.Is(err, errUserLinkConflict) {
			t.Fatalf("two linked accounts were merged through %q: %v", key, err)
		}
	}
	if err := store.link(second, first); err != nil {
		t.Fatalf("idempotent relink: %v", err)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("link file permissions: info=%v error=%v", info, err)
	}
	for _, key := range []string{first, second} {
		if store.account(key) != first {
			t.Fatalf("account for %q = %q, want %q", key, store.account(key), first)
		}
	}
	if store.account(third) != third || store.account(fourth) != third || store.account(fifth) != fifth {
		t.Fatal("rejected merge changed the other account")
	}
	reopened, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{first, second} {
		if reopened.account(key) != first {
			t.Fatalf("restarted account for %q = %q", key, reopened.account(key))
		}
	}
	if reopened.account(third) != third || reopened.account(fourth) != third || reopened.account(fifth) != fifth {
		t.Fatal("restarted cross-account merge")
	}
}

func TestUserLinksRejectCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.link")
	for _, data := range []string{
		`{"version":1,"links":{"github:github.com:7":"gitlab:gitlab.example:7","gitlab:gitlab.example:7":"github:github.com:7"}}`,
		`{"version":1,"links":{"github:github.com:8":"github:github.com:7"}}`,
		`{"version":1,"links":{"gitlab:gitlab.example:8":"github:github.com:7","gitlab:other.example:9":"github:github.com:7"}}`,
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
	fourth := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "10"}).key()
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
		if err := store.link(third, fourth); err != nil || store.account(fourth) != third {
			t.Fatalf("later link after reconciliation: error=%v account=%q", err, store.account(fourth))
		}
	})
	t.Run("unexpected replacement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "links.json")
		store, err := openUserLinks(path)
		if err != nil {
			t.Fatal(err)
		}
		data := []byte(`{"version":1,"links":{"` + third + `":"` + second + `"}}`)
		if err := workspace.WriteAtomic(path, data); err != nil {
			t.Fatal(err)
		}
		if err := store.recoverAfterWriteFailure(errors.New("synthetic unexpected replacement"), next); err == nil || store.account(first) != "" {
			t.Fatalf("unexpected visible map did not fail closed: error=%v account=%q", err, store.account(first))
		}
	})
}
