package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func remoteTestConfig() Config {
	return Config{
		BotLogin: "bot", Main: "codex", Codex: CodexConfig{Model: "test-model", ReasoningEffort: "medium"},
		Commands:     map[string]CommandMapping{"review": {Action: "review", Skills: []string{"https://github.com/o/r/tree/main/skills/uninstalled"}}},
		Repositories: []Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"review"}}},
		RemoteExecutor: &RemoteExecutorConfig{ExecutorID: "remote-1", Listen: "127.0.0.1:8443", TLSCertFile: "cert.pem", TLSKeyFile: "key.pem",
			Token: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))},
	}
}

func TestLoadRemoteExecutorConfigResolvesPathsWithoutLocalSkills(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(remoteTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteExecutor == nil || cfg.RemoteExecutor.TLSCertFile != filepath.Join(dir, "cert.pem") ||
		cfg.RemoteExecutor.TLSKeyFile != filepath.Join(dir, "key.pem") || cfg.RemoteExecutor.ExecutorID != "remote-1" {
		t.Fatalf("remote paths or identity were not loaded: %+v", cfg.RemoteExecutor)
	}
}

func TestLoadRemoteExecutorKeepsLexicalTLSPathResolution(t *testing.T) {
	t.Parallel()
	if filepath.Separator == '\\' {
		t.Skip("symlink traversal semantics differ on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		filepath.Join(dir, "cert.pem"):    "cleaned path",
		filepath.Join(target, "cert.pem"): "symlink path",
	} {
		if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(target, "sub"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := remoteTestConfig()
	cfg.RemoteExecutor.TLSCertFile = "link/../cert.pem"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "cert.pem")
	if cfg.RemoteExecutor == nil || cfg.RemoteExecutor.TLSCertFile != want {
		t.Fatalf("remote certificate path = %+v, want %q", cfg.RemoteExecutor, want)
	}
	contents, err := os.ReadFile(cfg.RemoteExecutor.TLSCertFile)
	if err != nil || string(contents) != "cleaned path" {
		t.Fatalf("remote certificate = %q, %v; want cleaned path", contents, err)
	}
}

func TestRemoteExecutorConfigRejectsInvalidTransportOrMissingSelectedModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		change     func(*Config)
	}{
		{"identity", "executor_id", func(c *Config) { c.RemoteExecutor.ExecutorID = "../other" }},
		{"address", "listen", func(c *Config) { c.RemoteExecutor.Listen = "not-an-address" }},
		{"zero port", "listen", func(c *Config) { c.RemoteExecutor.Listen = "127.0.0.1:0" }},
		{"certificate", "tls_cert_file", func(c *Config) { c.RemoteExecutor.TLSCertFile = "" }},
		{"key", "tls_key_file", func(c *Config) { c.RemoteExecutor.TLSKeyFile = "" }},
		{"token", "token", func(c *Config) { c.RemoteExecutor.Token = "short" }},
		{"codex model", "explicit model and effort", func(c *Config) { c.Codex.Model = "" }},
		{"codex effort", "explicit model and effort", func(c *Config) { c.Codex.ReasoningEffort = "" }},
		{"critic model", "explicit model and effort", func(c *Config) { c.Adversarial = "claude" }},
		{"critic effort", "explicit model and effort", func(c *Config) { c.Adversarial = "claude"; c.Claude.Model = "test-model" }},
		{"claude default alias", "concrete Claude model", func(c *Config) { c.Adversarial = "claude"; c.Claude.Model, c.Claude.Effort = "default", "medium" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := remoteTestConfig()
			tc.change(&cfg)
			if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLocalConfigDoesNotRequireExplicitModels(t *testing.T) {
	t.Parallel()
	cfg := remoteTestConfig()
	cfg.RemoteExecutor = nil
	cfg.Codex = CodexConfig{}
	if err := cfg.Validate(t.TempDir()); err != nil {
		t.Fatalf("local config changed: %v", err)
	}
}
