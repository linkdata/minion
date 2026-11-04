//go:build linux

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"golang.org/x/sys/unix"
)

func TestHashModeInteractiveTerminal(t *testing.T) {
	for _, test := range []struct {
		name, input, password, errorText string
		wantCode                         int
		failStdout                       bool
	}{
		{name: "Unicode Backspace", input: "é\x7fa\r", password: "a"},
		{name: "replacement character", input: "a�b\r", password: "a�b"},
		{name: "corrected overflow", input: strings.Repeat("a", 65) + "\x7f\r", password: strings.Repeat("a", 64)},
		{name: "too many characters", input: strings.Repeat("a", 65) + "\r", wantCode: 2, errorText: "1 to 64 Unicode code points"},
		{name: "canonical boundary", input: strings.Repeat("a", 4097) + "\r", wantCode: 2, errorText: "1 to 64 Unicode code points"},
		{name: "malformed UTF-8", input: "\xff\r", wantCode: 2, errorText: "1 to 64 Unicode code points"},
		{name: "Ctrl-C", input: "\x03", wantCode: 1, errorText: "read admin password"},
		{name: "stdout failure", input: "password\r", wantCode: 1, errorText: "write admin password hash", failStdout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			master, slave, before := newHashPTY(t)
			var err error
			stdout, err := os.CreateTemp(t.TempDir(), "hash-output")
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			if test.failStdout {
				stdout, err = os.OpenFile("/dev/full", os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer stdout.Close()
			}
			oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
			os.Stdin, os.Stdout, os.Stderr = slave, stdout, slave
			defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()
			done := make(chan int, 1)
			go func() { done <- runHashMode() }()
			readHashPrompt(t, master)
			if _, err := master.Write([]byte(test.input)); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				if code != test.wantCode {
					t.Fatalf("hash exit=%d, want %d", code, test.wantCode)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("hash command timed out")
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || after.Lflag != before.Lflag {
				t.Fatalf("terminal state not restored: before=%#x after=%#x error=%v", before.Lflag, after.Lflag, err)
			}
			if err := slave.Close(); err != nil {
				t.Fatal(err)
			}
			terminalOutput, _ := io.ReadAll(master)
			if test.password != "" {
				if strings.TrimSpace(string(terminalOutput)) != "" {
					t.Fatalf("password appeared on terminal: %q", terminalOutput)
				}
				hash, err := os.ReadFile(stdout.Name())
				if err != nil || !strings.HasSuffix(string(hash), "\n") || strings.Count(string(hash), "\n") != 1 ||
					!(config.AdminAccount{PasswordHash: strings.TrimSpace(string(hash))}).VerifyPassword(test.password) {
					t.Fatalf("stdout is not the expected password hash: output=%q error=%v", hash, err)
				}
			} else if !strings.Contains(string(terminalOutput), test.errorText) {
				t.Fatalf("terminal output=%q, want %q", terminalOutput, test.errorText)
			}
		})
	}
}

func newHashPTY(t *testing.T) (master, slave *os.File, before *unix.Termios) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	before, err = unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave, before
}

func readHashPrompt(t *testing.T, master *os.File) {
	t.Helper()
	prompt := make([]byte, len("Admin password: "))
	promptRead := make(chan error, 1)
	go func() { _, err := io.ReadFull(master, prompt); promptRead <- err }()
	select {
	case err := <-promptRead:
		if err != nil || string(prompt) != "Admin password: " {
			t.Fatalf("prompt=%q error=%v", prompt, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hash prompt timed out")
	}
}

func TestHashModeRejectsRedirectedStderr(t *testing.T) {
	_, slave, before := newHashPTY(t)
	errorLog, err := os.CreateTemp(t.TempDir(), "hash-errors")
	if err != nil {
		t.Fatal(err)
	}
	defer errorLog.Close()
	oldIn, oldErr := os.Stdin, os.Stderr
	os.Stdin, os.Stderr = slave, errorLog
	defer func() { os.Stdin, os.Stderr = oldIn, oldErr }()
	if code := runHashMode(); code != 2 {
		t.Fatalf("redirected stderr returned %d, want 2", code)
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil || after.Lflag != before.Lflag {
		t.Fatalf("terminal changed with redirected stderr: error=%v", err)
	}
	output, err := os.ReadFile(errorLog.Name())
	if err != nil || !strings.Contains(string(output), "terminal on stderr") {
		t.Fatalf("redirected stderr output=%q error=%v", output, err)
	}
}

func TestHashModeRestoresTerminalOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			master, slave, before := newHashPTY(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHashModeSignalHelper$")
			child.Env = append(os.Environ(), "MINION_HASH_SIGNAL_HELPER=1")
			child.Stdin, child.Stderr, child.Stdout = slave, slave, io.Discard
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			readHashPrompt(t, master)
			if err := child.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil || ctx.Err() != nil {
				t.Fatalf("signal exit=%v context=%v", err, ctx.Err())
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || after.Lflag != before.Lflag {
				t.Fatalf("terminal state not restored after %v: before=%#x after=%#x error=%v", sig, before.Lflag, after.Lflag, err)
			}
		})
	}
}

func TestHashModeSignalHelper(t *testing.T) {
	if os.Getenv("MINION_HASH_SIGNAL_HELPER") == "1" {
		os.Exit(runHashMode())
	}
}
