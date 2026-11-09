//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/wintest"
	"golang.org/x/sys/windows"
)

func TestAddRepositoryThroughJunctionWindows(t *testing.T) {
	physical := t.TempDir()
	config := filepath.Join(physical, "config.json")
	if err := os.WriteFile(config, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/first","id":1,"owner_id":2,"actions":["review"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "config-home")
	if err := wintest.CreateJunction(alias, physical); err != nil {
		t.Fatal(err)
	}
	if _, err := AddRepository(filepath.Join(alias, "config.json"), Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config)
	if err != nil || !strings.Contains(string(data), `"name": "owner/second"`) {
		t.Fatalf("config target was not updated: %s, %v", data, err)
	}
}

func TestUpdatePreservesWindowsConfigACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := before.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("config DACL was not protected: control=%#x error=%v", control, err)
	}
	if _, err := Update(path, func(cfg *Config) error { cfg.BotLogin = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() == "" || before.String() != after.String() {
		t.Fatal("config DACL changed after save")
	}
}
