package minion

import _ "embed"

//go:embed config.json
var defaultConfig string

// DefaultConfig returns the config template bundled with the binary.
func DefaultConfig() string { return defaultConfig }
