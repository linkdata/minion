package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAddRepositoryPreservesSettingsAndExistingRestrictions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{"bot_login":"linkdataminion","main":"claude","adversarial":"codex","critic_stopping":{"max_rounds":4,"nonblocking_after_round":0,"consecutive_no_blocker_rounds":0},"codex":{"model":"user-model","reasoning_effort":"user-effort","api_key":"dummy-codex-key"},"claude":{"model":"sonnet","effort":"high","api_key":"dummy-claude-key","allow_all_unix_sockets":false},"state_path":"relative/state.json","repositories":[{"name":"example-owner/example-repo","id":1,"owner_id":2,"actions":["review"],"allowed_actors":["alice"],"allowed_actor_ids":{"alice":7}}]}`)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	repo := Repository{Name: "Example-Owner/Second-Repo", ID: 3, OwnerID: 2, Actions: []string{"review", "fix", "audit"}}
	added, err := AddRepository(path, repo)
	if err != nil || added.Name != "example-owner/second-repo" {
		t.Fatalf("add repository: %+v, %v", added, err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw Config
	if err := json.Unmarshal(updated, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Codex.Model != "user-model" || raw.Codex.ReasoningEffort != "user-effort" || raw.StatePath != "relative/state.json" || raw.WorkRoot != "" || raw.PollIntervalSeconds != 0 {
		t.Fatalf("rewrote unrelated settings or defaults: %+v", raw)
	}
	if raw.Main != "claude" || raw.Adversarial != "codex" || raw.CriticStopping.MaxRounds != 4 || raw.Claude.Model != "sonnet" || raw.Claude.Effort != "high" {
		t.Fatalf("changed model or critic settings: %+v", raw)
	}
	if raw.CriticStopping.NonblockingAfterRound == nil || *raw.CriticStopping.NonblockingAfterRound != 0 || raw.CriticStopping.ConsecutiveNoBlockerRounds == nil || *raw.CriticStopping.ConsecutiveNoBlockerRounds != 0 {
		t.Fatalf("changed explicit zero critic stopping settings: %+v", raw.CriticStopping)
	}
	if raw.Codex.APIKey != "dummy-codex-key" || raw.Claude.APIKey != "dummy-claude-key" || raw.Claude.AllowAllUnixSockets == nil || *raw.Claude.AllowAllUnixSockets {
		t.Fatal("changed provider credentials or Unix-socket setting")
	}
	if len(raw.Repositories) != 2 || len(raw.Repositories[0].Actions) != 1 || raw.Repositories[0].AllowedActors[0] != "alice" {
		t.Fatalf("changed repository restrictions: %+v", raw.Repositories)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("changed config permissions: %v, %v", info, err)
	}
	for _, name := range []string{"EXAMPLE-OWNER/EXAMPLE-REPO", "example-owner/second-repo"} {
		if _, err := AddRepository(path, Repository{Name: name, Actions: []string{"fix"}}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(updated, after) {
		t.Fatalf("duplicate registration changed config: %s, %v", after, err)
	}
	if _, err := AddRepository(path, Repository{Name: "example-owner/../bad", Actions: []string{"fix"}}); err == nil {
		t.Fatal("accepted invalid repository")
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(updated, after) {
		t.Fatalf("failed validation changed config: %s, %v", after, err)
	}
}

func TestAddRepositoryPreservesCriticStoppingValues(t *testing.T) {
	for _, tt := range []struct {
		name, fields string
	}{
		{"omitted section", ""},
		{"omitted threshold and streak", `,"critic_stopping":{"max_rounds":4}`},
		{"zero maximum", `,"critic_stopping":{"max_rounds":0}`},
		{"explicit zero threshold", `,"critic_stopping":{"max_rounds":4,"nonblocking_after_round":0}`},
		{"explicit zero streak", `,"critic_stopping":{"nonblocking_after_round":1,"consecutive_no_blocker_rounds":0}`},
		{"explicit streak above maximum", `,"critic_stopping":{"max_rounds":2,"nonblocking_after_round":2,"consecutive_no_blocker_rounds":20}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + tt.fields + `}`)
			original, err := decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := AddRepository(path, Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
				t.Fatal(err)
			}
			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := decode(bytes.NewReader(updated))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raw.CriticStopping, original.CriticStopping) {
				t.Fatalf("rewrote critic stopping settings: got %+v, want %+v", raw.CriticStopping, original.CriticStopping)
			}
		})
	}
}
