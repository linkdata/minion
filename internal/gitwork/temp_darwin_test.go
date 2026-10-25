//go:build darwin

package gitwork

import (
	"testing"

	"github.com/linkdata/minion/internal/testtemp"
)

func TestMain(m *testing.M) { testtemp.Main(m) }
