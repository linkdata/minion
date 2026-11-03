package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUserLinksMergeExistingGroupsAndHostIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.user-links.json")
	store, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	first := (userIdentity{Provider: "github", Host: "github.com", ID: 7}).key()
	second := (userIdentity{Provider: "gitlab", Host: "gitlab.example", ID: 7}).key()
	third := (userIdentity{Provider: "gitlab", Host: "other.example", ID: 7}).key()
	fourth := (userIdentity{Provider: "github", Host: "github.com", ID: 8}).key()
	if store.account(first) == store.account(second) || store.account(second) == store.account(third) {
		t.Fatal("provider or host collision")
	}
	if err := store.link(first, second); err != nil {
		t.Fatal(err)
	}
	if err := store.link(third, fourth); err != nil {
		t.Fatal(err)
	}
	if err := store.link(second, fourth); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("link file permissions: info=%v error=%v", info, err)
	}
	for _, key := range []string{first, second, third, fourth} {
		if store.account(key) != first {
			t.Fatalf("account for %q = %q, want %q", key, store.account(key), first)
		}
	}
	reopened, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{first, second, third, fourth} {
		if reopened.account(key) != first {
			t.Fatalf("restarted account for %q = %q", key, reopened.account(key))
		}
	}
}

func TestUserLinksRejectCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.user-links.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"links":{"github:github.com:7":"gitlab:gitlab.example:7","gitlab:gitlab.example:7":"github:github.com:7"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openUserLinks(path); err == nil {
		t.Fatal("cyclic links were accepted")
	}
}
