package main

import (
	"errors"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

func TestServiceWiringPreservesAdmissionAndCredentials(t *testing.T) {
	strict := 0
	consecutive := 7
	cfg := config.Config{
		WorkRoot: "/private/work", StatePath: "/private/state.json", BotLogin: "bot",
		Main: "claude", Adversarial: "codex",
		CriticStopping:           config.CriticStopping{MaxRounds: 5, NonblockingAfterRound: &strict, ConsecutiveNoBlockerRounds: &consecutive},
		InactivityTimeoutMinutes: 3, MaxRuntimeMinutes: 7, MaxGitHubContextBytes: 1234,
		AlwaysOnSkills: []string{skills.BuiltinAntiPromptInjection},
		Commands:       map[string]config.CommandMapping{"inspect": {Action: "review"}},
		Codex:          config.CodexConfig{APIKey: "codex-test-key", Model: "codex-pinned", ReasoningEffort: "high", Skills: []string{"stale-host-skill"}},
		Claude:         config.ClaudeConfig{APIKey: "claude-test-key", Model: "claude-pinned", Effort: "medium", Skills: []string{"stale-host-skill"}},
	}
	admissionErr := errors.New("admission sentinel")
	client, store, logger := &github.Client{}, &state.Store{}, log.New(io.Discard, "", 0)
	orchestrator := newOrchestrator(cfg, client, store, logger, func() error { return admissionErr })
	if orchestrator.GitHub != client || orchestrator.Store != store || orchestrator.Logger != logger || orchestrator.Runner.Logger != logger {
		t.Fatal("service lost client, state or logger")
	}
	if err := orchestrator.Execute(t.Context(), monitor.Job{}); !errors.Is(err, admissionErr) {
		t.Fatalf("wired admission did not stop execution: %v", err)
	}
	if orchestrator.WorkRoot != cfg.WorkRoot || !errors.Is(orchestrator.CheckWorkRoot(), admissionErr) ||
		orchestrator.CriticStopping.NonblockingAfterRound == nil || *orchestrator.CriticStopping.NonblockingAfterRound != 0 {
		t.Fatal("service lost controlled admission or strict review policy")
	}
	if orchestrator.Main != cfg.Main || orchestrator.Adversarial != cfg.Adversarial || !reflect.DeepEqual(orchestrator.CriticStopping, cfg.CriticStopping) ||
		orchestrator.CodexModel != cfg.Codex.Model || orchestrator.CodexEffort != cfg.Codex.ReasoningEffort ||
		orchestrator.ClaudeModel != cfg.Claude.Model || orchestrator.ClaudeEffort != cfg.Claude.Effort {
		t.Fatal("service lost pinned provider roles, models, efforts or critic budget")
	}
	if orchestrator.MaxRuntime != 7*time.Minute || orchestrator.MaxGitHubContextBytes != cfg.MaxGitHubContextBytes ||
		!slices.Equal(orchestrator.AlwaysOnSkills, cfg.AlwaysOnSkills) ||
		!reflect.DeepEqual(orchestrator.Commands, cfg.Commands) || orchestrator.BotLogin != cfg.BotLogin ||
		orchestrator.Usage == nil || orchestrator.Usage.Dir != filepath.Join(filepath.Dir(cfg.StatePath), "usage") {
		t.Fatal("service lost configured execution, command or usage settings")
	}
	if orchestrator.Runner.Codex.APIKey != cfg.Codex.APIKey ||
		orchestrator.Runner.Claude.APIKey != cfg.Claude.APIKey ||
		len(orchestrator.Runner.Codex.Skills) != 0 ||
		len(orchestrator.Runner.Claude.Skills) != 0 {
		t.Fatal("runner lost provider authentication or retained unselected skills")
	}
}

func TestServiceWiringSnapshotsNativeCodexCredentials(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "native-key")
	t.Setenv("CODEX_ACCESS_TOKEN", "native-token")
	got := newOrchestrator(config.Config{Main: "claude"}, nil, nil, nil, nil).Runner.Codex
	if got.NativeAPIKey != "" || got.NativeAccessToken != "" || got.APIKey != "" {
		t.Fatalf("unselected ambient Codex credentials were retained: %+v", got)
	}
	got = newOrchestrator(config.Config{Main: "codex"}, nil, nil, nil, nil).Runner.Codex
	if got.NativeAPIKey != "native-key" || got.NativeAccessToken != "native-token" {
		t.Fatal("selected startup recovery lost ambient Codex credentials")
	}
	got = newOrchestrator(config.Config{Main: "claude", Adversarial: "codex"}, nil, nil, nil, nil).Runner.Codex
	if got.NativeAPIKey != "native-key" || got.NativeAccessToken != "native-token" {
		t.Fatal("selected Codex critic lost ambient credentials")
	}
	got = newOrchestrator(config.Config{Main: "codex", Codex: config.CodexConfig{NativeAPIKey: "validated-key", NativeAccessToken: "validated-token"}}, nil, nil, nil, nil).Runner.Codex
	if got.NativeAPIKey != "validated-key" || got.NativeAccessToken != "validated-token" {
		t.Fatalf("validated Codex credentials changed before dispatch: %+v", got)
	}
	got = newOrchestrator(config.Config{Codex: config.CodexConfig{APIKey: "configured-key"}}, nil, nil, nil, nil).Runner.Codex
	if got.APIKey != "configured-key" || got.NativeAPIKey != "" || got.NativeAccessToken != "" {
		t.Fatalf("configured Codex key did not exclude native credentials: %+v", got)
	}
}
