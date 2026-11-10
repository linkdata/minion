package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func (a *adminAccess) setupPost() http.Handler {
	return adminConfigPost("/", []string{"version", "bot_login", "github_token"}, "", func(_ context.Context, values url.Values) error {
		bot := strings.ToLower(strings.TrimSpace(values.Get("bot_login")))
		token := values.Get("github_token")
		if !config.ValidGitHubLogin(bot) || len(token) > 4096 {
			return errors.New("Enter a valid GitHub bot login and token")
		}
		_, err := config.UpdateWithAdminRuntime(a.setupPath, a.setupAdmin, func(cfg *config.Config) error {
			if cfg.Admin == nil {
				return errors.New("admin setup is no longer configured; restart Minion")
			}
			version, err := advancedVersion(*cfg)
			if err != nil {
				return err
			}
			before, err := cloneConfig(*cfg)
			if err != nil {
				return err
			}
			if err := before.Validate(filepath.Dir(a.setupPath)); err != nil {
				return err
			}
			if version != values.Get("version") {
				return errors.New("configuration changed; reload before saving")
			}
			if token == "" && cfg.GitHub.Token == "" {
				return errors.New("Enter a GitHub bot token")
			}
			cfg.BotLogin = bot
			if token != "" {
				cfg.GitHub.Token = token
			}
			return nil
		})
		return err
	})
}

func setupRepositoryActions(cfg config.Config) ([]string, error) {
	if cfg.Commands == nil {
		return []string{"review"}, nil
	}
	if mapping, ok := cfg.Commands["review"]; ok && mapping.SupportsProvider(cfg.Main) && mapping.SupportedOnHost() {
		return []string{"review"}, nil
	}
	for _, name := range cfg.CommandNames() {
		mapping := cfg.Commands[name]
		if mapping.SupportsProvider(cfg.Main) && mapping.SupportedOnHost() {
			return []string{name}, nil
		}
	}
	if len(cfg.Commands) > 0 {
		return nil, errors.New("No configured command can run with the selected main AI on this host")
	}
	return nil, nil
}

func (a *adminAccess) setupRepositoryPost() http.Handler {
	return adminConfigPost("/", []string{"version", "url"}, "", func(ctx context.Context, values url.Values) error {
		cfg, version, err := loadSetupConfig(a.setupPath, a.setupAdmin)
		if err != nil {
			return err
		}
		if version != values.Get("version") {
			return errors.New("configuration changed; reload before saving")
		}
		if cfg.BotLogin == "" || cfg.GitHub.Token == "" {
			return errors.New("Save the GitHub bot credentials before adding a repository")
		}
		actions, err := setupRepositoryActions(cfg)
		if err != nil {
			return err
		}
		repo, err := resolveAdminRepository(ctx, github.New(cfg.GitHub.Token), cfg, values.Get("url"), actions)
		if err != nil {
			return err
		}
		_, err = config.UpdateWithAdminRuntime(a.setupPath, a.setupAdmin, func(current *config.Config) error {
			if current.Admin == nil {
				return errors.New("admin setup is no longer configured; restart Minion")
			}
			currentVersion, err := advancedVersion(*current)
			if err != nil {
				return err
			}
			if currentVersion != version {
				return errors.New("configuration changed; reload before saving")
			}
			if _, exists := current.Repository(repo.Name); exists {
				return errors.New("Repository is already configured")
			}
			current.Repositories = append(current.Repositories, repo)
			return nil
		})
		return err
	})
}
