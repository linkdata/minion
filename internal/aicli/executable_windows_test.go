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

func TestAbsoluteExecutableSkipsEarlierCommandShimWindows(t *testing.T) {
	shimDir, exeDir := t.TempDir(), t.TempDir()
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+exeDir)
	t.Setenv("PATHEXT", ".CMD;.EXE")
	if err := os.WriteFile(filepath.Join(shimDir, "fixture.cmd"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(exeDir, "fixture.exe")
	if err := os.WriteFile(want, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := AbsoluteExecutable("fixture"); err != nil || got != want {
		t.Fatalf("native executable=%q want=%q error=%v", got, want, err)
	}
}

func TestAbsoluteExecutableRejectsCommandSymlinkWindows(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("PATHEXT", ".EXE;.CMD")
	target := filepath.Join(bin, "shim.cmd")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bin, "fixture.exe")); err != nil {
		t.Skipf("Windows file symlink fixture unavailable: %v", err)
	}
	if got, err := AbsoluteExecutable("fixture"); got != "" || err == nil || !strings.Contains(err.Error(), "native .exe") {
		t.Fatalf("command symlink accepted: path=%q error=%v", got, err)
	}
	if got, err := ExecutablePath("fixture"); got != "" || err == nil || !strings.Contains(err.Error(), "native .exe") {
		t.Fatalf("resolved command symlink accepted: path=%q error=%v", got, err)
	}
}
