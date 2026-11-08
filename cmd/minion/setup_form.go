package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

var newSetupGitHubClient = github.New // Test seam for the setup HTTP request.

func (a *adminAccess) setupPost() http.Handler {
	return adminConfigPost("/", []string{"bot_login", "github_token", "repository"}, "", func(ctx context.Context, values url.Values) error {
		bot := strings.ToLower(strings.TrimSpace(values.Get("bot_login")))
		token := values.Get("github_token")
		name := strings.ToLower(strings.TrimSpace(values.Get("repository")))
		if !validAdminGitHubLogin(bot) || token == "" || len(token) > 4096 || !workspace.ValidRepository(name) {
			return errors.New("Enter a bot login, token, and repository name")
		}
		client := newSetupGitHubClient(token)
		user, err := client.AuthenticatedUser(ctx)
		if err != nil || user.ID <= 0 || !strings.EqualFold(user.Login, bot) {
			return errors.New("GitHub token must authenticate as the configured bot login")
		}
		details, err := client.Repository(ctx, name)
		if err != nil || details.ID <= 0 || details.Owner.ID <= 0 || !strings.EqualFold(details.FullName, name) {
			return errors.New("GitHub repository could not be verified")
		}
		first := config.Repository{Name: name, ID: details.ID, OwnerID: details.Owner.ID,
			Actions: []string{"review"}, DenyAllActors: true}
		probe := config.Config{BotLogin: bot, Repositories: []config.Repository{first}}
		if err := checkRepositories(ctx, client, probe, log.New(io.Discard, "", 0)); err != nil {
			return errors.New("GitHub bot needs notification access and write permission to the repository")
		}
		_, err = config.UpdateWithAdminRuntime(a.setupPath, a.setupAdmin, func(cfg *config.Config) error {
			if cfg.Admin == nil {
				return errors.New("admin setup is no longer configured; restart Minion")
			}
			if len(cfg.Repositories) == 0 {
				cfg.Repositories = []config.Repository{first}
			} else if !strings.EqualFold(cfg.Repositories[0].Name, name) || !cfg.Repositories[0].MatchesIdentity(details.ID, details.Owner.ID) {
				return errors.New("the configured first repository differs; reload setup or use Advanced config")
			}
			cfg.BotLogin, cfg.GitHub.Token = bot, token
			return nil
		})
		return err
	})
}
