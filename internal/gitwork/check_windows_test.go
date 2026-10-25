package gitwork

import "testing"

func TestCheckGitNativeWindows(t *testing.T) {
	root := t.TempDir()
	if err := Check(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	assertGitCheckClean(t, root)
}
