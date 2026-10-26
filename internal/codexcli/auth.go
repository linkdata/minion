package codexcli

import (
	"cmp"
	"os"
	"slices"
	"strings"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
)

// captureAmbientNativeCredentials binds inherited auth to this CLI call so it
// can be redacted and cannot silently change between calls in one session.
func captureAmbientNativeCredentials(settings config.CodexConfig) config.CodexConfig {
	if settings.APIKey == "" && settings.NativeAPIKey == "" && settings.NativeAccessToken == "" {
		settings.NativeAPIKey = os.Getenv("CODEX_API_KEY")
		settings.NativeAccessToken = os.Getenv("CODEX_ACCESS_TOKEN")
	}
	return settings
}

// nativeCredentialEnvironment passes selected native auth only to the CLI process.
func nativeCredentialEnvironment(env []string, runtimeDir, cacheDir, home string, settings config.CodexConfig, writablePaths ...string) []string {
	explicit := settings.NativeAPIKey != "" || settings.NativeAccessToken != ""
	clean := childEnvironment(env, runtimeDir, cacheDir, home, settings.APIKey != "" || explicit, writablePaths...)
	if settings.APIKey == "" {
		if settings.NativeAPIKey != "" {
			clean = append(clean, "CODEX_API_KEY="+settings.NativeAPIKey)
		}
		if settings.NativeAccessToken != "" {
			clean = append(clean, "CODEX_ACCESS_TOKEN="+settings.NativeAccessToken)
		}
	}
	return clean
}

func codexSecrets(settings config.CodexConfig) []string {
	secrets := []string{settings.APIKey, settings.NativeAPIKey, settings.NativeAccessToken}
	slices.SortFunc(secrets, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return secrets
}

func redactCodexText(value string, settings config.CodexConfig) string {
	secrets := codexSecrets(settings)
	for _, secret := range secrets {
		value = aicli.RedactText(value, secret)
	}
	for _, secret := range secrets {
		if aicli.RedactText(value, secret) != value {
			return ""
		}
	}
	return value
}

type codexRedactedError struct {
	cause   error
	message string
}

func (e codexRedactedError) Error() string { return e.message }
func (e codexRedactedError) Unwrap() error { return e.cause }

func redactCodexError(err error, settings config.CodexConfig) error {
	if err == nil {
		return nil
	}
	message := redactCodexText(err.Error(), settings)
	if message == err.Error() {
		return err
	}
	if message == "" {
		message = "Codex error could not be safely displayed"
	}
	return codexRedactedError{cause: err, message: message}
}

func redactCodexTail(tail *bounded.Tail, settings config.CodexConfig) string {
	var selected string
	for _, secret := range codexSecrets(settings) {
		if secret == "" {
			continue
		}
		if tail.Truncated && selected != "" && selected != secret {
			// A truncated prefix may contain an unredactable fragment of either key.
			return ""
		}
		selected = secret
	}
	if tail.Truncated {
		return aicli.RedactTail(tail, selected)
	}
	var clean bounded.Tail
	_, _ = clean.Write([]byte(strings.TrimSpace(redactCodexText(tail.String(), settings))))
	return aicli.RedactTail(&clean, "")
}
