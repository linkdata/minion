//go:build darwin

package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpdatePreservesMacOSACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("chmod", "+a", "everyone allow read", path).CombinedOutput(); err != nil {
		t.Fatalf("set fixture ACL: %v: %s", err, output)
	}
	original, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before, hasACL, err := macOSACLBytes(original)
	_ = original.Close()
	if err != nil || !hasACL {
		t.Fatalf("fixture ACL unavailable: %v, %t", err, hasACL)
	}
	if _, err := AddRepository(path, Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
		t.Fatalf("save config with ACL: %v", err)
	}
	updated, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer updated.Close()
	after, stillHasACL, err := macOSACLBytes(updated)
	if err != nil || !stillHasACL || !bytes.Equal(after, before) {
		t.Fatalf("config ACL changed: before=%v after=%v present=%t error=%v", before, after, stillHasACL, err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Repositories) != 2 || cfg.Repositories[1].Name != "owner/second" {
		t.Fatalf("repository change was not saved: %+v, %v", cfg, err)
	}
}

func TestUpdateClearsInheritedStagingDirectoryACL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("chmod", "+a", "everyone allow read,search,directory_inherit", dir).CombinedOutput(); err != nil {
		t.Fatalf("set inheritable fixture ACL: %v: %s", err, output)
	}
	probe, err := os.MkdirTemp(dir, "probe-*")
	if err != nil {
		t.Fatal(err)
	}
	probeDirectory, err := os.Open(probe)
	if err != nil {
		t.Fatal(err)
	}
	_, inherited, aclErr := macOSACLBytes(probeDirectory)
	closeErr := probeDirectory.Close()
	removeErr := os.Remove(probe)
	if aclErr != nil || closeErr != nil || removeErr != nil || !inherited {
		t.Fatalf("fixture did not inherit ACL: inherited=%t, acl=%v, close=%v, remove=%v", inherited, aclErr, closeErr, removeErr)
	}
	if _, err := AddRepository(path, Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
		t.Fatalf("save config under inheritable ACL: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("temporary config remained: %v, %v", entries, err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Repositories) != 2 || cfg.Repositories[1].Name != "owner/second" {
		t.Fatalf("repository change was not saved: %+v, %v", cfg, err)
	}
	updated, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer updated.Close()
	_, hasACL, err := macOSACLBytes(updated)
	if err != nil || hasACL {
		t.Fatalf("saved config inherited staging ACL: present=%t error=%v", hasACL, err)
	}
}
