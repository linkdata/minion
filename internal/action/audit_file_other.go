//go:build !linux

package action

import (
	"errors"
	"os"
)

func openAuditArtifact(path string) (*os.File, error) {
	return nil, errors.New("audit artifact verification requires Linux")
}
