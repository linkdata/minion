package cleanup

import (
	"errors"
	"os"
	"path/filepath"
)

// RemoveAll anchors deletion in the trusted parent so a replaced entry cannot redirect cleanup.
func RemoveAll(path string) (resultErr error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	return parent.RemoveAll(filepath.Base(path))
}
