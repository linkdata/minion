package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritablePathsRejectGitMetadataAliases(t *testing.T) {
	aliases := []string{".git.", ".GiT ", ".git . .", "GIT~1", "git~1 .", ".git:stream", ".git::$INDEX_ALLOCATION", "git~1 . :stream"}
	// Git's HFS+ matcher ignores these exact code points at any position.
	for _, ignored := range []rune{0x200c, 0x200d, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f, 0xfeff} {
		for i := 0; i <= len(".git"); i++ {
			aliases = append(aliases, ".git"[:i]+string(ignored)+".git"[i:])
		}
	}
	for _, alias := range aliases {
		for _, prefix := range []string{"", "parent/"} {
			name := prefix + alias + "/config"
			if !validSourcePath(name) {
				t.Fatalf("strict writable policy changed exact source spelling %q", name)
			}
			if ValidPath(name) {
				t.Fatalf("accepted writable Git metadata alias %q", name)
			}
			if err := ValidateChanges([]Change{{Path: name, Mode: Regular}}); err == nil {
				t.Fatalf("accepted metadata import %q", name)
			}
			if err := ValidateFixPaths([]File{{Path: name, Mode: Regular}}, nil); err == nil {
				t.Fatalf("accepted metadata-bearing writable source %q", name)
			}
		}
	}
	for _, name := range []string{".gitignore", ".gitmodules", ".gitkeep", "git~2", "git~10", "git~1x", ".git. x", ".gitignore:stream", ".git\u200b", ".git\u2060", ".gİt", ".g\u200cit."} {
		if !ValidPath(name) {
			t.Fatalf("overbroad metadata rule rejected ordinary path %q", name)
		}
	}
}

func TestApplyRejectsMetadataAliasesBeforeAnyMutation(t *testing.T) {
	for _, name := range []string{".git./config", "nested/GIT~1/config", ".git::$INDEX_ALLOCATION/config", ".Git\u200c/config"} {
		t.Run(name, func(t *testing.T) {
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
		})
	}
}

func TestCheckoutMetadataAliasesRespectGitSeparators(t *testing.T) {
	for _, name := range []string{`git~1/hooks/x`, `.git./x`, `.git\x`, `dir\.GIT/x`, `dir\git~1/x`, ".g\u200cit/x"} {
		if ValidCheckoutPath(name) {
			t.Errorf("checkout alias accepted: %q", name)
		}
	}
	for _, name := range []string{`dir\name`, `parent/dir\name`, `.gitignore\x`, `git~10\x`, ".g\u200cit\\x", "dir\\.g\u200cit/x"} {
		if !validSourcePath(name) || !ValidCheckoutPath(name) {
			t.Errorf("legal exact source spelling rejected: %q", name)
		}
	}
}
