package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/claudecli"
	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "usage" {
		os.Exit(runUsage(os.Args[2:]))
	}
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.local.json", "path to JSON configuration")
	loop := flag.Bool("loop", false, "poll continuously; default is one cron-friendly pass")
	check := flag.Bool("check", false, "verify AI settings, authentication, and repository access, then exit")
	installSkills := flag.Bool("install-skills", false, "download configured skills for Codex and Claude, then exit")
	flag.Parse()
	logger := log.New(os.Stderr, "minion: ", log.LstdFlags|log.LUTC)
	if flag.NArg() != 0 {
		logger.Printf("unexpected positional arguments; use flags")
		return 2
	}
	loadedPath, err := filepath.Abs(*configPath)
	if err != nil {
		logger.Printf("config: %v", err)
		return 2
	}
	cfg, err := config.Load(loadedPath)
	if err != nil {
		logger.Printf("config: %v", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Restore default signal handling so a later signal can terminate shutdown.
	context.AfterFunc(ctx, stop)
	defer stop()
	if *installSkills {
		if *loop || *check {
			logger.Printf("-install-skills cannot be combined with -loop or -check")
			return 2
		}
		var urls []string
		for _, mapping := range cfg.Commands {
			urls = append(urls, mapping.Skills...)
		}
		urls = append(urls, cfg.AlwaysOnSkills...)
		if err := skills.Install(ctx, urls); err != nil {
			logger.Printf("install skills: %v", err)
			return 1
		}
		logger.Printf("configured skills installed for Codex and Claude")
		return 0
	}
	logCommandSkills(cfg, logger)
	checkWorkRoot := func() error {
		if err := workroot.Check(cfg.WorkRoot, cfg.StatePath, cfg.WorkRootMaxBytes); err != nil {
			return err
		}
		return checkIsolation(cfg, loadedPath)
	}
	if err := checkWorkRoot(); err != nil {
		logger.Printf("work root: %v", err)
		return 1
	}
	var store *state.Store
	initial := time.Now().UTC()
	if cfg.StartAt != "" {
		initial, _ = time.Parse(time.RFC3339, cfg.StartAt)
	}
	store, err = state.Open(cfg.StatePath, initial)
	if errors.Is(err, state.ErrLocked) && !*loop && !*check {
		logger.Printf("another run is active; skipping this cron pass")
		return 0
	}
	if err != nil {
		logger.Printf("state: %v", err)
		return 1
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Printf("close state: %v", err)
		}
	}()
	if store.Data.UnconfirmedProcessJob != "" {
		logger.Printf("command scope release for %s is unconfirmed; operator recovery required", store.Data.UnconfirmedProcessJob)
		return 1
	}
	latchProcessError := func(stage string, runErr error) {
		if errors.Is(runErr, processgroup.ErrScopeUnconfirmed) {
			store.Data.UnconfirmedProcessJob = stage
			if saveErr := store.Save(); saveErr != nil {
				logger.Printf("save unconfirmed command scope release: %v", saveErr)
			}
		}
	}
	if err := processgroup.Configure(cfg.CommandMaxTasks, int64(cfg.CommandMaxMemoryMiB)<<20); err != nil {
		logger.Printf("process limits: %v", err)
		return 1
	}
	if err := processgroup.Check(); err != nil {
		if errors.Is(err, processgroup.ErrScopeUnconfirmed) {
			latchProcessError("process probe", err)
		}
		logger.Printf("process ownership: %v", err)
		return 1
	}
	if err := gitwork.Check(ctx, cfg.WorkRoot); err != nil {
		latchProcessError("Git preflight", err)
		logger.Printf("Git preflight: %v", err)
		return 1
	}
	if err := checkModels(ctx, &cfg, logger); err != nil {
		latchProcessError("AI preflight", err)
		logger.Printf("AI preflight: %v", err)
		return 1
	}
	token, err := githubToken(ctx)
	if err != nil {
		latchProcessError("GitHub authentication", err)
		logger.Printf("GitHub authentication: %v", err)
		return 1
	}
	client := github.New(token)
	user, err := client.AuthenticatedUser(ctx)
	if err != nil {
		logger.Printf("GitHub identity check: %v", err)
		return 1
	}
	if !strings.EqualFold(user.Login, cfg.BotLogin) {
		logger.Printf("GitHub identity is @%s; config requires @%s", user.Login, cfg.BotLogin)
		return 1
	}
	if *check {
		if err := checkRepositories(ctx, client, cfg, logger); err != nil {
			logger.Printf("preflight failed: %v", err)
			return 1
		}
		logger.Printf("preflight passed as @%s", user.Login)
		return 0
	}

	orchestrator := newOrchestrator(cfg, client, store, logger, checkWorkRoot)
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: loadedPath, GitHub: client, Handler: orchestrator, Logger: logger}
	for {
		next, err := watcher.RunOnce(ctx, store)
		if err != nil {
			if ctx.Err() != nil {
				logger.Printf("poll interrupted: %v", err)
			} else {
				logger.Printf("poll: %v", err)
			}
			if !*loop || errors.Is(err, processgroup.ErrUnconfirmed) || errors.Is(err, processgroup.ErrScopeUnconfirmed) {
				return 1
			}
		}
		if !*loop {
			return 0
		}
		wait := time.Until(next)
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
}

