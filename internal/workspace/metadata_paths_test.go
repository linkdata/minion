package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWritablePathsRejectGitMetadataAliases(t *testing.T) {
	type aliasCase struct {
		name               string
		windowsSourceValid bool
	}
	aliases := []aliasCase{
		{".git.", false}, {".GiT ", false}, {".git . .", false}, {"GIT~1", false},
		{"git~1 .", false}, {".git:stream", false}, {".git::$INDEX_ALLOCATION", false}, {"git~1 . :stream", false},
	}
	// Git's HFS+ matcher ignores these exact code points at any position.
	for _, ignored := range []rune{0x200c, 0x200d, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f, 0xfeff} {
		aliases = append(aliases, aliasCase{".g" + string(ignored) + "it", true})
	}
	for _, alias := range aliases {
		for _, prefix := range []string{"", "parent/"} {
			name := prefix + alias.name + "/config"
			wantSourceValid := runtime.GOOS != "windows" || alias.windowsSourceValid
			if got := validSourcePath(name); got != wantSourceValid {
				t.Fatalf("source path %q validity=%t, want %t", name, got, wantSourceValid)
			}
			if ValidPath(name) {
				t.Fatalf("accepted writable Git metadata alias %q", name)
			}
		}
	}
	alias := ".g\u200cit/config"
	if err := ValidateChanges([]Change{{Path: alias, Mode: Regular}}); err == nil {
		t.Fatal("accepted metadata import")
	}
	if err := ValidateFixPaths([]File{{Path: alias, Mode: Regular}}, nil); err == nil {
		t.Fatal("accepted metadata-bearing writable source")
	}
	for _, test := range []struct {
		name         string
		windowsValid bool
	}{
		{"CON", false}, {"C:relative", false}, {".gitignore", true}, {".gitmodules", true}, {".gitkeep", true},
		{"git~2", true}, {"git~10", true}, {"git~1x", true}, {".git. x", true}, {".gitignore:stream", false},
		{".git\u200b", true}, {".git\u2060", true}, {".gİt", true}, {".g\u200cit.", false},
	} {
		want := runtime.GOOS != "windows" || test.windowsValid
		if got := ValidPath(test.name); got != want {
			t.Fatalf("ordinary path %q validity=%t, want %t", test.name, got, want)
		}
	}
}

func TestApplyRejectsMetadataAliasesBeforeAnyMutation(t *testing.T) {
	name := ".git./config"
	if runtime.GOOS == "windows" {
		name = ".g\u200cit/config"
	}
	root := privateRoot(t)
	if err := Materialize(root, []File{{Path: "source", Mode: Regular, Data: []byte("before")}}); err != nil {
		t.Fatal(err)
	}
	err := Apply(root, []Change{{Path: "source", Mode: Regular, Data: []byte("after")}, {Path: name, Mode: Regular, Data: []byte("untrusted Git configuration")}})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("accepted metadata import %q: %v", name, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "source"))
	if err != nil || string(data) != "before" {
		t.Fatalf("source was changed before metadata rejection: %q, %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "source" {
		t.Fatalf("metadata path created entries before rejection: %v, %v", entries, err)
	}
}

func TestCheckoutMetadataAliasesRespectGitSeparators(t *testing.T) {
	for _, name := range []string{`git~1/hooks/x`, `.git./x`, `.git\x`, `dir\.GIT/x`, `dir\git~1/x`, ".g\u200cit/x"} {
		if ValidCheckoutPath(name) {
			t.Errorf("checkout alias accepted: %q", name)
		}
	}
	for _, test := range []struct {
		name         string
		windowsValid bool
	}{
		{`dir/name`, true}, {`parent/dir/name`, true}, {`.gitignore/x`, true}, {`git~10/x`, true},
		{`dir\name`, false}, {`parent/dir\name`, false}, {`.gitignore\x`, false}, {`git~10\x`, false},
		{".g\u200cit\\x", false}, {"dir\\.g\u200cit/x", false},
	} {
		want := runtime.GOOS != "windows" || test.windowsValid
		if got := validSourcePath(test.name); got != want {
			t.Errorf("source path %q validity=%t, want %t", test.name, got, want)
		}
		if got := ValidCheckoutPath(test.name); got != want {
			t.Errorf("checkout path %q validity=%t, want %t", test.name, got, want)
		}
	}
}
