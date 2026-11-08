package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
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

func TestAdminLiveConfigAdmitRejectsPendingUpdate(t *testing.T) {
	live, _ := liveConfigFixture(t, false)
	if err := live.save(func(cfg *config.Config) error {
		cfg.AIInstructions = "Read carefully."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	err := live.admit(func() error {
		called = true
		return nil
	})
	if !errors.Is(err, monitor.ErrConfigUpdatePending) || called {
		t.Fatalf("accepted after saved update: called=%t error=%v", called, err)
	}
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

func TestAdminLiveConfigRejectsExternalAPIKeyEdit(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Codex.APIKey = "external-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.AIInstructions = "Review carefully."
		return nil
	}); !errors.Is(err, errAdminRestartRequired) {
		t.Fatalf("external API key edit was not detected: %v", err)
	}
	if got := configForAdminTest(t, path); got.Codex.APIKey != "external-key" || got.AIInstructions != "" {
		t.Fatalf("external edit was overwritten: %+v", got)
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
	live, path := liveConfigFixture(t, true)
	if selected := live.snapshot().RemoteCommands; !selected["review"] || selected["fix"] {
		t.Fatalf("remote selection = %v", selected)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.Repositories = append(cfg.Repositories, config.Repository{Name: "owner/second", ID: 12, OwnerID: 22, Actions: []string{"fix"}})
		return nil
	}); err == nil || !strings.Contains(err.Error(), "editing the configuration file") {
		t.Fatalf("remote worker gained a command without re-selection: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected remote command changed disk: %v", err)
	}
}

func TestAdminLiveConfigAllowsRemotePolicySaveWithUnsupportedAction(t *testing.T) {
	_, path := liveConfigFixture(t, true)
	cfg, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Main = "claude"
		cfg.Repositories[0].Actions = []string{"review", "security-audit"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if live.remoteSelected["security-audit"] || !live.remoteSelected["review"] {
		t.Fatalf("remote selection = %v", live.remoteSelected)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.AIInstructions = "Use the configured review style."
		return nil
	}); err != nil {
		t.Fatalf("unrelated policy save was blocked by unsupported action: %v", err)
	}
	if !live.snapshot().Pending || live.snapshot().Config.AIInstructions != "Use the configured review style." {
		t.Fatal("policy save was not staged for activation")
	}
}

func TestAdminLiveConfigAllowsPolicySaveWithUnsupportedRemoteAudit(t *testing.T) {
	_, path := liveConfigFixture(t, true)
	cfg, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Repositories[0].Actions = []string{"review", "security-audit"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	live.setRemoteSelected(map[string]bool{"review": true}, false)
	if err := live.save(func(cfg *config.Config) error {
		cfg.AIInstructions = "Use the configured review style."
		return nil
	}); err != nil {
		t.Fatalf("unrelated policy save was blocked by the worker host: %v", err)
	}
	if !live.snapshot().Pending || live.snapshot().RemoteCommands["security-audit"] {
		t.Fatal("unsupported audit appeared in selected commands")
	}
}

func TestAdminLiveConfigAllowsRemoteSelectedCommandReenable(t *testing.T) {
	_, path := liveConfigFixture(t, true)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].Actions = []string{"review", "fix"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(next *config.Config) error {
		next.Repositories[0].Actions = []string{"review"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	selected, generation := live.next()
	live.applied(selected, generation, nil)
	if err := live.save(func(next *config.Config) error {
		next.Repositories[0].Actions = []string{"review", "fix"}
		return nil
	}); err != nil {
		t.Fatalf("re-enabling frozen remote command: %v", err)
	}
	if err := live.save(func(next *config.Config) error {
		next.Repositories[0].Actions = []string{"review", "fix", "audit"}
		return nil
	}); err == nil {
		t.Fatal("new remote command bypassed frozen worker selection")
	}
}

func TestAdminLiveConfigRetriesFailedActivation(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	edit := func(cfg *config.Config) error {
		cfg.AIInstructions = "Check the task."
		return nil
	}
	if err := live.save(edit); err != nil {
		t.Fatal(err)
	}
	<-live.updates
	selected, generation := live.next()
	live.applied(live.current(), generation, errors.New("temporary worker failure"))
	if live.snapshot().Pending || live.snapshot().Error == "" {
		t.Fatal("failed activation was not reported")
	}
	called := false
	if err := live.admit(func() error { called = true; return nil }); !errors.Is(err, monitor.ErrConfigUpdatePending) || called {
		t.Fatalf("failed activation admitted old policy: called=%t error=%v", called, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(edit); err != nil {
		t.Fatal(err)
	}
	if !live.snapshot().Pending || live.snapshot().Error != "" {
		t.Fatal("resubmitted configuration did not queue a retry")
	}
	select {
	case <-live.updates:
	default:
		t.Fatal("retry did not wake the monitor")
	}
	current, retryGeneration := live.next()
	if retryGeneration <= generation || current.AIInstructions != selected.AIInstructions {
		t.Fatal("retry did not retain the saved configuration")
	}
	live.applied(current, retryGeneration, nil)
	if err := live.admit(func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("successful retry did not resume admission: called=%t error=%v", called, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("retry rewrote unchanged JSON: %v", err)
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
		mapping.Skills = append(mapping.Skills, skills.BuiltinAntiPromptInjection)
		cfg.Commands["review"] = mapping
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Commands["review"].Skills) != 2 || saved.Commands["review"].Skills[1] != skills.BuiltinAntiPromptInjection {
		t.Fatalf("editing a default command dropped its skills: %+v", saved.Commands["review"])
	}
}

func TestAdminLiveConfigKeepsInvitedRepositoryDeferred(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	if _, err := config.AddRepository(path, config.Repository{Name: "owner/invited", ID: 12, OwnerID: 22, Actions: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.AIInstructions = "Check the task."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(live.snapshot().Config.Repositories) != 1 {
		t.Fatal("unrelated admin save activated an invited repository")
	}
	cfg, generation := live.next()
	live.applied(cfg, generation, nil)
	if len(live.current().Repositories) != 1 {
		t.Fatal("invited repository reached the running monitor before restart")
	}
	saved, err := config.Load(path)
	if err != nil || len(saved.Repositories) != 2 {
		t.Fatalf("invited repository was lost from disk: %+v, %v", saved.Repositories, err)
	}
	restarted, err := newAdminLiveConfig(path, saved)
	if err != nil || len(restarted.current().Repositories) != 2 {
		t.Fatalf("restart did not activate invited repository: %+v, %v", restarted, err)
	}
}

func TestAdminLiveConfigRejectsLossOfLastRunnableCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{BotLogin: "bot", Commands: map[string]config.CommandMapping{
		"review": {Action: "review", Skills: []string{skills.BuiltinAntiPromptInjection}},
		"audit":  {Action: "comment", Skills: []string{"https://github.com/owner/skills/tree/main/skills/not-installed"}},
	}, AlwaysOnSkills: []string{}, Repositories: []config.Repository{
		{Name: "owner/ready", ID: 11, OwnerID: 22, Actions: []string{"review"}},
		{Name: "owner/missing", ID: 12, OwnerID: 22, Actions: []string{"audit"}},
	}}
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
	if !hasRunnableLocalCommand(loaded) {
		t.Fatal("fixture has no runnable command")
	}
	live, err := newAdminLiveConfig(path, loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*config.Config) error
	}{
		{"switch to missing command", func(cfg *config.Config) error {
			cfg.Repositories[0].Actions = []string{"audit"}
			return nil
		}},
		{"remove ready repository", func(cfg *config.Config) error {
			cfg.Repositories = cfg.Repositories[1:]
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := live.save(test.edit); err == nil || !strings.Contains(err.Error(), "at least one enabled command") {
				t.Fatalf("invalid local selection was saved: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) {
				t.Fatalf("rejected edit changed disk: %v", err)
			}
		})
	}
}

func policyRoundsForTest(value int) *int { return &value }
