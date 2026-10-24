package codexcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestCheckLoginUsesPrivateCwdAndNativeEnvironment(t *testing.T) {
	t.Setenv("TMPDIR", canonicalTempDir(t))
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	binary := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$@" > "$0.args"
pwd > "$0.cwd"
printf '%s,%s,%s,%s\n' "${GH_TOKEN-absent}" "${GITHUB_TOKEN-absent}" "$TMPDIR" "$CODEX_HOME" > "$0.env"
`)
	t.Setenv("GH_TOKEN", "dummy-github-token")
	t.Setenv("GITHUB_TOKEN", "dummy-github-token")
	home := filepath.Join(canonicalTempDir(t), "not-created")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", relative)
	if err := CheckLogin(t.Context(), config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(binary + ".args")
	if err != nil || string(args) != "login\nstatus\n" {
		t.Fatalf("login arguments=%q, %v", args, err)
	}
	data, err := os.ReadFile(binary + ".cwd")
	if err != nil {
		t.Fatal(err)
	}
	private := strings.TrimSpace(string(data))
	if private == cwd || !strings.HasPrefix(filepath.Base(private), "minion-codex-login-") {
		t.Fatalf("login inherited working directory: %q", private)
	}
	data, err = os.ReadFile(binary + ".env")
	if err != nil || string(data) != "absent,absent,"+private+","+home+"\n" {
		t.Fatalf("login inherited service credentials or lost native paths: %q, %v", data, err)
	}
	if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login retained its private working directory: %v", err)
	}
}

func TestCheckLoginSelectedCredentialsDoNotInspectStoredLogin(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"configured", "dummy-key"}, {"CODEX_API_KEY", "dummy-environment-key"},
		{"CODEX_ACCESS_TOKEN", "dummy-environment-token"}, {"CODEX_API_KEY", " "},
	} {
		t.Run(test.key+"/"+test.value, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CODEX_API_KEY", "")
			t.Setenv("CODEX_ACCESS_TOKEN", "")
			settings := config.CodexConfig{}
			if test.key == "configured" {
				settings.APIKey = test.value
			} else {
				t.Setenv(test.key, test.value)
			}
			if err := CheckLogin(t.Context(), settings); err != nil {
				t.Fatalf("selected credentials checked unrelated stored login: %v", err)
			}
		})
	}
}

func TestCheckLoginRejectsFailedStoredLogin(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	installFakeCodex(t, "#!/bin/sh\nexit 7\n")
	if err := CheckLogin(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "codex login status failed") {
		t.Fatalf("failed stored login accepted: %v", err)
	}
}

func TestCheckLoginKeepsImplicitNativeHome(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	installFakeCodex(t, "#!/bin/sh\n[ \"${CODEX_HOME+x}\" != x ] || exit 3\n[ ! -e \"$HOME/.codex\" ] || exit 4\n")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	if err := CheckLogin(t.Context(), config.CodexConfig{}); err != nil {
		t.Fatalf("stored login injected a missing implicit home: %v", err)
	}
}

func TestCheckLoginRequiresKnownNativeHome(t *testing.T) {
	binary := installFakeCodexUnchecked(t, "#!/bin/sh\ntouch \"$0.called\"\n")
	t.Setenv("HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	if err := CheckLogin(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "resolve CODEX_HOME home") {
		t.Fatalf("unknown login home accepted: %v", err)
	}
	if _, err := os.Stat(binary + ".called"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown home launched login: %v", err)
	}
}
