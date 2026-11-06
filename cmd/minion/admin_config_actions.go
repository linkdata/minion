package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

func registerAdminConfigActions(mux *http.ServeMux, live *adminLiveConfig, owner *action.Orchestrator) {
	if live == nil {
		return
	}
	mux.Handle("POST /admin/users/add", adminConfigPost("/admin/users", []string{"repository", "login"}, "", func(ctx context.Context, values url.Values) error {
		repoName, login := strings.ToLower(values.Get("repository")), strings.ToLower(values.Get("login"))
		if !workspace.ValidRepository(repoName) || !validAdminGitHubLogin(login) {
			return errors.New("Enter a configured repository and valid GitHub login")
		}
		if _, ok := live.snapshot().Config.Repository(repoName); !ok {
			return errors.New("Repository is not configured")
		}
		if owner == nil || owner.GitHub == nil {
			return errors.New("GitHub client is unavailable")
		}
		permission, err := owner.GitHub.Permission(ctx, repoName, login)
		if err != nil {
			return fmt.Errorf("Check requester permission: %w", err)
		}
		if permission.User.ID <= 0 || !strings.EqualFold(permission.User.Login, login) || !adminCanWrite(permission.Permission) {
			return errors.New("Requester must have write, maintain, or admin permission on this repository")
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				repo := &cfg.Repositories[i]
				if !strings.EqualFold(repo.Name, repoName) {
					continue
				}
				for _, existing := range repo.AllowedActors {
					if strings.EqualFold(existing, login) {
						return errors.New("Requester is already allowed")
					}
				}
				repo.AllowedActors = append(repo.AllowedActors, login)
				if repo.AllowedActorIDs == nil {
					repo.AllowedActorIDs = make(map[string]int64)
				}
				repo.AllowedActorIDs[login] = permission.User.ID
				return nil
			}
			return errors.New("Repository is no longer configured")
		})
	}))
	mux.Handle("POST /admin/users/remove", adminConfigPost("/admin/users", []string{"repository", "login"}, "", func(_ context.Context, values url.Values) error {
		repoName, login := strings.ToLower(values.Get("repository")), strings.ToLower(values.Get("login"))
		if !workspace.ValidRepository(repoName) || !validAdminGitHubLogin(login) {
			return errors.New("Enter a configured repository and valid GitHub login")
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				repo := &cfg.Repositories[i]
				if !strings.EqualFold(repo.Name, repoName) {
					continue
				}
				if repo.AllowedActors == nil {
					return errors.New("This repository allows any writer; there is no named requester to remove")
				}
				for j, existing := range repo.AllowedActors {
					if strings.EqualFold(existing, login) {
						repo.AllowedActors = slices.Delete(repo.AllowedActors, j, j+1)
						delete(repo.AllowedActorIDs, strings.ToLower(existing))
						return nil
					}
				}
				return errors.New("Requester is not in this repository's allowlist")
			}
			return errors.New("Repository is not configured")
		})
	}))
	mux.Handle("POST /admin/users/allow-any", adminConfigPost("/admin/users", []string{"repository"}, "", func(_ context.Context, values url.Values) error {
		repoName := strings.ToLower(values.Get("repository"))
		if !workspace.ValidRepository(repoName) {
			return errors.New("Enter a configured repository")
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				repo := &cfg.Repositories[i]
				if strings.EqualFold(repo.Name, repoName) {
					repo.AllowedActors, repo.AllowedActorIDs = nil, nil
					return nil
				}
			}
			return errors.New("Repository is not configured")
		})
	}))
	mux.Handle("POST /admin/repositories/add", adminConfigPost("/admin/repositories", []string{"name"}, "actions", func(ctx context.Context, values url.Values) error {
		name := strings.ToLower(values.Get("name"))
		if !workspace.ValidRepository(name) {
			return errors.New("Repository must use owner/name")
		}
		actions := values["actions"]
		current := live.snapshot().Config
		if _, exists := current.Repository(name); exists {
			return errors.New("Repository is already configured")
		}
		if err := validateAdminActions(current, actions); err != nil {
			return err
		}
		if owner == nil || owner.GitHub == nil {
			return errors.New("GitHub client is unavailable")
		}
		details, err := owner.GitHub.Repository(ctx, name)
		if err != nil {
			return fmt.Errorf("Check GitHub repository: %w", err)
		}
		if details.ID <= 0 || details.Owner.ID <= 0 {
			return errors.New("GitHub repository or owner identity is missing")
		}
		repo := config.Repository{Name: name, ID: details.ID, OwnerID: details.Owner.ID, AllowedActors: []string{}, Actions: actions}
		probe := current
		probe.Repositories = []config.Repository{repo}
		if err := checkRepositories(ctx, owner.GitHub, probe, log.New(io.Discard, "", 0)); err != nil {
			return fmt.Errorf("Repository is not ready: %w", err)
		}
		return live.save(func(cfg *config.Config) error {
			if _, exists := cfg.Repository(name); exists {
				return errors.New("Repository is already configured")
			}
			if err := validateAdminActions(*cfg, actions); err != nil {
				return err
			}
			cfg.Repositories = append(cfg.Repositories, repo)
			return nil
		})
	}))
	mux.Handle("POST /admin/repositories/actions", adminConfigPost("/admin/repositories", []string{"name"}, "actions", func(ctx context.Context, values url.Values) error {
		name, actions := strings.ToLower(values.Get("name")), values["actions"]
		if !workspace.ValidRepository(name) {
			return errors.New("Repository must use owner/name")
		}
		current := live.snapshot().Config
		if err := validateAdminActions(current, actions); err != nil {
			return err
		}
		repo, ok := current.Repository(name)
		if !ok {
			return errors.New("Repository is not configured")
		}
		workerRestart := current.RemoteExecutor != nil && hasNewCommand(current, config.Config{Repositories: []config.Repository{{Actions: actions}}})
		for _, action := range actions {
			if slices.Contains(repo.Actions, action) {
				continue
			}
			if !workerRestart {
				if owner == nil || owner.GitHub == nil {
					return errors.New("GitHub client is unavailable")
				}
				repo.Actions = actions
				probe := current
				probe.Repositories = []config.Repository{repo}
				if err := checkRepositories(ctx, owner.GitHub, probe, log.New(io.Discard, "", 0)); err != nil {
					return fmt.Errorf("Repository is not ready: %w", err)
				}
			}
			break
		}
		return live.save(func(cfg *config.Config) error {
			if err := validateAdminActions(*cfg, actions); err != nil {
				return err
			}
			for i := range cfg.Repositories {
				if strings.EqualFold(cfg.Repositories[i].Name, name) {
					cfg.Repositories[i].Actions = actions
					return nil
				}
			}
			return errors.New("Repository is not configured")
		})
	}))
	mux.Handle("POST /admin/repositories/remove", adminConfigPost("/admin/repositories", []string{"name"}, "", func(_ context.Context, values url.Values) error {
		name := strings.ToLower(values.Get("name"))
		if !workspace.ValidRepository(name) {
			return errors.New("Repository must use owner/name")
		}
		return live.save(func(cfg *config.Config) error {
			if len(cfg.Repositories) <= 1 {
				return errors.New("At least one repository is required")
			}
			for i, repo := range cfg.Repositories {
				if strings.EqualFold(repo.Name, name) {
					cfg.Repositories = slices.Delete(cfg.Repositories, i, i+1)
					return nil
				}
			}
			return errors.New("Repository is not configured")
		})
	}))
	mux.Handle("POST /admin/skills/add", adminConfigPost("/admin/skills", []string{"target", "url"}, "", func(ctx context.Context, values url.Values) error {
		target, source := values.Get("target"), values.Get("url")
		if len(source) > config.MaxSkillURLBytes {
			return errors.New("Skill URL is too long")
		}
		if _, err := skills.Name(source); err != nil {
			return fmt.Errorf("Invalid skill URL: %w", err)
		}
		if live.snapshot().Remote {
			return errors.New("Skill changes require restarting the remote worker")
		}
		edit := func(cfg *config.Config) error {
			return editAdminSkills(cfg, target, func(urls []string) ([]string, error) {
				if slices.Contains(urls, source) {
					return nil, errors.New("Skill is already configured for this target")
				}
				return append(urls, source), nil
			})
		}
		if err := preflightAdminSkillEdit(live, edit); err != nil {
			return err
		}
		current := live.snapshot().Config
		needsInstall := false
		providers := []string{current.Main}
		if target == "always" {
			providers = append(providers, current.Adversarial)
		}
		for _, provider := range providers {
			if provider == "" {
				continue
			}
			_, installed, err := skills.InstalledPathFor(source, provider)
			if err != nil {
				return fmt.Errorf("Check installed skill: %w", err)
			}
			needsInstall = needsInstall || !installed
		}
		if needsInstall {
			if adminHasSkillSource(current, source) || adminHasSkillSource(live.current(), source) {
				return errors.New("Skill is already configured but missing for a selected AI; install configured skills after active actions finish, then retry")
			}
			if err := skills.Install(ctx, []string{source}); err != nil {
				return fmt.Errorf("Install skill: %w", err)
			}
		}
		return live.save(edit)
	}))
	mux.Handle("POST /admin/skills/remove", adminConfigPost("/admin/skills", []string{"target", "url"}, "", func(_ context.Context, values url.Values) error {
		target, source := values.Get("target"), values.Get("url")
		if len(source) > config.MaxSkillURLBytes || source == "" {
			return errors.New("Invalid skill URL")
		}
		return live.save(func(cfg *config.Config) error {
			return editAdminSkills(cfg, target, func(urls []string) ([]string, error) {
				for i, configured := range urls {
					if configured == source {
						return slices.Delete(urls, i, i+1), nil
					}
				}
				return nil, errors.New("Skill is not configured for this target")
			})
		})
	}))
}

