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
		repositoryID, repositoryErr := strconv.ParseInt(values.Get("repository_id"), 10, 64)
		ownerID, ownerErr := strconv.ParseInt(values.Get("owner_id"), 10, 64)
		if !validAdminGitHubLogin(bot) || token == "" || len(token) > 4096 || !workspace.ValidRepository(name) ||
			repositoryErr != nil || repositoryID <= 0 || ownerErr != nil || ownerID <= 0 {
			return errors.New("Enter a bot login, token, repository name, repository ID, and owner ID")
		}
		first := config.Repository{Name: name, ID: repositoryID, OwnerID: ownerID, DenyAllActors: true}
		_, err := config.UpdateWithAdminRuntime(a.setupPath, a.setupAdmin, func(cfg *config.Config) error {
			if cfg.Admin == nil {
				return errors.New("admin setup is no longer configured; restart Minion")
			}
			if len(cfg.Repositories) == 0 {
				if cfg.Commands == nil {
					first.Actions = []string{"review"}
				} else {
					if mapping, ok := cfg.Commands["review"]; ok && mapping.SupportsProvider(cfg.Main) && mapping.SupportedOnHost() {
						first.Actions = []string{"review"}
					} else {
						for _, name := range cfg.CommandNames() {
							mapping := cfg.Commands[name]
							if mapping.SupportsProvider(cfg.Main) && mapping.SupportedOnHost() {
								first.Actions = []string{name}
								break
							}
						}
						if len(cfg.Commands) > 0 && len(first.Actions) == 0 {
							return errors.New("No configured command can run with the selected main AI on this host")
						}
					}
				}
				cfg.Repositories = []config.Repository{first}
			} else if !strings.EqualFold(cfg.Repositories[0].Name, name) || !cfg.Repositories[0].MatchesIdentity(repositoryID, ownerID) {
				return errors.New("the configured first repository differs; reload setup or use Advanced config")
			}
			cfg.BotLogin, cfg.GitHub.Token = bot, token
			return nil
		})
		return err
	})
}
