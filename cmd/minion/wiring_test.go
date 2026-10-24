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

func TestServiceWiringPreservesAdmissionAndHostPolicy(t *testing.T) {
	strict := 0
	consecutive := 7
	sockets := true
	cfg := config.Config{
		WorkRoot: "/private/work", StatePath: "/private/state.json", BotLogin: "bot",
		Main: "claude", Adversarial: "codex",
		CriticStopping:    config.CriticStopping{MaxRounds: 5, NonblockingAfterRound: &strict, ConsecutiveNoBlockerRounds: &consecutive},
		JobTimeoutMinutes: 7, MaxGitHubContextBytes: 1234,
		AlwaysOnSkills: []string{skills.BuiltinAntiPromptInjection},
		Commands:       map[string]config.CommandMapping{"inspect": {Action: "review"}},
		Codex:          config.CodexConfig{APIKey: "codex-test-key", Model: "codex-pinned", ReasoningEffort: "high", Skills: []string{"stale-host-skill"}},
		Claude:         config.ClaudeConfig{APIKey: "claude-test-key", Model: "claude-pinned", Effort: "medium", AllowAllUnixSockets: &sockets, Skills: []string{"stale-host-skill"}},
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
	if orchestrator.Timeout != 7*time.Minute || orchestrator.MaxGitHubContextBytes != cfg.MaxGitHubContextBytes ||
		!slices.Equal(orchestrator.AlwaysOnSkills, cfg.AlwaysOnSkills) ||
		!reflect.DeepEqual(orchestrator.Commands, cfg.Commands) || orchestrator.BotLogin != cfg.BotLogin ||
		orchestrator.Usage == nil || orchestrator.Usage.Dir != filepath.Join(filepath.Dir(cfg.StatePath), "usage") {
		t.Fatal("service lost configured execution, command or usage settings")
	}
	if orchestrator.Runner.Codex.APIKey != cfg.Codex.APIKey ||
		orchestrator.Runner.Claude.APIKey != cfg.Claude.APIKey ||
		orchestrator.Runner.Claude.AllowAllUnixSockets != cfg.Claude.AllowAllUnixSockets ||
		len(orchestrator.Runner.Codex.Skills) != 0 ||
		len(orchestrator.Runner.Claude.Skills) != 0 {
		t.Fatal("runner lost provider authentication/socket policy or retained unselected skills")
	}
}
