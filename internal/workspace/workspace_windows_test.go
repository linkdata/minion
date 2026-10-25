package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCanonicalPathResolvesNestedJunctionsWindows(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "releases", "v1")
	bin := filepath.Join(release, "bin")
	for _, dir := range []string{bin, filepath.Join(release, "sibling")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(bin, "codex.exe")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	current, visible := filepath.Join(root, "current"), filepath.Join(root, "visible")
	for _, link := range []struct{ path, target string }{
		{current, release},
		{visible, filepath.Join(current, "bin")},
	} {
		if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link.path, link.target).CombinedOutput(); err != nil {
			t.Fatalf("create junction %q -> %q: %v: %s", link.path, link.target, err, output)
		}
	}
	for _, test := range []struct{ path, want string }{
		{filepath.Join(visible, "codex.exe"), file},
		{visible + `\..\sibling`, filepath.Join(release, "sibling")},
	} {
		want, err := filepath.EvalSymlinks(test.want)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := CanonicalPath(test.path); err != nil || got != want {
			t.Fatalf("CanonicalPath(%q)=%q, %v; want %q", test.path, got, err, want)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(visible); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(release, "sibling"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := CanonicalPath(`..\sibling`); err != nil || got != want {
		t.Fatalf("relative parent through junction=%q, %v; want %q", got, err, want)
	}
	rootRelative := strings.TrimPrefix(visible+`\..\sibling`, filepath.VolumeName(visible))
	if got, err := CanonicalPath(rootRelative); err != nil || got != want {
		t.Fatalf("drive-root parent through junction=%q, %v; want %q", got, err, want)
	}
	if _, err := CanonicalPath(filepath.VolumeName(visible) + `sibling`); err == nil || !strings.Contains(err.Error(), "drive-relative") {
		t.Fatalf("drive-relative path accepted: %v", err)
	}
	if _, err := CanonicalPath(visible + `\..\missing`); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target after junction accepted: %v", err)
	}
	if _, err := CanonicalPath(filepath.Join(visible, "codex.exe") + `\..`); err == nil {
		t.Fatal("file parent component accepted")
	}
}

func TestWindowsPathFromHandlePreservesVolumeGUID(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{`\\?\C:\tools\git.exe`, `C:\tools\git.exe`},
		{`\\?\UNC\server\share\git.exe`, `\\server\share\git.exe`},
		{`\\?\Volume{12345678-1234-1234-1234-123456789abc}\git.exe`, `\\?\Volume{12345678-1234-1234-1234-123456789abc}\git.exe`},
	} {
		if got := windowsPathFromHandle(test.path); got != test.want || !filepath.IsAbs(got) {
			t.Errorf("windowsPathFromHandle(%q) = %q, want absolute %q", test.path, got, test.want)
		}
	}
}

func TestWithinWindowsUsesCanonicalComponentCase(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "Tool")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		root, name string
		want       bool
	}{
		{root, filepath.Join(root, "git.exe"), true},
		{root, filepath.Join(parent, "tool", "git.exe"), false},
		{root, filepath.Join(parent, "Toolbox", "git.exe"), false},
		{filepath.VolumeName(root) + `\`, root, true},
		{`\\Server\Share\Tool`, `\\server\share\Tool\git.exe`, true},
		{`\\?\Volume{12345678-1234-1234-1234-123456789abc}\Tool`, `\\?\Volume{12345678-1234-1234-1234-123456789abc}\Tool\git.exe`, true},
	} {
		if got := Within(test.root, test.name); got != test.want {
			t.Errorf("Within(%q, %q) = %t, want %t", test.root, test.name, got, test.want)
		}
	}
}

func TestWithinWindowsAbsentRootCaseAlias(t *testing.T) {
	parent := t.TempDir()
	grant := filepath.Join(parent, "FutureGrant")
	if !Within(grant, filepath.Join(parent, "FUTUREGRANT", "auth.json")) {
		t.Fatal("case alias escaped absent grant")
	}
	if Within(grant, filepath.Join(parent, "FUTUREGRANT-sibling", "auth.json")) {
		t.Fatal("sibling counted as inside absent grant")
	}
	if Within(grant, filepath.Join(parent, "unrelated")) {
		t.Fatal("unrelated path counted inside absent grant")
	}
}

func TestWindowsSourcePaths(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"ordinary/file.go", true},
		{"NUL", false},
		{"dir/COM1", false},
		{"git~1/config", false},
		{"file.", false},
		{"file ", false},
		{"file:stream", false},
		{`dir\file`, false},
	} {
		if got := validSourcePath(test.name); got != test.valid {
			t.Errorf("validSourcePath(%q) = %t, want %t", test.name, got, test.valid)
		}
	}
	for _, char := range []rune{'<', '>', '"', '|', '?', '*', '\x01', '\x1f'} {
		name := "dir/file" + string(char) + ".go"
		if validSourcePath(name) {
			t.Errorf("accepted Windows-forbidden source path %q", name)
		}
		if err := ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: []File{{Path: name, Mode: Regular}}}); err == nil {
			t.Errorf("accepted Windows-forbidden snapshot path %q", name)
		}
	}
}

func TestWindowsOpenPathRejectsLink(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "source")
	if err := os.WriteFile(name, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openPath(name, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(name, link); err != nil {
		t.Skipf("Windows symlink privilege unavailable: %v", err)
	}
	if file, err := openPath(link, false); err == nil {
		_ = file.Close()
		t.Fatal("opened source through a symlink")
	}
}

func TestWindowsOpenPathRejectsJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "source"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
	for _, test := range []struct {
		name      string
		path      string
		directory bool
	}{
		{"final component", junction, true},
		{"parent component", filepath.Join(junction, "source"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := openPath(test.path, test.directory)
			if file != nil {
				_ = file.Close()
			}
			if err == nil {
				t.Fatalf("opened %q through a junction", test.path)
			}
		})
	}
}

func TestWindowsOpenDirectoryThroughFileReturnsENOTDIR(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(file, "child")} {
		if dir, err := OpenDirectory(path); !errors.Is(err, syscall.ENOTDIR) {
			if dir != nil {
				_ = dir.Close()
			}
			t.Errorf("OpenDirectory(%q) = %v, want ENOTDIR", path, err)
		}
	}
}

func TestWindowsReadRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "source")
	if err := os.WriteFile(name, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(name, filepath.Join(root, "alias")); err != nil {
		t.Skipf("Windows hard links unavailable: %v", err)
	}
	if _, err := ReadRegular(name, MaxBytes); err == nil {
		t.Fatal("read file with multiple hard links")
	}
}

func TestWindowsUsesSelectedGitMode(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "tool")
	if err := os.WriteFile(name, []byte("content"), 0755); err != nil {
		t.Fatal(err)
	}
	file := File{Path: "tool", Mode: Executable, Data: []byte("content")}
	if err := CheckUnchanged(t.Context(), root, []File{file}); err != nil {
		t.Fatal(err)
	}
	changes, err := ReadChanges(t.Context(), root, []Change{{Path: "tool", Mode: Executable}})
	if err != nil || len(changes) != 1 || changes[0].Mode != Executable {
		t.Fatalf("selected executable mode lost: changes=%+v error=%v", changes, err)
	}
}
