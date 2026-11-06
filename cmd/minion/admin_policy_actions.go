package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func policyRounds(raw string, required bool) (*int, error) {
	if raw == "" && !required {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > config.MaxCriticMaxRounds {
		return nil, fmt.Errorf("required clean reviews must be between 0 and %d", config.MaxCriticMaxRounds)
	}
	return &value, nil
}

func registerAdminPolicyActions(mux *http.ServeMux, live *adminLiveConfig, owner *action.Orchestrator) {
	mux.Handle("POST /admin/policy/global", adminConfigPost("/admin/policy", []string{"rounds"}, "", func(_ context.Context, values url.Values) error {
		rounds, err := policyRounds(values.Get("rounds"), true)
		if err != nil {
			return err
		}
		return live.save(func(cfg *config.Config) error {
			cfg.CriticStopping.RequiredCleanRounds = rounds
			return nil
		})
	}))
	mux.Handle("POST /admin/policy/requester", adminConfigPost("/admin/policy", []string{"repository", "login", "rounds"}, "", func(ctx context.Context, values url.Values) error {
		repo, login := strings.ToLower(values.Get("repository")), strings.ToLower(values.Get("login"))
		rounds, err := policyRounds(values.Get("rounds"), true)
		if err != nil {
			return err
		}
		if !workspace.ValidRepository(repo) || !validAdminGitHubLogin(login) {
			return errors.New("enter a configured repository and valid GitHub login")
		}
		if _, ok := live.snapshot().Config.Repository(repo); !ok || owner == nil || owner.GitHub == nil {
			return errors.New("repository or GitHub client is unavailable")
		}
		permission, err := owner.GitHub.Permission(ctx, repo, login)
		if err != nil {
			return fmt.Errorf("check requester permission: %w", err)
		}
		if permission.User.ID <= 0 || !strings.EqualFold(permission.User.Login, login) || !adminCanWrite(permission.Permission) {
			return errors.New("requester must have write, maintain, or admin permission")
		}
		return live.save(func(cfg *config.Config) error {
			if _, ok := cfg.Repository(repo); !ok {
				return errors.New("repository is no longer configured")
			}
			for i := range cfg.RequesterPolicies {
				policy := &cfg.RequesterPolicies[i]
				if policy.ID == permission.User.ID {
					policy.Login, policy.RequiredCleanRounds = login, rounds
					return nil
				}
			}
			cfg.RequesterPolicies = append(cfg.RequesterPolicies, config.RequesterPolicy{Login: login, ID: permission.User.ID, RequiredCleanRounds: rounds})
			return nil
		})
	}))
	mux.Handle("POST /admin/policy/requester/remove", adminConfigPost("/admin/policy", []string{"id"}, "", func(_ context.Context, values url.Values) error {
		id, err := strconv.ParseInt(values.Get("id"), 10, 64)
		if err != nil || id <= 0 {
			return errors.New("invalid requester ID")
		}
		return live.save(func(cfg *config.Config) error {
			for i, policy := range cfg.RequesterPolicies {
				if policy.ID == id {
					cfg.RequesterPolicies = slices.Delete(cfg.RequesterPolicies, i, i+1)
					return nil
				}
			}
			return errors.New("requester override is not configured")
		})
	}))
	mux.Handle("POST /admin/policy/repository", adminConfigPost("/admin/policy", []string{"repository", "rounds"}, "", func(_ context.Context, values url.Values) error {
		repo := strings.ToLower(values.Get("repository"))
		if !workspace.ValidRepository(repo) {
			return errors.New("invalid repository")
		}
		rounds, err := policyRounds(values.Get("rounds"), false)
		if err != nil {
			return err
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				if cfg.Repositories[i].Name == repo {
					cfg.Repositories[i].RequiredCleanRounds = rounds
					return nil
				}
			}
			return errors.New("repository is not configured")
		})
	}))
	mux.Handle("POST /admin/policy/instructions", adminConfigPost("/admin/policy", []string{"instructions"}, "", func(_ context.Context, values url.Values) error {
		instructions := strings.ReplaceAll(values.Get("instructions"), "\r\n", "\n")
		if strings.TrimSpace(instructions) == "" {
			return errors.New("instructions cannot be blank; use Reset to restore the built-in default")
		}
		return live.save(func(cfg *config.Config) error {
			cfg.AIInstructions = instructions
			return nil
		})
	}))
	mux.Handle("POST /admin/policy/instructions/reset", adminConfigPost("/admin/policy", nil, "", func(_ context.Context, _ url.Values) error {
		return live.save(func(cfg *config.Config) error {
			cfg.AIInstructions = ""
			return nil
		})
	}))
}
