package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
)

func TestServiceWiringPreservesAdmissionAndHostPolicy(t *testing.T) {
	strict := 0
	sockets := true
	cfg := config.Config{
		WorkRoot: "/private/work", StatePath: "/private/state.json", BotLogin: "bot",
		Main: "claude", Adversarial: "codex", CriticNonblockingRounds: &strict,
		CriticMaxRounds:   5,
		JobTimeoutMinutes: 7, MaxGitHubContextBytes: 1234,
		AlwaysOnSkills: []string{skills.BuiltinAntiPromptInjection},
		Commands:       map[string]config.CommandMapping{"inspect": {Action: "review"}},
		Codex:          config.CodexConfig{APIKey: "codex-test-key", Model: "codex-pinned", ReasoningEffort: "high", Skills: []string{"stale-host-skill"}},
		Claude:         config.ClaudeConfig{APIKey: "claude-test-key", Model: "claude-pinned", Effort: "medium", AllowAllUnixSockets: &sockets, Skills: []string{"stale-host-skill"}},
	}
	admissionErr := errors.New("admission sentinel")
	orchestrator := newOrchestrator(cfg, nil, nil, nil, func() error { return admissionErr })
	if orchestrator.WorkRoot != cfg.WorkRoot || !errors.Is(orchestrator.CheckWorkRoot(), admissionErr) ||
		orchestrator.CriticNonblockingRounds == nil || *orchestrator.CriticNonblockingRounds != 0 {
		t.Fatal("service lost controlled admission or strict review policy")
	}
	if orchestrator.Main != cfg.Main || orchestrator.Adversarial != cfg.Adversarial || orchestrator.CriticMaxRounds != cfg.CriticMaxRounds ||
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
