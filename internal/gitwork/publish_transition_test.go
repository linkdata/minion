package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestCommitAndPushPreservesFileDirectoryTransitions(t *testing.T) {
	for _, transition := range []string{"file to directory", "directory to file"} {
		t.Run(transition, func(t *testing.T) {
			fixture := newFixture(t)
			changes := []workspace.Change{
				{Path: "README.md", Delete: true},
				{Path: "README.md/child", Mode: workspace.Executable, Data: []byte("nested\n")},
			}
			publishedPath, content := "README.md/child", "nested"
			if transition == "directory to file" {
				for _, path := range []string{"a/b", "a/c/d"} {
					path = filepath.Join(fixture.checkout.path, filepath.FromSlash(path))
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					writeFile(t, path, "old\n")
				}
				fixture.git(fixture.checkout.path, "add", ".")
				fixture.git(fixture.checkout.path, "commit", "-m", "directory baseline")
				fixture.base = fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
				fixture.checkout.expectedHead = fixture.base
				changes = []workspace.Change{
					{Path: "a", Mode: workspace.Executable, Data: []byte("file\n")},
					{Path: "a/b", Delete: true},
					{Path: "a/c/d", Delete: true},
				}
				publishedPath, content = "a", "file"
			}
			ctx := context.Background()
			const branch = "minion/fix-transition"
			if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			if err := fixture.checkout.Import(ctx, fixture.base, changes); err != nil {
				t.Fatalf("Import: %v", err)
			}
			var checkpoint string
			if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "example-bot", "fix transition", func(sha string) error {
				checkpoint = sha
				return nil
			}); err != nil {
				t.Fatalf("CommitAndPush after successful Import: %v", err)
			}
			ref := "refs/heads/" + branch
			if head := fixture.git("", "--git-dir="+fixture.remote, "rev-parse", ref); head != checkpoint {
				t.Fatalf("published head=%s checkpoint=%s", head, checkpoint)
			}
			if data := fixture.git("", "--git-dir="+fixture.remote, "show", ref+":"+publishedPath); data != content {
				t.Fatalf("published content=%q want=%q", data, content)
			}
			if entry := fixture.git("", "--git-dir="+fixture.remote, "ls-tree", "-r", ref, publishedPath); !strings.HasPrefix(entry, "100755 blob ") {
				t.Fatalf("published executable mode lost: %s", entry)
			}
			for _, change := range changes {
				if change.Delete {
					if entry := fixture.git("", "--git-dir="+fixture.remote, "ls-tree", ref, change.Path); entry != "" && !strings.HasPrefix(entry, "040000 tree ") {
						t.Fatalf("deleted path remains a blob: %s", entry)
					}
				}
			}
		})
	}
}
