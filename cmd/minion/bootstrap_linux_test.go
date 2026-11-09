//go:build linux

package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestCreateInitialConfigPromptsWithRedirectedStderr(t *testing.T) {
	master, terminal, _ := newHashPTY(t)
	input, err := os.Open(terminal.Name())
	if err != nil {
		t.Fatal(err)
	}
	errorLog, err := os.CreateTemp(t.TempDir(), "minion-stderr")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	defer func() {
		_ = master.Close()
		_ = input.Close()
		_ = terminal.Close()
		_ = errorLog.Close()
		workers.Wait()
	}()
	oldInput, oldError := os.Stdin, os.Stderr
	os.Stdin, os.Stderr = input, errorLog
	t.Cleanup(func() { os.Stdin, os.Stderr = oldInput, oldError })
	path := filepath.Join(t.TempDir(), "config.local.json")
	var output bytes.Buffer
	type result struct {
		created bool
		err     error
	}
	done := make(chan result, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		created, err := createInitialConfig(path, log.New(&output, "", 0))
		done <- result{created, err}
	}()
	prompt := make([]byte, len("Set admin password: "))
	promptRead := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); _, err := io.ReadFull(master, prompt); promptRead <- err }()
	select {
	case err := <-promptRead:
		if err != nil || string(prompt) != "Set admin password: " {
			t.Fatalf("initial password prompt=%q error=%v", prompt, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial password prompt timed out")
	}
	if _, err := master.Write([]byte("\rnew-admin-password\r")); err != nil {
		t.Fatal(err)
	}
	type retryResult struct {
		output string
		err    error
	}
	retryRead := make(chan retryResult, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		var output strings.Builder
		for output.Len() < 256 && !strings.HasSuffix(output.String(), "Set admin password: ") {
			var next [1]byte
			if _, err := master.Read(next[:]); err != nil {
				retryRead <- retryResult{output.String(), err}
				return
			}
			output.WriteByte(next[0])
		}
		retryRead <- retryResult{output: output.String()}
	}()
	select {
	case got := <-retryRead:
		if got.err != nil || !strings.Contains(got.output, errInvalidAdminPassword.Error()) || !strings.HasSuffix(got.output, "Set admin password: ") {
			t.Fatalf("blank password retry = %q, %v", got.output, got.err)
		}
	case got := <-done:
		t.Fatalf("blank password ended setup: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("blank password retry timed out")
	}
	select {
	case got := <-done:
		if got.err != nil || !got.created {
			t.Fatalf("create initial config: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial password prompt timed out")
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	terminalOutput, _ := io.ReadAll(master)
	if strings.Contains(string(terminalOutput), "new-admin-password") {
		t.Fatal("initial password was echoed")
	}
	if stderr, err := os.ReadFile(errorLog.Name()); err != nil || len(stderr) != 0 {
		t.Fatalf("password prompt went to redirected stderr: %q, %v", stderr, err)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Admin == nil || len(cfg.Admin.Administrators) != 1 ||
		!cfg.Admin.Administrators[0].VerifyPassword("new-admin-password") {
		t.Fatalf("initial admin password was not saved: error=%v", err)
	}
}

func TestCreateInitialConfigWithoutPromptFromTerminal(t *testing.T) {
	master, terminal, _ := newHashPTY(t)
	var worker sync.WaitGroup
	defer func() {
		_ = master.Close()
		_ = terminal.Close()
		worker.Wait()
	}()
	oldInput := os.Stdin
	os.Stdin = terminal
	defer func() { os.Stdin = oldInput }()
	path := filepath.Join(t.TempDir(), "config.local.json")
	done := make(chan error, 1)
	worker.Add(1)
	go func() {
		defer worker.Done()
		_, err := createInitialConfigWithoutPrompt(path, log.New(io.Discard, "", 0))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("install-only config creation waited for a password")
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	terminalOutput, _ := io.ReadAll(master)
	if len(terminalOutput) != 0 {
		t.Fatalf("install-only config creation prompted: %q", terminalOutput)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Admin == nil || len(cfg.Admin.Administrators) != 1 ||
		cfg.Admin.Administrators[0].PasswordHash != config.AdminPasswordHashPlaceholder {
		t.Fatalf("install-only config did not retain inert password placeholder: error=%v", err)
	}
}
