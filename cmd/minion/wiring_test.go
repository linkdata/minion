package main

import (
	"errors"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestServiceWiringPreservesConfiguredRolesAndHostPolicy(t *testing.T) {
	strict := 0
	sockets := true
	cfg := config.Config{
		WorkRoot: "/private/work", StatePath: "/private/state.json", BotLogin: "bot",
		Main: "claude", Adversarial: "codex", CriticMaxRounds: 7, CriticNonblockingRounds: &strict,
		JobTimeoutMinutes: 137, MaxGitHubContextBytes: 12345,
		AlwaysOnSkills: []string{"builtin://anti-prompt-injection"},
		Commands:       map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{"builtin://anti-prompt-injection"}}},
		Codex:          config.CodexConfig{APIKey: "codex-test-key", Model: "codex-model", ReasoningEffort: "codex-effort", Skills: []string{"stale-host-skill"}},
		Claude:         config.ClaudeConfig{APIKey: "claude-test-key", Model: "claude-model", Effort: "claude-effort", AllowAllUnixSockets: &sockets, Skills: []string{"stale-host-skill"}},
	}
	client, store, logger := github.New("github-test-token"), &state.Store{}, log.New(io.Discard, "", 0)
	admissionErr := errors.New("admission sentinel")
	orchestrator := newOrchestrator(cfg, client, store, logger, func() error { return admissionErr })
	if orchestrator.WorkRoot != cfg.WorkRoot ||
		orchestrator.Runner.WorkRoot != cfg.WorkRoot ||
		orchestrator.BotLogin != cfg.BotLogin ||
		orchestrator.GitHub != client ||
		orchestrator.Store != store ||
		orchestrator.Logger != logger ||
		orchestrator.Runner.Logger != logger ||
		orchestrator.Usage.Dir != filepath.Join(filepath.Dir(cfg.StatePath), "usage") ||
		!errors.Is(orchestrator.CheckWorkRoot(), admissionErr) {
		t.Fatal("service lost its host roots, stores, credentials or admission policy")
	}
	if orchestrator.Main != cfg.Main ||
		orchestrator.Adversarial != cfg.Adversarial ||
		orchestrator.CriticMaxRounds != cfg.CriticMaxRounds ||
		orchestrator.CriticNonblockingRounds == nil ||
		*orchestrator.CriticNonblockingRounds != 0 ||
		orchestrator.Timeout != time.Duration(cfg.JobTimeoutMinutes)*time.Minute ||
		orchestrator.MaxGitHubContextBytes != cfg.MaxGitHubContextBytes ||
		!reflect.DeepEqual(orchestrator.Commands, cfg.Commands) ||
		!reflect.DeepEqual(orchestrator.AlwaysOnSkills, cfg.AlwaysOnSkills) {
		t.Fatal("service lost configured action or review policy")
	}
	if orchestrator.CodexModel != cfg.Codex.Model ||
		orchestrator.CodexEffort != cfg.Codex.ReasoningEffort ||
		orchestrator.ClaudeModel != cfg.Claude.Model ||
		orchestrator.ClaudeEffort != cfg.Claude.Effort {
		t.Fatal("service lost a model or effort setting")
	}
	if orchestrator.Runner.Codex.APIKey != cfg.Codex.APIKey ||
		orchestrator.Runner.Claude.APIKey != cfg.Claude.APIKey ||
		orchestrator.Runner.Claude.AllowAllUnixSockets != cfg.Claude.AllowAllUnixSockets ||
		len(orchestrator.Runner.Codex.Skills) != 0 ||
		len(orchestrator.Runner.Claude.Skills) != 0 {
		t.Fatal("runner lost provider authentication/socket policy or retained unselected skills")
	}
}
