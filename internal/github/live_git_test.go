package github

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLiveBotGitAccess(t *testing.T) {
	cfg := loadLiveTestConfig(t)
	if cfg.BotSSHKey == "" {
		t.Skip("bot_ssh_key is absent")
	}
	key := liveBotSSHKeyPath(t, cfg.BotSSHKey)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	ssh := exec.CommandContext(ctx, "ssh", "-F", os.DevNull, "-i", key, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-T", "git@github.com")
	output, err := ssh.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasPrefix(strings.TrimSpace(string(output)), "Hi "+cfg.BotLogin+"! You've successfully authenticated") {
		t.Fatalf("SSH identity for %s: %v: %s", cfg.BotLogin, err, strings.TrimSpace(string(output)))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(cfg.Repo) {
		t.Fatalf("repository name is unsafe for SSH: %q", cfg.Repo)
	}

	git := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", "git@github.com:"+cfg.Repo+".git", "HEAD")
	git.Dir = t.TempDir()
	git.Env = liveBotGitEnv(key)
	output, err = git.CombinedOutput()
	if err != nil {
		t.Fatalf("bot cannot read %s over SSH: %v: %s", cfg.Repo, err, strings.TrimSpace(string(output)))
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || len(fields[0]) != 40 || fields[1] != "HEAD" {
		t.Fatalf("unexpected git ls-remote output: %q", output)
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		t.Fatalf("invalid remote HEAD SHA %q: %v", fields[0], err)
	}
}

func liveBotSSHKeyPath(t *testing.T, key string) string {
	t.Helper()
	if !filepath.IsAbs(key) {
		key = filepath.Join("..", "..", key)
	}
	key, err := filepath.Abs(key)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func liveBotGitEnv(key string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(
		env,
		"MINION_LIVE_BOT_SSH_KEY="+key,
		`GIT_SSH_COMMAND=ssh -F /dev/null -i "$MINION_LIVE_BOT_SSH_KEY" -o IdentitiesOnly=yes -o IdentityAgent=none -o ControlMaster=no -o ControlPath=none -o BatchMode=yes -o StrictHostKeyChecking=yes`,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=ssh",
	)
}
