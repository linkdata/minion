package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func (a *adminAccess) setupPost() http.Handler {
	return adminConfigPost("/", []string{"bot_login", "github_token", "repository", "repository_id", "owner_id"}, "", func(_ context.Context, values url.Values) error {
		bot := strings.ToLower(strings.TrimSpace(values.Get("bot_login")))
		token := values.Get("github_token")
		name := strings.ToLower(strings.TrimSpace(values.Get("repository")))
		id, idErr := strconv.ParseInt(values.Get("repository_id"), 10, 64)
		ownerID, ownerErr := strconv.ParseInt(values.Get("owner_id"), 10, 64)
		if !validAdminGitHubLogin(bot) || token == "" || len(token) > 4096 || !workspace.ValidRepository(name) ||
			idErr != nil || ownerErr != nil || id <= 0 || ownerID <= 0 {
			return errors.New("Enter a bot login, token, repository name, and positive GitHub IDs")
		}
		_, err := config.UpdateWithAdminRuntime(a.setupPath, a.setupAdmin, func(cfg *config.Config) error {
			if cfg.Admin == nil {
				return errors.New("admin setup is no longer configured; restart Minion")
			}
			if len(cfg.Repositories) == 0 {
				cfg.Repositories = []config.Repository{{Name: name, ID: id, OwnerID: ownerID,
					Actions: []string{"review"}, DenyAllActors: true}}
			} else if !strings.EqualFold(cfg.Repositories[0].Name, name) || !cfg.Repositories[0].MatchesIdentity(id, ownerID) {
				return errors.New("the configured first repository differs; reload setup or use Advanced config")
			}
			cfg.BotLogin, cfg.GitHub.Token = bot, token
			return nil
		})
		return err
	})
}
