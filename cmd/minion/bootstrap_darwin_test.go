//go:build darwin

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateInitialConfigClearsInheritedACL(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("/bin/chmod", "+a", "everyone allow read,search,file_inherit", dir).CombinedOutput(); err != nil {
		t.Fatalf("set inheritable fixture ACL: %v: %s", err, output)
	}
	probe, err := os.CreateTemp(dir, "probe-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	probeACL, err := exec.Command("/bin/ls", "-le", probe.Name()).CombinedOutput()
	if err != nil || !strings.Contains(string(probeACL), "everyone") {
		t.Fatalf("fixture did not inherit ACL: %s, %v", probeACL, err)
	}
	if err := os.Remove(probe.Name()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.local.json")
	if created, err := createInitialConfigWithPrompt(path, log.New(io.Discard, "", 0), false, nil); err != nil || !created {
		t.Fatalf("create initial config: created=%t error=%v", created, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("initial config mode: %v, %v", info, err)
	}
	configACL, err := exec.Command("/bin/ls", "-le", path).CombinedOutput()
	if err != nil || strings.Contains(string(configACL), "everyone") {
		t.Fatalf("initial config inherited ACL: %s, %v", configACL, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("temporary config remained: %v, %v", entries, err)
	}
}
