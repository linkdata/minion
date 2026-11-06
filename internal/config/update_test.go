package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAllowedActorsPolicySurvivesConfigUpdate(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		allowed     bool
		omitted     bool
	}{
		{"omitted", "", true, true},
		{"empty", `,"allowed_actors":[]`, true, false},
		{"deny all", `,"allowed_actors":[],"deny_all_actors":true`, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data := []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]` + tt.field + `}]}`)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			for phase := 0; phase < 2; phase++ {
				cfg, err := Load(path)
				if err != nil {
					t.Fatal(err)
				}
				repo := cfg.Repositories[0]
				if got := repo.AllowsActor("writer", 7); got != tt.allowed {
					t.Fatalf("phase %d: requester allowed = %t, want %t", phase, got, tt.allowed)
				}
				if got := repo.AllowedActors == nil; got != tt.omitted {
					t.Fatalf("phase %d: omitted = %t, want %t", phase, got, tt.omitted)
				}
				encoded, err := json.Marshal(cfg.Repositories[0])
				if err != nil {
					t.Fatal(err)
				}
				if tt.omitted && strings.Contains(string(encoded), `"allowed_actors"`) ||
					!tt.omitted && !strings.Contains(string(encoded), `"allowed_actors":[]`) ||
					strings.Contains(string(encoded), `"deny_all_actors":true`) != !tt.allowed {
					t.Fatalf("phase %d: encoded actors = %s", phase, encoded)
				}
				if phase == 0 {
					if _, err := Update(path, func(cfg *Config) error { cfg.Codex.Model = "configured"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestDenyAllActorsRejectsNamedAllowlist(t *testing.T) {
	for _, tt := range []struct {
		name   string
		actors []string
		ids    map[string]int64
	}{
		{"named actor", []string{"writer"}, map[string]int64{"writer": 7}},
		{"ID pin", nil, map[string]int64{"writer": 7}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", Repositories: []Repository{{
				Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"},
				AllowedActors: tt.actors, AllowedActorIDs: tt.ids, DenyAllActors: true,
			}}}
			if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "deny_all_actors") {
				t.Fatalf("conflicting actor policy was accepted: %v", err)
			}
		})
	}
}

func TestUpdatePreservesUnchangedSettingsAndRejectsInvalidChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"bot_login":"bot","codex":{"api_key":"configured-key"},"state_path":"relative/state.json","repositories":[{"name":"owner/first","id":1,"owner_id":2,"actions":["review"]}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Update(path, func(cfg *Config) error {
		cfg.Admin = &AdminConfig{Administrators: []AdminAccount{{Username: "admin", PasswordHash: AdminPasswordHashPlaceholder}}}
		return nil
	})
	if err != nil || cfg.Admin == nil || cfg.Admin.Listen != DefaultAdminListen {
		t.Fatalf("validated update: %+v, %v", cfg.Admin, err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte(`"state_path": "relative/state.json"`)) ||
		!bytes.Contains(updated, []byte(`"api_key": "configured-key"`)) ||
		bytes.Contains(updated, []byte(`"listen"`)) || bytes.Contains(updated, []byte(`"work_root"`)) {
		t.Fatalf("update persisted defaults or changed unrelated settings: %s", updated)
	}
	for _, mutate := range []func(*Config) error{
		func(cfg *Config) error { cfg.Repositories[0].AllowedActors = []string{"alice"}; return nil },
		func(cfg *Config) error { cfg.BotLogin = "changed"; return errors.New("reject") },
	} {
		if _, err := Update(path, mutate); err == nil {
			t.Fatal("invalid mutation succeeded")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(updated, after) {
			t.Fatalf("failed mutation changed config: %s, %v", after, err)
		}
	}
	if _, err := Update(path, func(*Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(updated, after) {
		t.Fatalf("no-op mutation changed config: %s, %v", after, err)
	}
}

func TestUpdateSerializesWithAddRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/first","id":1,"owner_id":2,"actions":["review"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errors := make(chan error, 12)
	for id := int64(2); id <= 13; id++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			repo := Repository{Name: fmt.Sprintf("owner/repo-%d", id), ID: id, OwnerID: 2, Actions: []string{"review"}}
			if id%2 == 0 {
				_, err := AddRepository(path, repo)
				errors <- err
				return
			}
			_, err := Update(path, func(cfg *Config) error {
				cfg.Repositories = append(cfg.Repositories, repo)
				return nil
			})
			errors <- err
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Repositories) != 13 {
		t.Fatalf("lost concurrent config update: %d repositories, %v", len(cfg.Repositories), err)
	}
}
