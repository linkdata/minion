package action

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	base, head, remote := gitFixtureRemote(t, workspace.File{Path: "fixture.txt", Mode: workspace.Regular, Data: []byte("literal fixture contents\n")})
	if !workspace.ValidSHA(base) || !workspace.ValidSHA(head) || base == head {
		t.Fatalf("fixture failed to create distinct commits: base=%q head=%q", base, head)
	}
	if got := modelGit(t, remote, "rev-parse", "refs/heads/main"); got != base+"\n" {
		t.Fatalf("fixture main ref=%q; want %s", got, base)
	}
	if got := modelGit(t, remote, "rev-parse", "refs/pull/7/head"); got != head+"\n" {
		t.Fatalf("fixture PR ref=%q; want %s", got, head)
	}
	if got := modelGit(t, remote, "show", head+":fixture.txt"); got != "literal fixture contents\n" {
		t.Fatalf("fixture committed unexpected source: %q", got)
	}
	if data, err := os.ReadFile(index); err != nil || string(data) != sentinel {
		t.Fatalf("fixture touched inherited index: contents=%q error=%v", data, err)
	}
	if _, err := os.Lstat(gitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture touched inherited repository directory: %v", err)
	}
}
