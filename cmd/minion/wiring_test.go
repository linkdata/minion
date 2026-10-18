package main

import (
	"errors"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestServiceWiringPreservesAdmissionAndHostPolicy(t *testing.T) {
	strict := 0
	sockets := true
	cfg := config.Config{
		WorkRoot: "/private/work", StatePath: "/private/state.json", BotLogin: "bot",
		Main: "claude", Adversarial: "codex", CriticNonblockingRounds: &strict,
		Codex:  config.CodexConfig{APIKey: "codex-test-key", Skills: []string{"stale-host-skill"}},
		Claude: config.ClaudeConfig{APIKey: "claude-test-key", AllowAllUnixSockets: &sockets, Skills: []string{"stale-host-skill"}},
	}
	admissionErr := errors.New("admission sentinel")
	orchestrator := newOrchestrator(cfg, nil, nil, nil, func() error { return admissionErr })
	if orchestrator.WorkRoot != cfg.WorkRoot || !errors.Is(orchestrator.CheckWorkRoot(), admissionErr) ||
		orchestrator.CriticNonblockingRounds == nil || *orchestrator.CriticNonblockingRounds != 0 {
		t.Fatal("service lost controlled admission or strict review policy")
	}
	if orchestrator.Runner.Codex.APIKey != cfg.Codex.APIKey ||
		orchestrator.Runner.Claude.APIKey != cfg.Claude.APIKey ||
		orchestrator.Runner.Claude.AllowAllUnixSockets != cfg.Claude.AllowAllUnixSockets ||
		len(orchestrator.Runner.Codex.Skills) != 0 ||
		len(orchestrator.Runner.Claude.Skills) != 0 {
		t.Fatal("runner lost provider authentication/socket policy or retained unselected skills")
	}
}
