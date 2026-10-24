//go:build windows

package aicli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAbsoluteExecutableRequiresEXEWindows(t *testing.T) {
	for _, extension := range []string{".cmd", ".bat", ".ps1", ".com"} {
		t.Run(extension, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			t.Setenv("PATHEXT", ".PS1;.COM;.EXE;.BAT;.CMD")
			if err := os.WriteFile(filepath.Join(bin, "fixture"+extension), []byte("@echo off\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := AbsoluteExecutable("fixture"); got != "" || err == nil || !strings.Contains(err.Error(), "native .exe") {
				t.Fatalf("non-native launcher accepted: path=%q error=%v", got, err)
			}
		})
	}
}
