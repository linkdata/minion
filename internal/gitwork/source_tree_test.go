package gitwork

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func treeLine(mode, kind, size, name string) string {
	return fmt.Sprintf("%s %s %s %s\t%s\x00", mode, kind, strings.Repeat("a", 40), size, name)
}

func TestParseSourceEntries(t *testing.T) {
	sha := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, raw, msg string
		want           error
		files          int
	}{
		{name: "empty", files: 0},
		{name: "regular", raw: treeLine("100644", "blob", "1", "x"), files: 1},
		{name: "executable", raw: treeLine("100755", "blob", "1", "x"), files: 1},
		{name: "symlink", raw: treeLine("120000", "blob", "1", "link"), files: 1},
		{name: "gitlink", raw: treeLine("160000", "commit", "-", "sub"), files: 1},
		{name: "tree and file", raw: treeLine("040000", "tree", "-", "dir") + treeLine("100644", "blob", "1", "dir/x"), files: 1},
		{name: "unterminated", raw: strings.TrimSuffix(treeLine("100644", "blob", "1", "x"), "\x00"), want: ErrUnsupportedSource},
		{name: "missing size", raw: "100644 blob " + strings.Repeat("a", 40) + "\tx\x00", want: ErrUnsupportedSource},
		{name: "bad size", raw: treeLine("100644", "blob", "-", "x"), want: ErrUnsupportedSource},
		{name: "negative size", raw: treeLine("100644", "blob", "-1", "x"), want: ErrUnsupportedSource},
		{name: "bad hash", raw: "100644 blob z 1\tx\x00", want: ErrUnsupportedSource},
		{name: "unknown type", raw: treeLine("100644", "unknown", "1", "x"), want: ErrUnsupportedSource},
		{name: "invalid blob mode", raw: treeLine("100664", "blob", "1", "x"), want: ErrUnsupportedSource},
		{name: "invalid tree mode", raw: treeLine("120000", "tree", "-", "x"), want: ErrUnsupportedSource},
		{name: "invalid gitlink mode", raw: treeLine("100644", "commit", "-", "x"), want: ErrUnsupportedSource},
		{name: "sized tree", raw: treeLine("040000", "tree", "1", "x"), want: ErrUnsupportedSource},
		{name: "sized gitlink", raw: treeLine("160000", "commit", "1", "x"), want: ErrUnsupportedSource},
		{name: "unsafe metadata alias", raw: treeLine("100644", "blob", "1", "git~1/config"), want: ErrUnsupportedSource},
		{name: "duplicate", raw: treeLine("100644", "blob", "1", "x") + treeLine("100644", "blob", "1", "x"), want: ErrUnsupportedSource},
		{name: "duplicate file and tree", raw: treeLine("100644", "blob", "1", "a") + treeLine("040000", "tree", "-", "a") + treeLine("100644", "blob", "1", "a/b"), want: ErrUnsupportedSource, msg: "duplicate source path"},
		{name: "file-directory collision without tree entry", raw: treeLine("100644", "blob", "1", "a") + treeLine("100644", "blob", "1", "a/b"), want: ErrUnsupportedSource, msg: "file/directory collision"},
		{name: "exact bytes", raw: treeLine("100644", "blob", fmt.Sprint(workspace.MaxBytes), "x"), files: 1},
		{name: "oversized symlink", raw: treeLine("120000", "blob", fmt.Sprint(workspace.MaxBytes+1), "link"), want: ErrCheckoutTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := parseSourceEntries(sha, []byte(tc.raw))
			if !errors.Is(err, tc.want) || len(entries) != tc.files || (tc.msg != "" && !strings.Contains(err.Error(), tc.msg)) {
				t.Fatalf("entries=%d error=%v, want entries=%d error=%v containing %q", len(entries), err, tc.files, tc.want, tc.msg)
			}
		})
	}
}

func TestParseSourceEntriesCountsFilesAndMetadata(t *testing.T) {
	sha := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, line string
		count      int
		entries    int
		want       error
	}{
		{"exact files", treeLine("160000", "commit", "-", "x"), workspace.MaxFiles, workspace.MaxFiles, nil},
		{"too many files", treeLine("160000", "commit", "-", "x"), workspace.MaxFiles + 1, 0, ErrCheckoutTooLarge},
		{"exact entries", treeLine("040000", "tree", "-", "x"), workspace.MaxEntries, 0, nil},
		{"too many entries", treeLine("040000", "tree", "-", "x"), workspace.MaxEntries + 1, 0, ErrCheckoutTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw strings.Builder
			for i := range tc.count {
				raw.WriteString(strings.Replace(tc.line, "\tx\x00", fmt.Sprintf("\tentry-%06d\x00", i), 1))
			}
			entries, err := parseSourceEntries(sha, []byte(raw.String()))
			if !errors.Is(err, tc.want) || len(entries) != tc.entries {
				t.Fatalf("count=%d entries=%d error=%v, want entries=%d error=%v", tc.count, len(entries), err, tc.entries, tc.want)
			}
		})
	}
}

func TestSourceTreeRejectsInvalidRef(t *testing.T) {
	var checkout Checkout
	_, err := checkout.sourceTree(context.Background(), "--output=x")
	if !errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), "invalid source identity") {
		t.Fatalf("invalid source identity error=%v", err)
	}
}
