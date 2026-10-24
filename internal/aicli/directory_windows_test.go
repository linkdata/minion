//go:build windows

package aicli

import "testing"

func TestVolumeRootIncludesBareUNCShare(t *testing.T) {
	for _, path := range []string{`\\server\share`, `\\server\share\`, `C:\`} {
		if !isVolumeRoot(path) {
			t.Errorf("did not recognize volume root %q", path)
		}
	}
	for _, path := range []string{`\\server\share\child`, `C:\child`} {
		if isVolumeRoot(path) {
			t.Errorf("misclassified child %q as a volume root", path)
		}
	}
}