func newOrchestrator(cfg config.Config, client *github.Client, store *state.Store, logger *log.Logger, checkWorkRoot func() error) *action.Orchestrator {
	return &action.Orchestrator{
		WorkRoot:              cfg.WorkRoot,
		Logger:                logger,
		GitHub:                client,
		BotLogin:              cfg.BotLogin,
		Store:                 store,
		Usage:                 &usage.Store{Dir: filepath.Join(filepath.Dir(cfg.StatePath), "usage")},
		CheckWorkRoot:         checkWorkRoot,
		Timeout:               time.Duration(cfg.JobTimeoutMinutes) * time.Minute,
		MaxGitHubContextBytes: cfg.MaxGitHubContextBytes,
		Commands:              cfg.Commands,
		Main:                  cfg.Main,
		Adversarial:           cfg.Adversarial,
		CriticStopping:        cfg.CriticStopping,
		AlwaysOnSkills:        cfg.AlwaysOnSkills,
		CodexModel:            cfg.Codex.Model,
		CodexEffort:           cfg.Codex.ReasoningEffort,
		ClaudeModel:           cfg.Claude.Model,
		ClaudeEffort:          cfg.Claude.Effort,
		// Jobs override model, effort and skills; the runner keeps host credentials and sandbox grants.
		Runner: action.Runner{
			Codex:  config.CodexConfig{APIKey: cfg.Codex.APIKey},
			Claude: config.ClaudeConfig{APIKey: cfg.Claude.APIKey, AllowAllUnixSockets: cfg.Claude.AllowAllUnixSockets},
			Logger: logger,
		},
	}
}

