package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestWebservCertDirValidatesConfigWithoutListener(t *testing.T) {
	cfg := config.Config{
		BotLogin: "bot", Main: "codex",
		Admin:    &config.AdminConfig{Listen: "0.0.0.0:8377", Administrators: []config.AdminAccount{testAdminAccount()}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
	}
	certDir := t.TempDir()
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return errors.New("stopped after config") })
	}, "-oneshot", "-certdir="+certDir)
	if code != 1 || !strings.Contains(output, "Git preflight: stopped after config") {
		t.Fatalf("-oneshot with -certdir: exit=%d log=%q", code, output)
	}
	code, output = runStartupFixture(t, cfg, "-install-skills", "-certdir="+certDir)
	if code != 0 || !strings.Contains(output, "configured skills installed") {
		t.Fatalf("-install-skills with -certdir: exit=%d log=%q", code, output)
	}
}
