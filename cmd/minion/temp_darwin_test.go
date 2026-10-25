//go:build darwin

package main

import (
	"testing"

	"github.com/linkdata/minion/internal/testtemp"
)

func TestMain(m *testing.M) { testtemp.Main(m) }
