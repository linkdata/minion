package workspace

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestFoldPathMatchesUnicodeSimpleCaseFolding(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		name := string(r)
		key := FoldPath(name)
		if !strings.EqualFold(name, key) {
			t.Fatalf("folded non-equivalent rune: %U => %q", r, key)
		}
		// Every orbit edge must keep its key; EqualFold checks the canonical
		// spelling independently without storing a million-rune lookup table.
		next := string(unicode.SimpleFold(r))
		if strings.EqualFold(name, next) && FoldPath(next) != key {
			t.Fatalf("equivalent runes have distinct keys: %U => %q", r, next)
		}
	}
}
