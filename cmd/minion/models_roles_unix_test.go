//go:build !windows

package main

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestCheckModelsResolvesSameProviderRolesIndependently(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"CODEX_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}
	const first = `{"model":"main-1","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}`
	const second = `{"model":"critic-2","defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}`
	script := offlineModeCodexScript(filepath.Join(t.TempDir(), "calls"))
	script = strings.Replace(script, first, first+","+second, 1)
	if !strings.Contains(script, second) {
		t.Fatal("provider fixture did not include the critic model")
	}
	testsupport.FakeOnPath(t, "codex", script)
	cfg := config.Config{Main: "codex", Adversarial: "codex",
		MainSettings:   config.RoleModelConfig{Model: "main-1", Effort: "high"},
		CriticSettings: config.RoleModelConfig{Model: "critic-2", Effort: "medium"}}
	if err := checkModels(t.Context(), &cfg, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	if cfg.MainSettings.Model != "main-1" || cfg.MainSettings.Effort != "high" ||
		cfg.CriticSettings.Model != "critic-2" || cfg.CriticSettings.Effort != "medium" || cfg.Codex.Model != "" {
		t.Fatalf("same-provider role settings mixed: main=%+v critic=%+v defaults=%+v", cfg.MainSettings, cfg.CriticSettings, cfg.Codex)
	}
	if got := newOrchestrator(cfg, nil, nil, nil, nil); got.MainModel != "main-1" || got.CriticModel != "critic-2" {
		t.Fatalf("resolved role settings lost in wiring: main=%q critic=%q", got.MainModel, got.CriticModel)
	}
}
