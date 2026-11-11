package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/config"
)

func addInvitationOwner(cfg *config.Config, login, rawID string) error {
	login = strings.ToLower(strings.TrimSpace(login))
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if !config.ValidGitHubLogin(login) || err != nil || id <= 0 {
		return errors.New("Enter a valid GitHub owner login and positive numeric ID")
	}
	for _, existing := range cfg.InvitationOwners {
		if strings.EqualFold(existing, login) {
			return errors.New("Owner is already approved")
		}
	}
	cfg.InvitationOwners = append(cfg.InvitationOwners, login)
	if cfg.InvitationOwnerIDs == nil {
		cfg.InvitationOwnerIDs = make(map[string]int64)
	}
	cfg.InvitationOwnerIDs[login] = id
	return nil
}

func removeInvitationOwner(cfg *config.Config, login string) error {
	if login == "" {
		return errors.New("Enter an approved owner")
	}
	for i, existing := range cfg.InvitationOwners {
		if strings.EqualFold(existing, login) {
			cfg.InvitationOwners = slices.Delete(cfg.InvitationOwners, i, i+1)
			delete(cfg.InvitationOwnerIDs, strings.ToLower(existing))
			return nil
		}
	}
	return errors.New("Owner is not approved")
}

func (a *adminAccess) setupInvitationOwnerAdd() http.Handler {
	return adminConfigPost("/", []string{"version", "login", "id"}, "", func(_ context.Context, values url.Values) error {
		return a.updateSetup(values.Get("version"), func(cfg *config.Config) error {
			return addInvitationOwner(cfg, values.Get("login"), values.Get("id"))
		})
	})
}

func (a *adminAccess) setupInvitationOwnerRemove() http.Handler {
	return adminConfigPost("/", []string{"version", "login"}, "", func(_ context.Context, values url.Values) error {
		return a.updateSetup(values.Get("version"), func(cfg *config.Config) error {
			return removeInvitationOwner(cfg, values.Get("login"))
		})
	})
}