func checkIsolation(cfg config.Config, configPath string) (resultErr error) {
	// Host admission retries unsafe placement; later job preparation has its own failure policy.
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%w: %w", workroot.ErrUnsafe, resultErr)
		}
	}()
	root, err := workspace.CanonicalPath(cfg.WorkRoot)
	if err != nil {
		return err
	}
	if err := workspace.CheckRoot(root); err != nil {
		return err
	}
	runner := action.Runner{}
	for _, provider := range []string{cfg.Main, cfg.Adversarial} {
		if provider == "" {
			continue
		}
		sources := slices.Clone(cfg.AlwaysOnSkills)
		if provider == cfg.Main {
			for _, mapping := range cfg.Commands {
				if mapping.SupportsProvider(provider) {
					sources = append(sources, mapping.Skills...)
				}
			}
		}
		// Missing required skills disable their command; admission needs only existing grants.
		if err := runner.ConfigureRole(action.RoleSettings{Provider: provider, AlwaysOnSkills: sources}); err != nil {
			return err
		}
	}
	if err := runner.CheckWorkRootIsolation(root, cfg.Main, cfg.Adversarial); err != nil {
		return err
	}
	private := []string{configPath}
	if cfg.StatePath != "" {
		private = append(private,
			cfg.StatePath,
			cfg.StatePath+".lock",
			filepath.Join(filepath.Dir(cfg.StatePath), "usage"),
		)
	}
	// Shared system reads can expose stored credentials even for an unused provider.
	codexHome, err := codexcli.HomePath()
	if err != nil {
		return err
	}
	private = append(private, codexHome, filepath.Join(codexHome, "auth.json"), filepath.Join(codexHome, "config.toml"))
	claudeHome, err := claudecli.HomePath()
	if err != nil {
		return err
	}
	private = append(private, claudeHome, filepath.Join(claudeHome, ".credentials.json"), filepath.Join(claudeHome, ".claude.json"), filepath.Join(claudeHome, "settings.json"))
	userHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	if !filepath.IsAbs(userHome) {
		return errors.New("resolve user home: HOME must be absolute")
	}
	private = append(private,
		filepath.Join(userHome, ".codex"),
		filepath.Join(userHome, ".codex", "auth.json"),
		filepath.Join(userHome, ".codex", "config.toml"),
		filepath.Join(userHome, ".claude"),
		filepath.Join(userHome, ".claude", ".credentials.json"),
		filepath.Join(userHome, ".claude", ".claude.json"),
		filepath.Join(userHome, ".claude", "settings.json"),
		filepath.Join(userHome, ".claude.json"),
		filepath.Join(userHome, ".git-credentials"),
		filepath.Join(userHome, ".gitconfig"),
		filepath.Join(userHome, ".netrc"),
		filepath.Join(userHome, ".ssh"),
		filepath.Join(userHome, ".config", "git", "credentials"),
		filepath.Join(userHome, ".config", "git", "config"),
		filepath.Join(userHome, ".config", "gh"),
		filepath.Join(userHome, ".config", "gh", "hosts.yml"),
	)
	configHome, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	private = append(private, filepath.Join(configHome, "git", "credentials"), filepath.Join(configHome, "git", "config"))
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG"} {
		if path := os.Getenv(key); path != "" {
			private = append(private, path)
		}
	}
	ghHome := os.Getenv("GH_CONFIG_DIR")
	if ghHome == "" {
		ghHome = filepath.Join(configHome, "gh")
	}
	// Match gh's filepath.Join cleaning before checking aliases of hosts.yml.
	ghHome = filepath.Clean(ghHome)
	private = append(private, ghHome, filepath.Join(ghHome, "hosts.yml"))
	return runner.CheckPrivatePaths(private, cfg.Main, cfg.Adversarial)
}

func logCommandSkills(cfg config.Config, logger *log.Logger) {
	for _, command := range cfg.CommandNames() {
		if !cfg.Commands[command].SupportsProvider(cfg.Main) {
			logger.Printf("command %s disabled: action %s is unsupported with %s as main", command, cfg.Commands[command].Action, cfg.Main)
			continue
		}
		var missing []string
		for _, source := range cfg.Commands[command].Skills {
			name, err := skills.Name(source)
			if err != nil {
				missing = append(missing, source)
				continue
			}
			_, installed, err := skills.InstalledPathFor(source, cfg.Main)
			if err != nil {
				missing = append(missing, fmt.Sprintf("%s (%v)", name, err))
			} else if !installed {
				missing = append(missing, name)
			}
		}
		if len(missing) != 0 {
			logger.Printf("command %s disabled: missing or invalid skills: %s", command, strings.Join(missing, ", "))
		} else {
			logger.Printf("command %s: skills installed", command)
		}
	}
}

func githubToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token, nil
	}
	binary, err := aicli.AbsoluteExecutable("gh")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := aicli.Probe(ctx, binary, "gh", []string{"auth", "token", "--hostname", "github.com"}, "", nil, "", "")
	if err != nil {
		return "", fmt.Errorf("gh auth token failed: %w", err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gh auth token returned an empty token")
	}
	return token, nil
}

