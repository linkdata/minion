//go:build windows

package main

import (
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestWindowsSetupSkipsSecurityAudit(t *testing.T) {
	cfg := config.Config{
		Commands: map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
		},
	}
	if actions, err := setupRepositoryActions(cfg); err == nil || len(actions) != 0 {
		t.Fatalf("audit-only setup actions=%v error=%v", actions, err)
	}
	cfg.Commands["review"] = config.CommandMapping{Action: "review", Skills: []string{"builtin://anti-prompt-injection"}}
	if actions, err := setupRepositoryActions(cfg); err != nil || len(actions) != 1 || actions[0] != "review" {
		t.Fatalf("review setup actions=%v error=%v", actions, err)
	}
}

func TestWindowsAdminDoesNotOfferRemoteAudit(t *testing.T) {
	cfg := config.Config{Main: "codex", RemoteExecutor: &config.RemoteExecutorConfig{}, Commands: map[string]config.CommandMapping{
		"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
	}}
	if err := validateAdminActions(cfg, []string{"audit"}, nil); err == nil {
		t.Fatal("admin form accepted a remote audit without a publishing validator")
	}
	view := configurationView(adminConfigSnapshot{Config: cfg, Remote: true, RemoteCommands: map[string]bool{"audit": true}})
	if len(view.Commands) != 0 {
		t.Fatalf("admin form offered audit on Windows publishing host: %v", view.Commands)
	}
}
