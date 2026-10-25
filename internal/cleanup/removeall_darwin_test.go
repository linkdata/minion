//go:build darwin

package cleanup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinRemoveAllRestoresLockedDirectoriesWithoutFollowingLinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(root, "job")
	nested := filepath.Join(job, "nested")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{nested, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(job, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(job, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(job, 0700)
		_ = os.Chmod(nested, 0700)
	})
	if err := RemoveAll(job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Fatalf("job remains: %v", err)
	}
	info, err := os.Stat(secret)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("outside secret changed: %v, %v", info, err)
	}
}
