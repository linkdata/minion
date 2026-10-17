package workspace

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentitySegmentBounds(t *testing.T) {
	for _, value := range []string{"", ".", "..", "a/b", "a\nb", "café", strings.Repeat("a", 101)} {
		if ValidIdentitySegment(value) {
			t.Fatalf("accepted identifier %q", value)
		}
	}
	if !ValidIdentitySegment(strings.Repeat("a", 100)) || !ValidIdentitySegment("owner.repo_1-2") {
		t.Fatal("valid identifier refused")
	}
}

func TestWithinCanonicalPaths(t *testing.T) {
	root := t.TempDir()
	if !Within(root, root) || !Within(root, filepath.Join(root, "child")) || Within(root, root+"-sibling") || Within(root, filepath.Dir(root)) {
		t.Fatal("incorrect path containment")
	}
}
