package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/linkdata/minion"
	"github.com/linkdata/minion/internal/config"
)

func TestCreateInitialConfigWithoutTerminal(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close() })
	oldInput := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldInput })

	path := filepath.Join(t.TempDir(), "config.local.json")
	var output bytes.Buffer
	logger := log.New(&output, "", 0)
	created, err := createInitialConfig(path, logger)
	if err != nil || !created {
		t.Fatalf("create initial config: created=%t error=%v", created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != minion.DefaultConfig() {
		t.Fatalf("initial config differs from embedded default: error=%v", err)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("initial config permissions: info=%v error=%v", info, err)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Admin == nil || len(cfg.Admin.Administrators) != 1 ||
		cfg.Admin.Administrators[0].PasswordHash != config.AdminPasswordHashPlaceholder ||
		cfg.BotLogin != "" || len(cfg.Repositories) != 0 {
		t.Fatalf("initial config is not a setup config: config=%+v error=%v", cfg, err)
	}
	created, err = createInitialConfig(path, logger)
	if err != nil || created {
		t.Fatalf("existing config was recreated: created=%t error=%v", created, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != minion.DefaultConfig() {
		t.Fatalf("existing config changed: error=%v", err)
	}
}
