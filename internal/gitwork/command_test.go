package gitwork

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestGitNeutralIdentityIgnoresAmbientEmailAndAllowsBotOverride(t *testing.T) {
	for _, email := range []string{"", "ambient@example.invalid"} {
		t.Run(email, func(t *testing.T) {
			t.Setenv("EMAIL", email)
			if email == "" {
				if err := os.Unsetenv("EMAIL"); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			ctx := context.Background()
			if email, err := runGit(ctx, dir, "config", "user.email"); err != nil || email != "minion@localhost" {
				t.Fatalf("controlled user.email=%q error=%v", email, err)
			}
			identity, err := runGit(ctx, dir, "var", "GIT_COMMITTER_IDENT")
			if err != nil || !strings.HasPrefix(identity, "Minion <minion@localhost> ") {
				t.Fatalf("neutral identity=%q error=%v", identity, err)
			}
			identity, err = runGit(ctx, dir, "-c", "user.name=example-bot", "-c", "user.email=example-bot@users.noreply.github.com", "var", "GIT_COMMITTER_IDENT")
			if err != nil || !strings.HasPrefix(identity, "example-bot <example-bot@users.noreply.github.com> ") {
				t.Fatalf("explicit identity=%q error=%v", identity, err)
			}
		})
	}
}

func TestGitDisablesAutomaticMaintenance(t *testing.T) {
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		value, err := runGit(context.Background(), t.TempDir(), "config", "--get", key)
		if err != nil || value != want {
			t.Fatalf("controlled Git %s=%q error=%v", key, value, err)
		}
	}
}
