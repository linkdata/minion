package wintest

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func RequireHardLink(t *testing.T, source, link string) {
	t.Helper()
	if err := os.Link(source, link); err != nil {
		if errors.Is(err, errors.ErrUnsupported) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			t.Skipf("Windows hard links unavailable: %v", err)
		}
		t.Fatalf("create hard link %q -> %q: %v", link, source, err)
	}
}
