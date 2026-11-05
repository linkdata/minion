package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
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

func TestUserLinksGitLabRootHasOneGitHubID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.link")
	links, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	gitlab := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "person"}).key()
	github := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	otherGitHub := (userIdentity{Provider: "github", Host: "github.com", Subject: "43"}).key()
	if err := links.link(gitlab, github); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{gitlab, github} {
		if id, err := links.githubID(key); err != nil || id != 42 {
			t.Fatalf("GitHub ID for %q: %d, %v", key, id, err)
		}
	}
	if err := links.link(gitlab, otherGitHub); !errors.Is(err, errUserLinkConflict) {
		t.Fatalf("second GitHub identity: %v", err)
	}
	loaded, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := loaded.githubID(gitlab); err != nil || id != 42 {
		t.Fatalf("persisted GitHub ID: %d, %v", id, err)
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
		if err := store.recoverAfterWriteFailure(errors.New("synthetic write failure"), next, nil); err == nil || store.account(first) != first {
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
		if err := store.recoverAfterWriteFailure(errors.New("synthetic post-rename failure"), next, nil); err == nil || store.account(second) != first {
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
		if err := store.recoverAfterWriteFailure(errors.New("synthetic unexpected replacement"), next, nil); err == nil || store.account(first) != "" {
			t.Fatalf("unexpected visible map did not fail closed: error=%v account=%q", err, store.account(first))
		}
	})
}

func TestUserPreferencesFollowLinkedAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.link")
	store, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	github := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	gitlab := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "8"}).key()
	first := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "first", Effort: "low"}}
	second := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "second", Effort: "medium"}}
	if err := store.setPreferences(github, first); err != nil {
		t.Fatal(err)
	}
	store.lastPreferenceWrite = time.Now().Add(-preferenceWriteInterval)
	if err := store.setPreferences(gitlab, second); err != nil {
		t.Fatal(err)
	}
	if err := store.link(github, gitlab); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{github, gitlab} {
		got, err := store.preferencesFor(key)
		if err != nil || got != first {
			t.Fatalf("initiating root preference did not win for %q: %+v, %v", key, got, err)
		}
	}
	reopened, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.preferencesForGitHubID(7); err != nil || got != first || len(reopened.preferences) != 1 {
		t.Fatalf("reopened linked preferences: %+v, %v; roots=%d", got, err, len(reopened.preferences))
	}
	if err := reopened.setPreferences(gitlab, second); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.preferencesForGitHubID(7); err != nil || got != second {
		t.Fatalf("alias write did not reach canonical preference: %+v, %v", got, err)
	}
}

func TestUserPreferencesBoundChangedWritesAndSkipIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	store, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	key := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	first := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "first", Effort: "low"}}
	second := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "second", Effort: "medium"}}
	if err := store.setPreferences(key, config.UserModelPreferences{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged default created a file: %v", err)
	}
	if err := store.setPreferences(key, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setPreferences(key, first); err != nil {
		t.Fatalf("identical choice was rate limited: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("identical choice rewrote the file: %v", err)
	}
	if store.lastPreferenceWrite.IsZero() {
		t.Fatal("successful preference write did not record its limit")
	}
	store.lastPreferenceWrite = time.Now()
	if err := store.setPreferences(key, second); !errors.Is(err, errPreferenceRateLimited) {
		t.Fatalf("changed choice bypassed write limit: %v", err)
	}
	if got, err := store.preferencesFor(key); err != nil || got != first {
		t.Fatalf("rate-limited write changed preference: %+v, %v", got, err)
	}
	store.lastPreferenceWrite = time.Now().Add(-2 * preferenceWriteInterval)
	if err := store.setPreferences(key, second); err != nil {
		t.Fatalf("changed choice after interval: %v", err)
	}
}

func TestUserPreferencesLeaveNextWriteSlotForAnotherAccount(t *testing.T) {
	store, err := openUserLinks(filepath.Join(t.TempDir(), "links.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	alias := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "9"}).key()
	other := (userIdentity{Provider: "github", Host: "github.com", Subject: "8"}).key()
	low := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "model", Effort: "low"}}
	high := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "model", Effort: "high"}}
	if err := store.setPreferences(alias, low); err != nil {
		t.Fatal(err)
	}
	if err := store.link(first, alias); err != nil {
		t.Fatal(err)
	}
	store.lastPreferenceWrite = time.Now().Add(-preferenceWriteInterval)
	for _, key := range []string{first, alias} {
		var limited preferenceRateLimitError
		if err := store.setPreferences(key, high); !errors.As(err, &limited) || limited.retryAfterSeconds != 1 {
			t.Fatalf("linked account took consecutive slot: key=%q error=%v wait=%d", key, err, limited.retryAfterSeconds)
		}
	}
	if err := store.setPreferences(other, high); err != nil {
		t.Fatalf("other account could not use next slot: %v", err)
	}
	store.lastPreferenceWrite = time.Now().Add(-preferenceWriteInterval)
	if err := store.setPreferences(first, high); err != nil {
		t.Fatalf("first account could not write after other account: %v", err)
	}
}

