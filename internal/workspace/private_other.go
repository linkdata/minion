//go:build !windows

package workspace

import (
	"fmt"
	"os"
)

func checkPrivateRoot(root string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("workspace root must be a private directory: %s (%s)", root, info.Mode())
	}
	return nil
}
