//go:build linux

package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestCreateInitialConfigPromptsAndSavesHash(t *testing.T) {
	master, terminal, _ := newHashPTY(t)
	oldInput, oldError := os.Stdin, os.Stderr
	os.Stdin, os.Stderr = terminal, terminal
	t.Cleanup(func() { os.Stdin, os.Stderr = oldInput, oldError })
	path := filepath.Join(t.TempDir(), "config.local.json")
	var output bytes.Buffer
	type result struct {
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		created, err := createInitialConfig(path, log.New(&output, "", 0))
		done <- result{created, err}
	}()
	prompt := make([]byte, len("Set admin password: "))
	promptRead := make(chan error, 1)
	go func() { _, err := io.ReadFull(master, prompt); promptRead <- err }()
	select {
	case err := <-promptRead:
		if err != nil || string(prompt) != "Set admin password: " {
			t.Fatalf("initial password prompt=%q error=%v", prompt, err)
		}
	case <-time.After(5 * time.Second):
		_ = master.Close()
		t.Fatal("initial password prompt timed out")
	}
	if _, err := master.Write([]byte("new-admin-password\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || !got.created {
			t.Fatalf("create initial config: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial password prompt timed out")
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	terminalOutput, _ := io.ReadAll(master)
	if strings.Contains(string(terminalOutput), "new-admin-password") {
		t.Fatal("initial password was echoed")
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Admin == nil || len(cfg.Admin.Administrators) != 1 ||
		!cfg.Admin.Administrators[0].VerifyPassword("new-admin-password") {
		t.Fatalf("initial admin password was not saved: error=%v", err)
	}
}