func TestUserPreferencesLimitDistinctAndLinkedAccounts(t *testing.T) {
	store, err := openUserLinks(filepath.Join(t.TempDir(), "links.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	second := (userIdentity{Provider: "github", Host: "github.com", Subject: "8"}).key()
	alias := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "9"}).key()
	if err := store.link(first, alias); err != nil {
		t.Fatal(err)
	}
	low := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "model", Effort: "low"}}
	high := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "model", Effort: "high"}}
	if err := store.setPreferences(first, low); err != nil {
		t.Fatal(err)
	}
	if store.lastPreferenceWrite.IsZero() {
		t.Fatal("preference write did not record the aggregate limit")
	}
	store.lastPreferenceWrite = time.Now()
	if err := store.setPreferences(second, high); !errors.Is(err, errPreferenceRateLimited) {
		t.Fatalf("distinct account bypassed aggregate write limit: %v", err)
	}
	if err := store.setPreferences(second, config.UserModelPreferences{}); err != nil {
		t.Fatalf("unchanged default was rate limited: %v", err)
	}
	if got, err := store.preferencesFor(second); err != nil || got != (config.UserModelPreferences{}) {
		t.Fatalf("rate-limited account changed preference: %+v, %v", got, err)
	}
	store.lastPreferenceWrite = time.Now().Add(-preferenceWriteInterval)
	if err := store.setPreferences(second, high); err != nil {
		t.Fatalf("distinct account did not succeed after aggregate interval: %v", err)
	}
	store.lastPreferenceWrite = time.Now()
	if err := store.setPreferences(alias, high); !errors.Is(err, errPreferenceRateLimited) {
		t.Fatalf("linked alias bypassed the aggregate limit: %v", err)
	}
	if got, err := store.preferencesFor(second); err != nil || got != high {
		t.Fatalf("other account changed under rate limit: %+v, %v", got, err)
	}
	store.lastPreferenceWrite = time.Now().Add(-preferenceWriteInterval)
	if err := store.setPreferences(alias, high); err != nil {
		t.Fatalf("linked account did not succeed after aggregate interval: %v", err)
	}
}

func TestUserPreferencesLinkPreservesAggregateWriteLimit(t *testing.T) {
	store, err := openUserLinks(filepath.Join(t.TempDir(), "links.json"))
	if err != nil {
		t.Fatal(err)
	}
	current := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	proved := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "9"}).key()
	low := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "model", Effort: "low"}}
	high := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "model", Effort: "high"}}
	if err := store.setPreferences(proved, low); err != nil {
		t.Fatal(err)
	}
	if store.lastPreferenceWrite.IsZero() {
		t.Fatal("preference write did not record the aggregate limit")
	}
	lastWrite := store.lastPreferenceWrite
	if err := store.link(current, proved); err != nil {
		t.Fatal(err)
	}
	if !store.lastPreferenceWrite.Equal(lastWrite) {
		t.Fatal("link changed the aggregate preference write limit")
	}
	for _, key := range []string{current, proved} {
		store.lastPreferenceWrite = time.Now()
		if err := store.setPreferences(key, high); !errors.Is(err, errPreferenceRateLimited) {
			t.Fatalf("linked account %q bypassed recent preference write: %v", key, err)
		}
	}
	if got, err := store.preferencesFor(current); err != nil || got != low {
		t.Fatalf("linked account lost its frozen choice: %+v, %v", got, err)
	}
}

func TestUserPreferencesReconcileBothMapsAfterAmbiguousWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	store, err := openUserLinks(path)
	if err != nil {
		t.Fatal(err)
	}
	github := (userIdentity{Provider: "github", Host: "github.com", Subject: "7"}).key()
	gitlab := (userIdentity{Provider: "gitlab", Host: "gitlab.example", Subject: "8"}).key()
	choice := config.UserModelPreferences{Main: config.RoleModelConfig{Model: "chosen", Effort: "low"}}
	links := map[string]string{gitlab: github}
	preferences := map[string]config.UserModelPreferences{github: choice}
	data := []byte(`{"version":1,"links":{"` + gitlab + `":"` + github + `"},"preferences":{"` + github + `":{"main":{"model":"chosen","effort":"low"}}}}`)
	if err := workspace.WriteAtomic(path, data); err != nil {
		t.Fatal(err)
	}
	if err := store.recoverAfterWriteFailure(errors.New("synthetic post-rename failure"), links, preferences); err == nil {
		t.Fatal("ambiguous write did not report its original error")
	}
	if got, err := store.preferencesFor(gitlab); err != nil || got != choice || store.account(gitlab) != github {
		t.Fatalf("post-rename maps were not reconciled together: %+v, %v", got, err)
	}
	other := config.UserModelPreferences{Critic: config.RoleModelConfig{Model: "critic", Effort: "medium"}}
	if err := store.setPreferences(gitlab, other); err != nil {
		t.Fatal(err)
	}
	if got, err := store.preferencesFor(github); err != nil || got != other {
		t.Fatalf("later write after reconciliation failed: %+v, %v", got, err)
	}
}

func TestUserPreferencesRejectPartialStoredChoice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"links":{},"preferences":{"github:github.com:7":{"main":{"model":"only"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openUserLinks(path); err == nil {
		t.Fatal("partial user preference was accepted")
	}
}
