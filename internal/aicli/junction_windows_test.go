//go:build windows

package aicli

import (
	"testing"

	"github.com/linkdata/minion/internal/wintest"
)

func makeJunctionWindows(t *testing.T, link, target string) {
	t.Helper()
	if err := wintest.CreateJunction(link, target); err != nil {
		t.Fatal(err)
	}
}
