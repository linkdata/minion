package codexcli

import (
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestNativeCredentialEnvironmentRejectsCredentialsAddedAfterCapture(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	settings := CaptureAmbientNativeCredentials(config.CodexConfig{})
	env := nativeCredentialEnvironment([]string{
		"HOME=/trusted/home",
		"CODEX_API_KEY=late-key",
		"CODEX_ACCESS_TOKEN=late-token",
	}, "/run", "/cache", "/home", settings)
	for _, entry := range env {
		if strings.HasPrefix(entry, "CODEX_API_KEY=") || strings.HasPrefix(entry, "CODEX_ACCESS_TOKEN=") {
			t.Fatal("uncaptured credential reached child")
		}
	}
}
