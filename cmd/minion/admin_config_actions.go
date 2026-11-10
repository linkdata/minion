package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

func registerAdminConfigActions(mux *http.ServeMux, live *adminLiveConfig, owner *action.Orchestrator) {
	if live == nil {
		return
	}
	var skillMu sync.Mutex
	mux.Handle("POST /admin/users/add", adminConfigPost("/admin/users", []string{"repository", "login"}, "", func(ctx context.Context, values url.Values) error {
		repoName, login := strings.ToLower(values.Get("repository")), strings.ToLower(values.Get("login"))
		configured, ok := live.snapshot().Config.Repository(repoName)
		if !ok {
			return errors.New("Repository is not configured")
		}
		requesterID, err := validateAdminRequesterInRepository(ctx, owner, configured, login)
		if err != nil {
			return err
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				repo := &cfg.Repositories[i]
				if !strings.EqualFold(repo.Name, repoName) {
					continue
				}
				if !repo.MatchesIdentity(configured.ID, configured.OwnerID) {
					return errors.New("Repository identity changed; reload and try again")
				}
				for _, existing := range repo.AllowedActors {
					if strings.EqualFold(existing, login) {
						return errors.New("Requester is already allowed")
					}
				}
				repo.AllowedActors = append(repo.AllowedActors, login)
				repo.DenyAllActors = false
				if repo.AllowedActorIDs == nil {
					repo.AllowedActorIDs = make(map[string]int64)
				}
				repo.AllowedActorIDs[login] = requesterID
				return nil
			}
			return errors.New("Repository is no longer configured")
		})
	}))
	mux.Handle("POST /admin/users/remove", adminConfigPost("/admin/users", []string{"repository", "login"}, "", func(_ context.Context, values url.Values) error {
		repoName, login := strings.ToLower(values.Get("repository")), values.Get("login")
		if !workspace.ValidRepository(repoName) || login == "" {
			return errors.New("Enter a configured repository and requester")
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				repo := &cfg.Repositories[i]
				if !strings.EqualFold(repo.Name, repoName) {
					continue
				}
				if len(repo.AllowedActors) == 0 {
					return errors.New("There is no named requester to remove")
				}
				for j, existing := range repo.AllowedActors {
					if strings.EqualFold(existing, login) {
						repo.AllowedActors = slices.Delete(repo.AllowedActors, j, j+1)
						delete(repo.AllowedActorIDs, strings.ToLower(existing))
						if len(repo.AllowedActors) == 0 {
							repo.AllowedActors, repo.AllowedActorIDs = nil, nil
							repo.DenyAllActors = true
						}
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
					repo.DenyAllActors = false
					return nil
				}
			}
			return errors.New("Repository is not configured")
		})
	}))
	mux.Handle("POST /admin/users/deny-all", adminConfigPost("/admin/users", []string{"repository"}, "", func(_ context.Context, values url.Values) error {
		repoName := strings.ToLower(values.Get("repository"))
		if !workspace.ValidRepository(repoName) {
			return errors.New("Enter a configured repository")
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				repo := &cfg.Repositories[i]
				if strings.EqualFold(repo.Name, repoName) {
					repo.AllowedActors, repo.AllowedActorIDs = nil, nil
					repo.DenyAllActors = true
					return nil
				}
			}
			return errors.New("Repository is not configured")
		})
	}))
	mux.Handle("POST /admin/repositories/add", adminConfigPost("/admin/repositories", []string{"url"}, "actions", func(ctx context.Context, values url.Values) error {
		actions := values["actions"]
		current := live.snapshot().Config
		if err := validateAdminActions(current, actions, nil); err != nil {
			return err
		}
		if owner == nil || owner.GitHub == nil {
			return errors.New("GitHub client is unavailable")
		}
		repo, err := resolveAdminRepository(ctx, owner.GitHub, current, values.Get("url"), actions)
		if err != nil {
			return err
		}
		return live.save(func(cfg *config.Config) error {
			if _, exists := cfg.Repository(repo.Name); exists {
				return errors.New("Repository is already configured")
			}
			if err := validateAdminActions(*cfg, actions, nil); err != nil {
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
		repo, ok := current.Repository(name)
		if !ok {
			return errors.New("Repository is not configured")
		}
		if err := validateAdminActions(current, actions, repo.Actions); err != nil {
			return err
		}
		originalActions := slices.Clone(repo.Actions)
		for _, action := range actions {
			if slices.Contains(repo.Actions, action) {
				continue
			}
			if owner == nil || owner.GitHub == nil {
				return errors.New("GitHub client is unavailable")
			}
			repo.Actions = actions
			probe := current
			probe.Repositories = []config.Repository{repo}
			if err := checkRepositories(ctx, owner.GitHub, probe, log.New(io.Discard, "", 0)); err != nil {
				return fmt.Errorf("Repository is not ready: %w", err)
			}
			break
		}
		return live.save(func(cfg *config.Config) error {
			for i := range cfg.Repositories {
				if strings.EqualFold(cfg.Repositories[i].Name, name) {
					if !cfg.Repositories[i].MatchesIdentity(repo.ID, repo.OwnerID) {
						return errors.New("Repository identity changed; reload and try again")
					}
					if !slices.Equal(cfg.Repositories[i].Actions, originalActions) {
						return errors.New("Repository commands changed; reload and try again")
					}
					if err := validateAdminActions(*cfg, actions, cfg.Repositories[i].Actions); err != nil {
						return err
					}
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
		name, err := skills.Name(source)
		if err != nil {
			return fmt.Errorf("Invalid skill URL: %w", err)
		}
		source = strings.TrimSuffix(source, "/")
		skillMu.Lock()
		defer skillMu.Unlock()
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
		providers := []string{current.Main}
		if target == "always" && current.Adversarial != current.Main {
			providers = append(providers, current.Adversarial)
		}
		var missing []string
		for _, provider := range providers {
			if provider == "" {
				continue
			}
			_, installed, err := skills.InstalledPathFor(source, provider)
			if err != nil && !errors.Is(err, skills.ErrSourceMismatch) {
				return fmt.Errorf("Check installed skill: %w", err)
			}
			if installed {
				continue
			}
			if activeSource := adminActiveSkillSource(live.current(), provider, name); activeSource != "" {
				if activeSource == source {
					return errors.New("Skill is already configured but missing for a selected AI; install configured skills after active actions finish, then retry")
				}
				return errors.New("A skill with this name is still active from another source; wait for active actions to finish before replacing it")
			}
			missing = append(missing, provider)
		}
		if len(missing) == 2 {
			if err := skills.Install(ctx, []string{source}); err != nil {
				return fmt.Errorf("Install skill: %w", err)
			}
		} else if len(missing) == 1 {
			if err := skills.InstallFor(ctx, source, missing[0]); err != nil {
				return fmt.Errorf("Install skill: %w", err)
			}
		}
		return live.save(edit)
	}))
	mux.Handle("POST /admin/skills/remove", adminConfigPost("/admin/skills", []string{"target", "url"}, "", func(_ context.Context, values url.Values) error {
		target, source := values.Get("target"), values.Get("url")
		if len(source) > config.MaxSkillURLBytes {
			return errors.New("Skill URL is too long")
		}
		if _, err := skills.Name(source); err != nil {
			return fmt.Errorf("Invalid skill URL: %w", err)
		}
		source = strings.TrimSuffix(source, "/")
		skillMu.Lock()
		defer skillMu.Unlock()
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

func validateAdminRequester(ctx context.Context, live *adminLiveConfig, owner *action.Orchestrator, repoName, login string) (int64, error) {
	if !workspace.ValidRepository(repoName) {
		return 0, errors.New("Enter a configured repository and valid GitHub login")
	}
	repo, ok := live.snapshot().Config.Repository(repoName)
	if !ok {
		return 0, errors.New("Repository is not configured")
	}
	return validateAdminRequesterInRepository(ctx, owner, repo, login)
}

func validateAdminRequesterInRepository(ctx context.Context, owner *action.Orchestrator, repo config.Repository, login string) (int64, error) {
	if !config.ValidGitHubLogin(login) {
		return 0, errors.New("Enter a configured repository and valid GitHub login")
	}
	if owner == nil || owner.GitHub == nil {
		return 0, errors.New("GitHub client is unavailable")
	}
	permission, err := owner.GitHub.Permission(ctx, repo.Name, login)
	if err != nil {
		return 0, fmt.Errorf("Check requester permission: %w", err)
	}
	if permission.User.ID <= 0 || !strings.EqualFold(permission.User.Login, login) || !adminCanWrite(permission.Permission) {
		return 0, errors.New("Requester must have write, maintain, or admin permission on this repository")
	}
	details, err := owner.GitHub.Repository(ctx, repo.Name)
	if err != nil {
		return 0, fmt.Errorf("Check GitHub repository: %w", err)
	}
	if !repo.MatchesIdentity(details.ID, details.Owner.ID) {
		return 0, errors.New("GitHub repository ID or owner ID differs from configured identity")
	}
	return permission.User.ID, nil
}

func adminConfigPost(back string, fields []string, repeated string, action func(context.Context, url.Values) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid configuration form", http.StatusBadRequest)
			return
		}
		want := len(fields)
		if repeated != "" && len(r.PostForm[repeated]) != 0 {
			want++
		}
		if len(r.PostForm) != want {
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

func adminCanWrite(permission string) bool {
	return permission == "write" || permission == "maintain" || permission == "admin"
}

func validateAdminActions(cfg config.Config, actions, existing []string) error {
	if len(actions) == 0 || len(actions) > len(cfg.Commands) {
		return errors.New("Select at least one configured command")
	}
	selected := make(map[string]bool, len(actions))
	for _, name := range actions {
		mapping, exists := cfg.Commands[name]
		if !exists || selected[name] || (!mapping.SupportsProvider(cfg.Main) || !mapping.SupportedOnHost()) && !slices.Contains(existing, name) {
			return errors.New("Select distinct commands supported by the main AI and selected executor, or keep already enabled commands")
		}
		selected[name] = true
	}
	return nil
}

func adminActiveSkillSource(cfg config.Config, provider, name string) string {
	find := func(sources []string) string {
		for _, source := range sources {
			configured, _ := skills.Name(source) // Validated configuration.
			if strings.EqualFold(configured, name) {
				return source
			}
		}
		return ""
	}
	if provider == cfg.Main || provider == cfg.Adversarial {
		if source := find(cfg.AlwaysOnSkills); source != "" {
			return source
		}
	}
	if provider == cfg.Main {
		for _, command := range cfg.Commands {
			if source := find(command.Skills); source != "" {
				return source
			}
		}
	}
	return ""
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
	candidate, err := cloneConfig(live.snapshot().Config)
	if err != nil {
		return err
	}
	if err := edit(&candidate); err != nil {
		return err
	}
	return candidate.Validate(filepath.Dir(live.path))
}
