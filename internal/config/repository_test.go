package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAddRepositoryPreservesSettingsAndExistingRestrictions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{"bot_login":"linkdataminion","main":"claude","adversarial":"codex","critic_max_rounds":4,"codex":{"model":"user-model","reasoning_effort":"user-effort","api_key":"dummy-codex-key"},"claude":{"model":"sonnet","effort":"high","api_key":"dummy-claude-key","allow_all_unix_sockets":false},"state_path":"relative/state.json","repositories":[{"name":"example-owner/example-repo","id":1,"owner_id":2,"actions":["review"],"allowed_actors":["alice"],"allowed_actor_ids":{"alice":7}}]}`)
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
	if raw.Main != "claude" || raw.Adversarial != "codex" || raw.CriticMaxRounds != 4 || raw.Claude.Model != "sonnet" || raw.Claude.Effort != "high" {
		t.Fatalf("changed model or critic settings: %+v", raw)
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
