package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func resolveAdminRepository(ctx context.Context, client *github.Client, cfg config.Config, rawURL string, actions []string) (config.Repository, error) {
	entry, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || entry == nil || !strings.EqualFold(entry.Scheme, "https") || !strings.EqualFold(entry.Host, "github.com") ||
		entry.User != nil || entry.Opaque != "" || strings.ContainsAny(rawURL, "%?#") {
		return config.Repository{}, errors.New("Enter a GitHub repository URL like https://github.com/owner/repo")
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(entry.Path, "/"), "/"))
	if !workspace.ValidRepository(name) {
		return config.Repository{}, errors.New("Enter a GitHub repository URL like https://github.com/owner/repo")
	}
	if _, exists := cfg.Repository(name); exists {
		return config.Repository{}, errors.New("Repository is already configured")
	}
	if client == nil {
		return config.Repository{}, errors.New("GitHub client is unavailable")
	}
	details, err := client.Repository(ctx, name)
	if err != nil {
		return config.Repository{}, fmt.Errorf("Check GitHub repository: %w", err)
	}
	if details.ID <= 0 || details.Owner.ID <= 0 {
		return config.Repository{}, errors.New("GitHub repository or owner identity is missing")
	}
	repo := config.Repository{Name: name, ID: details.ID, OwnerID: details.Owner.ID, DenyAllActors: true, Actions: actions}
	probe := cfg
	probe.Repositories = []config.Repository{repo}
	if err := checkRepositories(ctx, client, probe, log.New(io.Discard, "", 0)); err != nil {
		return config.Repository{}, fmt.Errorf("Repository is not ready: %w", err)
	}
	return repo, nil
}
