package aicli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGitPathReadable(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "bin", "git")
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-other", "git")
	if !gitPathReadable(inside, []string{root}) || gitPathReadable(outside, []string{root}) {
		t.Fatal("Git grant must include a path, not merely share its string prefix")
	}
}

func TestValidateModelGitAccessRejectsGitOutsideModelGrants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell Git fixture is unavailable on native Windows")
	}
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	helper := filepath.Join(t.TempDir(), "git-core")
	if err := os.WriteFile(git, []byte("#!/bin/sh\n[ -z \"${GIT_EXEC_PATH+x}\" ] || exit 13\nprintf '%s\\n' "+ShellQuote(helper)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_EXEC_PATH", "/forged/git-core")
	for _, provider := range []string{"Codex", "Claude"} {
		if err := ValidateModelGitAccess(t.Context(), provider, []string{"/usr", "/bin"}); err == nil || !strings.Contains(err.Error(), "outside "+provider+" model read grants") || !strings.Contains(err.Error(), git) {
			t.Fatalf("unsupported Git installation was accepted by %s: %v", provider, err)
		}
		if err := ValidateModelGitAccess(t.Context(), provider, []string{bin}); err == nil || !strings.Contains(err.Error(), "outside "+provider+" model read grants") || !strings.Contains(err.Error(), helper) {
			t.Fatalf("unsupported Git helpers were accepted by %s: %v", provider, err)
		}
	}
}

func TestValidateModelGitAccessAllowsDirectoryAliasButRejectsFileAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell and symlink fixture is unavailable on native Windows")
	}
	physical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(physical, "git-core")
	git := filepath.Join(physical, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nprintf '%s\\n' "+ShellQuote(helper)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", alias+string(os.PathListSeparator)+path)
	if err := ValidateModelGitAccess(t.Context(), "Codex", []string{physical}); err != nil {
		t.Fatalf("directory alias to granted Git was refused: %v", err)
	}
	outside := t.TempDir()
	outsideGit := filepath.Join(outside, "git")
	if err := os.Symlink(git, outsideGit); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", outside+string(os.PathListSeparator)+path)
	if err := ValidateModelGitAccess(t.Context(), "Codex", []string{physical}); err == nil || !strings.Contains(err.Error(), outsideGit) {
		t.Fatalf("outside file alias was accepted: %v", err)
	}
}
