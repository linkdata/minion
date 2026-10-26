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
	return runWithGitCheck(gitwork.Check)
}

func runWithGitCheck(checkGit func(context.Context, string) error) int {
	configPath := flag.String("config", "config.local.json", "path to JSON configuration")
	mode := flag.String("mode", "in-process", "service mode: in-process, orchestrate, or execute")
	loop := flag.Bool("loop", false, "poll continuously; default is one cron-friendly pass")
	check := flag.Bool("check", false, "verify Git readiness, AI settings, authentication, and repository access, then exit")
	installSkills := flag.Bool("install-skills", false, "download configured skills for Codex and Claude, then exit")
	flag.Parse()
	logger := log.New(os.Stderr, "minion: ", log.LstdFlags|log.LUTC)
	if flag.NArg() != 0 {
		logger.Printf("unexpected positional arguments; use flags")
		return 2
	}
	switch *mode {
	case "in-process", "orchestrate":
	case "execute":
		incompatible := false
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "config", "loop", "check", "install-skills":
				incompatible = true
			}
		})
		if incompatible {
			logger.Print("-mode=execute cannot be combined with -config, -loop, -check, or -install-skills")
			return 2
		}
		return runExecuteMode()
	default:
		logger.Printf("unsupported mode %q; use in-process, orchestrate, or execute", *mode)
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
		return checkWorkRootPath(cfg.WorkRoot)
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
	if err := checkGit(ctx, cfg.WorkRoot); err != nil {
		logger.Printf("Git preflight: %v", err)
		return 1
	}
	if err := checkModels(ctx, &cfg, logger); err != nil {
		logger.Printf("AI preflight: %v", err)
		return 1
	}
	token, err := githubToken(ctx)
	if err != nil {
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
	if *mode == "orchestrate" {
		path, err := os.Executable()
		if err != nil {
			logger.Printf("locate executable: %v", err)
			return 1
		}
		return runOrchestrateMode(ctx, orchestrator, watcher, store, *loop, logger, path, 2*time.Minute)
	}
	return runMonitorLoop(ctx, watcher, store, *loop, nil, logger)
}

func runMonitorLoop(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, workerExited <-chan struct{}, logger *log.Logger) int {
	for {
		next, err := watcher.RunOnce(ctx, store)
		if err != nil {
			if ctx.Err() != nil {
				logger.Printf("poll interrupted: %s", monitor.InertLogText(err.Error()))
			} else {
				logger.Printf("poll: %s", monitor.InertLogText(err.Error()))
			}
			if !loop || workerExited != nil && ctx.Err() == nil && errors.Is(err, action.ErrLocalWorkerUnavailable) {
				return 1
			}
		}
		if workerExited != nil {
			select {
			case <-workerExited:
				if ctx.Err() == nil {
					logger.Print("selected local worker exited")
					return 1
				}
			default:
			}
		}
		if !loop {
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
		case <-workerExited:
			timer.Stop()
			if ctx.Err() != nil {
				return 0
			}
			logger.Print("selected local worker exited")
			return 1
		case <-timer.C:
		}
	}
}

func newOrchestrator(cfg config.Config, client *github.Client, store *state.Store, logger *log.Logger, checkWorkRoot func() error) *action.Orchestrator {
	codexAuth := config.CodexConfig{APIKey: cfg.Codex.APIKey}
	if codexAuth.APIKey == "" {
		codexAuth.NativeAPIKey = cfg.Codex.NativeAPIKey
		codexAuth.NativeAccessToken = cfg.Codex.NativeAccessToken
	}
	return &action.Orchestrator{
		WorkRoot:              cfg.WorkRoot,
		Logger:                logger,
		GitHub:                client,
		BotLogin:              cfg.BotLogin,
		Store:                 store,
		Usage:                 &usage.Store{Dir: filepath.Join(filepath.Dir(cfg.StatePath), "usage")},
		CheckWorkRoot:         checkWorkRoot,
		MaxRuntime:            time.Duration(cfg.MaxRuntimeMinutes) * time.Minute,
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
		// Jobs override model, effort and skills; the runner keeps host credentials.
		Runner: action.Runner{
			Codex:  codexAuth,
			Claude: config.ClaudeConfig{APIKey: cfg.Claude.APIKey},
			Logger: logger,
		},
	}
}

func checkWorkRootPath(path string) (resultErr error) {
	// Host admission retries unsafe placement; later job preparation has its own failure policy.
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%w: %w", workroot.ErrUnsafe, resultErr)
		}
	}()
	root, err := workspace.CanonicalPath(path)
	if err != nil {
		return err
	}
	return workspace.CheckRoot(root)
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
	logger.Printf("AI roles: main=%s adversarial=%s inactivity_timeout_minutes=%d max_runtime_minutes=%d", cfg.Main, cfg.Adversarial, cfg.InactivityTimeoutMinutes, cfg.MaxRuntimeMinutes)
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