func checkModels(ctx context.Context, cfg *config.Config, logger *log.Logger) (err error) {
	for _, provider := range []string{cfg.Main, cfg.Adversarial} {
		role := action.RoleSettings{Provider: provider}
		var label, effortName string
		switch provider {
		case "codex":
			if err = codexcli.CheckLogin(ctx, cfg.Codex); err == nil {
				if err = codexcli.ValidateInstructionBoundary(ctx); err == nil {
					cfg.Codex, err = codexcli.Validate(ctx, cfg.Codex)
				}
			}
			if err != nil {
				//lint:ignore ST1005 Codex is a proper noun.
				return fmt.Errorf("Codex: %w", err) //nolint:staticcheck // -- Codex is a proper noun.
			}
			role.Model, role.Effort = cfg.Codex.Model, cfg.Codex.ReasoningEffort
			label, effortName = "Codex", "reasoning_effort"
		case "claude":
			if cfg.Claude, err = claudecli.Validate(ctx, cfg.Claude); err != nil {
				//lint:ignore ST1005 Claude is a proper noun.
				return fmt.Errorf("Claude: %w", err) //nolint:staticcheck // -- Claude is a proper noun.
			}
			role.Model, role.Effort = cfg.Claude.Model, cfg.Claude.Effort
			label, effortName = "Claude", "effort"
		case "":
			continue
		default:
			return fmt.Errorf("unsupported AI provider %q", provider)
		}
		if err := role.Validate(); err != nil {
			return fmt.Errorf("%s settings: %w", provider, err)
		}
		logger.Printf("%s settings: model=%s %s=%s", label, role.Model, effortName, role.Effort)
	}
	logger.Printf("AI roles: main=%s adversarial=%s job_timeout_minutes=%d command_max_tasks=%d command_max_memory_mib=%d", cfg.Main, cfg.Adversarial, cfg.JobTimeoutMinutes, cfg.CommandMaxTasks, cfg.CommandMaxMemoryMiB)
	if cfg.Adversarial != "" {
		maxRounds, nonblockingAfterRound, consecutiveNoBlockerRounds := cfg.CriticStopping.Resolve()
		logger.Printf("critic policy: max_rounds=%d nonblocking_after_round=%d consecutive_no_blocker_rounds=%d", maxRounds, nonblockingAfterRound, consecutiveNoBlockerRounds)
	}
	return
}

func checkRepositories(ctx context.Context, client *github.Client, cfg config.Config, logger *log.Logger) error {
	var problems []error
	for _, repo := range cfg.Repositories {
		if _, _, err := client.ListRepositoryNotifications(ctx, repo.Name, time.Now().UTC()); err != nil {
			problems = append(problems, fmt.Errorf("%s: cannot read repository notifications: %w", repo.Name, err))
		}
		details, err := client.Repository(ctx, repo.Name)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", repo.Name, err))
			continue
		}
		if !repo.MatchesIdentity(details.ID, details.Owner.ID) {
			problems = append(problems, fmt.Errorf("%s: repository ID or owner ID differs from config", repo.Name))
			continue
		}
		if details.Archived {
			problems = append(problems, fmt.Errorf("%s is archived", repo.Name))
			continue
		}
		logger.Printf("repository %s: default branch %s", repo.Name, details.DefaultBranch)
		permission, err := client.Permission(ctx, repo.Name, cfg.BotLogin)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: cannot inspect collaborator permissions; grant @%s write access: %w", repo.Name, cfg.BotLogin, err))
			continue
		}
		if permission.Permission != "write" && permission.Permission != "admin" && permission.Permission != "maintain" {
			problems = append(problems, fmt.Errorf("%s: @%s needs write access for configured actions (currently %q)", repo.Name, cfg.BotLogin, permission.Permission))
		}
		needsFixFork := false
		for _, name := range repo.Actions {
			if cfg.Commands[name].Action == "fix" {
				needsFixFork = true
				break
			}
		}
		if needsFixFork {
			orchestrator := action.Orchestrator{GitHub: client, BotLogin: cfg.BotLogin}
			if err := orchestrator.CheckFixFork(ctx, repo); err != nil {
				problems = append(problems, fmt.Errorf("%s: fix fork preflight: %w", repo.Name, err))
			}
		}
	}
	return errors.Join(problems...)
}
