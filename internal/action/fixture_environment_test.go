package action

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

func TestGitFixtureRemoteIgnoresInheritedRepositorySelectors(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "missing.git")
	index := filepath.Join(root, "sentinel.index")
	const sentinel = "leave this index untouched\n"
	if err := os.WriteFile(index, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", gitDir)
	t.Setenv("GIT_INDEX_FILE", index)
	base, head, remote := gitFixtureRemoteRaw(t, workspace.File{Path: "fixture.txt", Mode: workspace.Regular, Data: []byte("literal fixture contents\n")})
	if !workspace.ValidSHA(base) || !workspace.ValidSHA(head) || base == head {
		t.Fatalf("fixture failed to create distinct commits: base=%q head=%q", base, head)
	}
	if got := modelGit(t, root, "--git-dir="+remote, "rev-parse", "refs/heads/main"); got != base+"\n" {
		t.Fatalf("fixture main ref=%q; want %s", got, base)
	}
	if got := modelGit(t, root, "--git-dir="+remote, "rev-parse", "refs/pull/7/head"); got != head+"\n" {
		t.Fatalf("fixture PR ref=%q; want %s", got, head)
	}
	if got := modelGit(t, root, "--git-dir="+remote, "show", head+":fixture.txt"); got != "literal fixture contents\n" {
		t.Fatalf("fixture committed unexpected source: %q", got)
	}
	if data, err := os.ReadFile(index); err != nil || string(data) != sentinel {
		t.Fatalf("fixture touched inherited index: contents=%q error=%v", data, err)
	}
	if _, err := os.Lstat(gitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture touched inherited repository directory: %v", err)
	}
}

func TestGitFixtureRemotePreservesOfflineProtocolDenial(t *testing.T) {
	_, _, remote := gitFixtureRemoteRaw(t)
	if out := modelGit(t, t.TempDir(), "ls-remote", remote); !strings.Contains(out, "\trefs/heads/main\n") {
		t.Fatalf("trusted local fixture remote is unavailable: %q", out)
	}
	out, err := testsupport.ModelGit(t, t.TempDir(), "-c", "protocol.allow=never", "ls-remote", remote)
	if err == nil || !strings.Contains(string(out), "transport 'file' not allowed") {
		t.Fatalf("fixture bypassed offline protocol denial: %s, %v", out, err)
	}
}
