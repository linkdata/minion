//go:build linux || darwin

package action

import (
	"testing"

	"github.com/linkdata/minion/internal/testtemp"
)

func TestMain(m *testing.M) { testtemp.Main(m) }
