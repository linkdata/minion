//go:build windows

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRepositoryThroughJunctionWindows(t *testing.T) {
	physical := t.TempDir()
	config := filepath.Join(physical, "config.json")
	if err := os.WriteFile(config, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/first","id":1,"owner_id":2,"actions":["review"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "config-home")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", alias, physical).CombinedOutput(); err != nil {
		t.Fatalf("create config junction: %v: %s", err, output)
	}
	if _, err := AddRepository(filepath.Join(alias, "config.json"), Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config)
	if err != nil || !strings.Contains(string(data), `"name": "owner/second"`) {
		t.Fatalf("config target was not updated: %s, %v", data, err)
	}
}
