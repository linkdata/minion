//go:build linux

package testsupport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWaitExitedParsesTheLastCommDelimiter(t *testing.T) {
	for _, test := range []struct {
		name, stat string
		exited     bool
		invalid    bool
	}{
		{name: "zombie with closing parenthesis in comm", stat: "7 (odd) name) Z 1 2", exited: true},
		{name: "comm cannot fake zombie state", stat: "7 (odd) Z label) S 1 2"},
		{name: "stopped is not exited", stat: "7 (worker) T 1 2"},
		{name: "missing delimiter", stat: "malformed", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stat")
			if err := os.WriteFile(path, []byte(test.stat), 0600); err != nil {
				t.Fatal(err)
			}
			pinned, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := pinned.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err = WaitExited(ctx, pinned)
			switch {
			case test.exited:
				if err != nil {
					t.Fatal(err)
				}
			case test.invalid:
				if err == nil || !strings.Contains(err.Error(), "invalid pinned process state") {
					t.Fatalf("malformed state=%v", err)
				}
			default:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("live process reported exited: %v", err)
				}
			}
		})
	}
}

func TestPinProcessRequiresAValidPIDFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if _, err := PinProcess(path); !errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("missing PID marker was mistaken for an exited child: %v", err)
	}
	for _, data := range []string{"", "invalid", "0", "-1"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if child, err := PinProcess(path); child != nil || err == nil || errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("invalid PID %q was accepted or mistaken for exit: %v, %v", data, child, err)
		}
	}
}