func adminConfigPost(back string, fields []string, repeated string, action func(context.Context, url.Values) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		parseErr := r.ParseForm()
		closeUnreadAdminBody(w, r)
		want := len(fields)
		if repeated != "" && len(r.PostForm[repeated]) != 0 {
			want++
		}
		if parseErr != nil || len(r.PostForm) != want {
			http.Error(w, "Invalid configuration form", http.StatusBadRequest)
			return
		}
		for _, field := range fields {
			if len(r.PostForm[field]) != 1 {
				http.Error(w, "Invalid configuration form", http.StatusBadRequest)
				return
			}
		}
		if err := action(r.Context(), r.PostForm); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	})
}

func validAdminGitHubLogin(login string) bool {
	if len(login) == 0 || len(login) > 39 || login[0] == '-' || login[len(login)-1] == '-' {
		return false
	}
	for _, ch := range login {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func adminCanWrite(permission string) bool {
	return permission == "write" || permission == "maintain" || permission == "admin"
}

func validateAdminActions(cfg config.Config, actions []string) error {
	if len(actions) == 0 || len(actions) > len(cfg.Commands) {
		return errors.New("Select at least one configured command")
	}
	selected := make(map[string]bool, len(actions))
	for _, name := range actions {
		mapping, exists := cfg.Commands[name]
		if !exists || !mapping.SupportsProvider(cfg.Main) || selected[name] {
			return errors.New("Select distinct commands supported by the configured main AI")
		}
		selected[name] = true
	}
	return nil
}

func adminHasSkillSource(cfg config.Config, source string) bool {
	if slices.Contains(cfg.AlwaysOnSkills, source) {
		return true
	}
	for _, command := range cfg.Commands {
		if slices.Contains(command.Skills, source) {
			return true
		}
	}
	return false
}

func editAdminSkills(cfg *config.Config, target string, change func([]string) ([]string, error)) error {
	if target == "always" {
		updated, err := change(cfg.AlwaysOnSkills)
		if err == nil {
			cfg.AlwaysOnSkills = updated
		}
		return err
	}
	name, ok := strings.CutPrefix(target, "command:")
	if !ok || name == "" {
		return errors.New("Select a configured skill target")
	}
	mapping, ok := cfg.Commands[name]
	if !ok {
		return errors.New("Selected command is not configured")
	}
	updated, err := change(mapping.Skills)
	if err != nil {
		return err
	}
	mapping.Skills = updated
	cfg.Commands[name] = mapping
	return nil
}

func preflightAdminSkillEdit(live *adminLiveConfig, edit func(*config.Config) error) error {
	data, err := json.Marshal(live.snapshot().Config)
	if err != nil {
		return err
	}
	var candidate config.Config
	if err := json.Unmarshal(data, &candidate); err != nil {
		return err
	}
	if err := edit(&candidate); err != nil {
		return err
	}
	return candidate.Validate(filepath.Dir(live.path))
}
