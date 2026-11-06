package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func liveConfigFixture(t *testing.T, remote bool) (*adminLiveConfig, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{BotLogin: "bot", Adversarial: "codex", Repositories: []config.Repository{{Name: "owner/repo", ID: 11, OwnerID: 22, Actions: []string{"review"}}}}
	if remote {
		cfg.RemoteExecutor = &config.RemoteExecutorConfig{ExecutorID: "worker", Listen: "127.0.0.1:9999", TLSCertFile: "cert", TLSKeyFile: "key", Token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		cfg.MainSettings = config.RoleModelConfig{Model: "model", Effort: "medium"}
		cfg.CriticSettings = config.RoleModelConfig{Model: "critic", Effort: "medium"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, loaded)
	if err != nil {
		t.Fatal(err)
	}
	return live, path
}

func TestAdminLiveConfigPersistsThenAppliesAtBoundary(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	if err := live.save(func(cfg *config.Config) error {
		cfg.Repositories[0].AllowedActors = []string{"writer"}
		cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
		cfg.CriticStopping.RequiredCleanRounds = policyRoundsForTest(2)
		cfg.AIInstructions = "Read carefully."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !live.snapshot().Pending || live.current().Repositories[0].AllowedActors != nil {
		t.Fatal("saved policy became active before the monitor boundary")
	}
	select {
	case <-live.updates:
	default:
		t.Fatal("monitor was not signaled")
	}
	saved, err := config.Load(path)
	if err != nil || len(saved.Repositories[0].AllowedActors) != 1 || saved.AIInstructions != "Read carefully." || saved.RequiredCleanRounds(11, 7) != 2 {
		t.Fatalf("saved config = %+v, %v", saved, err)
	}
	candidate, generation := live.next()
	live.applied(candidate, generation, nil)
	if live.snapshot().Pending || live.current().Repositories[0].AllowedActors[0] != "writer" || live.current().AIInstructions != "Read carefully." {
		t.Fatal("monitor boundary did not activate saved settings")
	}
}

func TestAdminLiveConfigRejectsRestartOnlyChanges(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error { cfg.BotLogin = "other"; return nil }); err == nil {
		t.Fatal("live editor changed bot identity")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected update changed disk: %v", err)
	}
}

func TestAdminLiveConfigRejectsRemoteSkillChanges(t *testing.T) {
	live, path := liveConfigFixture(t, true)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.AlwaysOnSkills = append(cfg.AlwaysOnSkills, "https://github.com/example/skills/tree/main/skills/extra")
		return nil
	}); err == nil {
		t.Fatal("remote worker skill selection changed live")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected remote update changed disk: %v", err)
	}
}

func TestAdminLiveConfigRejectsRemoteCommandExpansion(t *testing.T) {
	live, _ := liveConfigFixture(t, true)
	if err := live.save(func(cfg *config.Config) error {
		cfg.Repositories = append(cfg.Repositories, config.Repository{Name: "owner/second", ID: 12, OwnerID: 22, Actions: []string{"fix"}})
		return nil
	}); err == nil {
		t.Fatal("remote worker gained a command without re-selection")
	}
}

func TestAdminLiveConfigKeepsResolvedModels(t *testing.T) {
	_, path := liveConfigFixture(t, false)
	runtime, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Codex.Model = "resolved-model"
	runtime.Codex.ReasoningEffort = "medium"
	runtime.MainSettings = config.RoleModelConfig{Model: "resolved-model", Effort: "medium"}
	runtime.CriticSettings = config.RoleModelConfig{Model: "critic-model", Effort: "high"}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.AIInstructions = "Check the changed files."
		return nil
	}); err != nil {
		t.Fatalf("admin edit with resolved startup models: %v", err)
	}
	want, _ := json.Marshal(runtime.MainSettings)
	next, _ := live.next()
	got, _ := json.Marshal(next.MainSettings)
	if string(got) != string(want) || next.Codex.Model != "resolved-model" || next.CriticSettings.Model != "critic-model" {
		t.Fatalf("resolved models lost after admin edit: main=%+v critic=%+v codex=%+v", next.MainSettings, next.CriticSettings, next.Codex)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var beforeFields, afterFields map[string]json.RawMessage
	if err := json.Unmarshal(original, &beforeFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &afterFields); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "resolved-model") || string(beforeFields["commands"]) != string(afterFields["commands"]) || string(beforeFields["always_on_skills"]) != string(afterFields["always_on_skills"]) {
		t.Fatalf("runtime model or unrelated defaults were written to JSON: %s", data)
	}
}

func TestAdminLiveConfigEditsOmittedSkillDefaults(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	if err := live.save(func(cfg *config.Config) error {
		cfg.AlwaysOnSkills = append(cfg.AlwaysOnSkills, "https://github.com/owner/skills/tree/main/skills/extra")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.AlwaysOnSkills) != 3 || saved.AlwaysOnSkills[2] != "https://github.com/owner/skills/tree/main/skills/extra" {
		t.Fatalf("adding a skill dropped defaults: %v", saved.AlwaysOnSkills)
	}
	if err := live.save(func(cfg *config.Config) error {
		mapping := cfg.Commands["review"]
		mapping.Skills = append(mapping.Skills, "https://github.com/owner/skills/tree/main/skills/reviewer")
		cfg.Commands["review"] = mapping
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Commands["review"].Skills) != 2 || saved.Commands["review"].Skills[1] != "https://github.com/owner/skills/tree/main/skills/reviewer" {
		t.Fatalf("editing a default command dropped its skills: %+v", saved.Commands["review"])
	}
}

func policyRoundsForTest(value int) *int { return &value }
