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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/action"
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
	"github.com/linkdata/webserv"
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
	return runWithGitCheckAt(checkGit, time.Now)
}

func runWithGitCheckAt(checkGit func(context.Context, string) error, now func() time.Time) int {
	configPath := flag.String("config", "config.local.json", "path to JSON configuration")
	mode := flag.String("mode", "orchestrate", "service mode: orchestrate, execute, remote-execute, or hash")
	oneshot := flag.Bool("oneshot", false, "run one polling pass, then exit; default is a continuous service")
	check := flag.Bool("check", false, "verify Git, GitHub, repositories, selected AI tools or remote TLS, and admin TLS files, then exit")
	installSkills := flag.Bool("install-skills", false, "install configured skills for Codex and Claude, then exit")
	address := flag.String("address", "", "webserv listen address for the admin UI")
	certDir := flag.String("certdir", "", "webserv certificate directory containing fullchain.pem and privkey.pem")
	webUser := flag.String("user", "", "webserv user to switch to before setup and listener binding")
	listenURL := flag.String("listenurl", "", "external URL for the admin UI")
	flag.Parse()
	logger := log.New(os.Stderr, "minion: ", log.LstdFlags|log.LUTC)
	webservFlagsUsed := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "address", "certdir", "user", "listenurl":
			webservFlagsUsed = true
		}
	})
	if webservFlagsUsed && *mode != "orchestrate" {
		logger.Print("webserv flags require orchestrate mode")
		return 2
	}
	if flag.NArg() != 0 {
		if *mode == "hash" {
			logger.Print("-mode=hash prompts for a password; do not pass it as an argument")
		} else {
			logger.Print("unexpected positional arguments; use flags")
		}
		return 2
	}
	switch *mode {
	case "hash":
		incompatible := false
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "oneshot", "check", "install-skills":
				incompatible = true
			}
		})
		if incompatible {
			logger.Print("-mode=hash cannot be combined with -oneshot, -check, or -install-skills")
			return 2
		}
		return runHashMode()
	case "orchestrate":
	case "execute":
		incompatible := false
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "config", "oneshot", "check", "install-skills":
				incompatible = true
			}
		})
		if incompatible {
			logger.Print("-mode=execute cannot be combined with -config, -oneshot, -check, or -install-skills")
			return 2
		}
		return runExecuteMode()
	case "remote-execute":
		incompatible := false
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "oneshot", "check", "install-skills":
				incompatible = true
			}
		})
		if incompatible {
			logger.Print("-mode=remote-execute cannot be combined with -oneshot, -check, or -install-skills")
			return 2
		}
		return runRemoteExecuteMode(*configPath, logger)
	default:
		logger.Printf("unsupported mode %q; use orchestrate, execute, remote-execute, or hash", *mode)
		return 2
	}
	if *oneshot && *check {
		logger.Print("-oneshot cannot be combined with -check")
		return 2
	}
	if *installSkills && (*oneshot || *check) {
		logger.Print("-install-skills cannot be combined with -oneshot or -check")
		return 2
	}
	loadedPath, err := filepath.Abs(*configPath)
	if err != nil {
		logger.Printf("config: %v", err)
		return 2
	}
	if err := webserv.BecomeUser(*webUser); err != nil {
		logger.Printf("switch webserv user: %v", err)
		return 2
	}
	if _, err := createInitialConfig(loadedPath, logger); err != nil {
		logger.Printf("create config: %v", err)
		return 2
	}
	cfg, err := config.LoadWithAdminRuntime(loadedPath, config.AdminConfig{
		WebservAddress: *address, WebservCertDir: *certDir, WebservListenURL: *listenURL,
	})
	if err != nil {
		logger.Printf("config: %v", err)
		return 2
	}
	if webservFlagsUsed && cfg.Admin == nil {
		logger.Print("webserv flags require an admin block in the config")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Restore default signal handling so a later signal can terminate shutdown.
	context.AfterFunc(ctx, stop)
	defer stop()
	if *installSkills {
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
	checkWorkRoot := func() error {
		return checkWorkRootPath(cfg.WorkRoot)
	}
	var store *state.Store
	initial := time.Now().UTC()
	if cfg.StartAt != "" {
		initial, _ = time.Parse(time.RFC3339, cfg.StartAt)
	}
	store, err = state.Open(cfg.StatePath, initial)
	if errors.Is(err, state.ErrLocked) && *oneshot {
		logger.Printf("another run is active; skipping this cron pass")
		return 0
	}
	if err != nil {
		logger.Printf("state: %v", err)
		return 1
	}
	pendingDurableWork := func() bool {
		if store.Data.Accepted != nil {
			return true
		}
		for _, write := range store.Data.PublicationWrites {
			if write.Status == state.PublicationWritePending {
				return true
			}
		}
		return false
	}
	if cfg.Admin != nil && !*check && !*oneshot && !pendingDurableWork() &&
		(cfg.BotLogin == "" || cfg.GitHub.Token == "" || len(cfg.Repositories) == 0) {
		if err := store.Close(); err != nil {
			logger.Printf("close state before setup: %v", err)
			return 1
		}
		return runSetupMode(ctx, *cfg.Admin, loadedPath, logger)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Printf("close state: %v", err)
		}
	}()
	logCommandSkills(cfg, logger)
	if runtime.GOOS == "linux" && cfg.Claude.AllowAllUnixSockets && (cfg.Main == "claude" || cfg.Adversarial == "claude") {
		logger.Print(claudeUnixSocketWarning)
	}
	if !*check {
		defer func() {
			if ctx.Err() != nil {
				return
			}
			pruner := &action.Orchestrator{WorkRoot: cfg.WorkRoot, Logger: logger}
			if err := pruner.PrunePrivateRecords(ctx, store, now().UTC()); err != nil {
				logger.Printf("private record retention: %s", monitor.InertLogText(err.Error()))
			}
		}()
	}
	var client *github.Client
	var user github.User
	authenticateGitHub := func() bool {
		if client != nil {
			return true
		}
		if err := cfg.RequireGitHubToken(); err != nil {
			logger.Printf("GitHub authentication: %v", err)
			return false
		}
		candidate := github.New(cfg.GitHub.Token)
		user, err = candidate.AuthenticatedUser(ctx)
		if err != nil {
			logger.Printf("GitHub identity check: %v", err)
			return false
		}
		if user.ID <= 0 {
			logger.Printf("GitHub identity check: authenticated account ID is missing")
			return false
		}
		if !strings.EqualFold(user.Login, cfg.BotLogin) {
			logger.Printf("GitHub identity is @%s; config requires @%s", user.Login, cfg.BotLogin)
			return false
		}
		client = candidate
		return true
	}
	recoverySetup := func() int {
		if cfg.Admin == nil || *check || *oneshot || ctx.Err() != nil || pendingDurableWork() {
			return 1
		}
		logger.Print("starting admin recovery UI; no jobs run until Minion restarts")
		return runSetupMode(ctx, *cfg.Admin, loadedPath, logger)
	}
	terminalRecovery := !*check && terminalRecoveryPendingAt(store, now())
	aiPreflightFailed := false
	preflight := func() bool {
		aiPreflightFailed = false
		if err := action.CheckNativeRolePair(cfg.Main, cfg.Adversarial); err != nil {
			aiPreflightFailed = true
			logger.Printf("AI preflight: %v", err)
			return false
		}
		if err := checkWorkRoot(); err != nil {
			logger.Printf("work root: %v", err)
			return false
		}
		if err := checkGit(ctx, cfg.WorkRoot); err != nil {
			logger.Printf("Git preflight: %v", err)
			return false
		}
		if cfg.Admin != nil && !*oneshot {
			if err := checkAdminServerConfig(*cfg.Admin); err != nil {
				logger.Printf("admin TLS preflight: %v", err)
				return false
			}
		}
		if remote := cfg.RemoteExecutor; remote != nil {
			if _, err := loadTLSCertificate("remote", remote.TLSCertFile, remote.TLSKeyFile); err != nil {
				logger.Printf("remote TLS preflight: %v", err)
				return false
			}
		} else {
			if err := checkModels(ctx, &cfg, logger); err != nil {
				aiPreflightFailed = true
				logger.Printf("AI preflight: %v", err)
				return false
			}
		}
		return true
	}
	failedPreflight := !terminalRecovery && !preflight()
	if failedPreflight && !*check {
		for _, write := range store.Data.PublicationWrites {
			if write.Status != state.PublicationWritePending {
				continue
			}
			if !authenticateGitHub() {
				return 1
			}
			watcher := &monitor.Monitor{Config: cfg, ConfigPath: loadedPath, GitHub: client, BotID: user.ID, Logger: logger}
			if err := watcher.ReconcilePublicationWrites(ctx, store); err != nil {
				logger.Printf("reconcile issued publication after preflight failure: %s", monitor.InertLogText(err.Error()))
				return 1
			}
			break
		}
	}
	if failedPreflight {
		if *check || !terminalRecoveryPendingAt(store, now()) {
			if aiPreflightFailed {
				return recoverySetup()
			}
			return 1
		}
		terminalRecovery = true
	}
	if !authenticateGitHub() {
		return recoverySetup()
	}
	if *check {
		if err := checkRepositories(ctx, client, cfg, logger); err != nil {
			logger.Printf("preflight failed: %v", err)
			return 1
		}
		logger.Printf("preflight passed as @%s", user.Login)
		return 0
	}

	watcher := &monitor.Monitor{Config: cfg, ConfigPath: loadedPath, GitHub: client, BotID: user.ID, Logger: logger}
	if terminalRecovery {
		recovery := newOrchestrator(cfg, client, store, logger, checkWorkRoot)
		recovery.BotID = user.ID
		bindPublicationValidation(recovery, watcher)
		watcher.Handler = recovery
		code := recoverAcceptedService(ctx, watcher, store, !*oneshot && !failedPreflight, nil, logger)
		if failedPreflight {
			return 1
		}
		if code != 0 || *oneshot || ctx.Err() != nil {
			return code
		}
		cfg = watcher.Config
		if !preflight() {
			if aiPreflightFailed {
				return recoverySetup()
			}
			return 1
		}
	}
	orchestrator := newOrchestrator(cfg, client, store, logger, checkWorkRoot)
	orchestrator.BotID = user.ID
	bindPublicationValidation(orchestrator, watcher)
	watcher.Config, watcher.Handler = cfg, orchestrator
	path := ""
	if cfg.RemoteExecutor == nil {
		path, err = os.Executable()
		if err != nil {
			logger.Printf("locate executable: %v", err)
			return 1
		}
	}
	return runOrchestrateMode(ctx, orchestrator, watcher, store, !*oneshot, logger, path, 2*time.Minute)
}

const monitorConfigChanged = 3

func runMonitorLoop(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, workerExited <-chan struct{}, logger *log.Logger, stopAfterAccepted bool) int {
	return runMonitorLoopWithConfigUpdates(ctx, watcher, store, loop, workerExited, logger, stopAfterAccepted, nil)
}

func runMonitorLoopWithConfigUpdates(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, workerExited <-chan struct{}, logger *log.Logger, stopAfterAccepted bool, configUpdates <-chan struct{}) int {
	pendingConfigUpdate := false
	for {
		if ctx.Err() != nil {
			return 0
		}
		select {
		case <-configUpdates:
			pendingConfigUpdate = true
			configUpdates = nil
		default:
		}
		if ctx.Err() != nil {
			return 0
		}
		if pendingConfigUpdate && store.Data.Accepted == nil {
			return monitorConfigChanged
		}
		watcher.ConfigUpdates = configUpdates
		next, err := watcher.RunOnce(ctx, store)
		workerUnavailable := workerExited != nil && ctx.Err() == nil &&
			(errors.Is(err, action.ErrLocalWorkerUnavailable) || errors.Is(err, action.ErrRemoteWorkerUnavailable))
		fatal := !loop || errors.Is(err, monitor.ErrAcceptanceState) || workerUnavailable
		if errors.Is(err, monitor.ErrConfigUpdatePending) && !fatal && store.Data.Accepted == nil {
			if ctx.Err() != nil {
				return 0
			}
			logger.Printf("poll before config update: %s", monitor.InertLogText(err.Error()))
			select {
			case <-configUpdates:
			default:
			}
			return monitorConfigChanged
		}
		if err != nil {
			if ctx.Err() != nil {
				logger.Printf("poll interrupted: %s", monitor.InertLogText(err.Error()))
			} else {
				logger.Printf("poll: %s", monitor.InertLogText(err.Error()))
			}
			if fatal {
				return 1
			}
		}
		if workerExited != nil {
			select {
			case <-workerExited:
				if ctx.Err() == nil {
					logger.Print("selected worker exited")
					return 1
				}
			default:
			}
		}
		if stopAfterAccepted && store.Data.Accepted == nil {
			if err != nil {
				return 1
			}
			return 0
		}
		if !loop {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-configUpdates:
			pendingConfigUpdate = true
			configUpdates = nil
		default:
		}
		if ctx.Err() != nil {
			return 0
		}
		if pendingConfigUpdate && store.Data.Accepted == nil {
			return monitorConfigChanged
		}
		wait := time.Until(next)
		if wait < time.Second {
			accepted := store.Data.Accepted
			if accepted == nil || !next.Before(accepted.Deadline) {
				wait = time.Second
			} else {
				push := store.Data.PublicationWrites[accepted.JobID+"-push"]
				reply := store.Data.PublicationWrites[accepted.JobID]
				activePush := push.Status == state.PublicationWritePending && push.AttemptID == accepted.AttemptID
				activeReply := accepted.ResultReady && reply.AttemptID == accepted.AttemptID &&
					(reply.Status == state.PublicationWritePending ||
						reply.Status == state.PublicationWriteObserved && !accepted.RetryAt.IsZero() && !next.After(accepted.RetryAt))
				activeSavedRetry := accepted.ResultReady && next.Equal(accepted.RetryAt) &&
					(accepted.Action != "fix" || push.Status != state.PublicationWritePending)
				if wait <= 0 {
					if !activeSavedRetry || next.IsZero() {
						wait = time.Second
					}
				} else if !activePush && !activeReply && !activeSavedRetry {
					wait = time.Second
				}
			}
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
			logger.Print("selected worker exited")
			return 1
		case <-configUpdates:
			timer.Stop()
			if ctx.Err() != nil {
				return 0
			}
			if store.Data.Accepted == nil {
				return monitorConfigChanged
			}
			pendingConfigUpdate = true
			configUpdates = nil
		case <-timer.C:
		}
	}
}

func waitForConfigRetry(ctx context.Context, updates, workerExited <-chan struct{}) int {
	select {
	case <-ctx.Done():
		return 0
	case <-workerExited:
		if ctx.Err() != nil {
			return 0
		}
		return 1
	case <-updates:
		if ctx.Err() != nil {
			return 0
		}
		return monitorConfigChanged
	}
}

func newOrchestrator(cfg config.Config, client *github.Client, store *state.Store, logger *log.Logger, checkWorkRoot func() error) *action.Orchestrator {
	codexAuth := config.CodexConfig{APIKey: cfg.Codex.APIKey}
	claudeKey := cfg.Claude.APIKey
	if cfg.RemoteExecutor == nil && (cfg.Main == "claude" || cfg.Adversarial == "claude") {
		claudeKey = claudeRunnerKey(claudeKey)
	}
	if codexAuth.APIKey == "" && cfg.RemoteExecutor == nil {
		codexAuth.NativeAPIKey = cfg.Codex.NativeAPIKey
		codexAuth.NativeAccessToken = cfg.Codex.NativeAccessToken
		if cfg.Main == "codex" || cfg.Adversarial == "codex" {
			codexAuth = codexcli.CaptureAmbientNativeCredentials(codexAuth)
		}
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
		InactivityTimeout:     time.Duration(cfg.InactivityTimeoutMinutes) * time.Minute,
		PublicationGrace:      time.Duration(cfg.PublicationGraceSeconds) * time.Second,
		MaxGitHubContextBytes: cfg.MaxGitHubContextBytes,
		Commands:              cfg.Commands,
		Main:                  cfg.Main,
		Adversarial:           cfg.Adversarial,
		CriticStopping:        cfg.CriticStopping,
		CleanCriticRounds:     cfg.RequiredCleanRounds,
		AIInstructions:        cfg.AIInstructions,
		AlwaysOnSkills:        cfg.AlwaysOnSkills,
		CodexModel:            cfg.Codex.Model,
		CodexEffort:           cfg.Codex.ReasoningEffort,
		ClaudeModel:           cfg.Claude.Model,
		ClaudeEffort:          cfg.Claude.Effort,
		MainModel:             cfg.RoleModelSettings(false).Model,
		MainEffort:            cfg.RoleModelSettings(false).Effort,
		CriticModel:           cfg.RoleModelSettings(true).Model,
		CriticEffort:          cfg.RoleModelSettings(true).Effort,
		// Jobs override model, effort and skills; the runner keeps host credentials.
		Runner: action.Runner{
			Codex:  codexAuth,
			Claude: config.ClaudeConfig{APIKey: claudeKey, AllowAllUnixSockets: cfg.Claude.AllowAllUnixSockets},
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
		if !cfg.Commands[command].SupportsProvider(cfg.Main) || !cfg.Commands[command].SupportedOnHost() {
			logger.Printf("command %s disabled: action %s is unsupported with %s as main or on this host", command, cfg.Commands[command].Action, cfg.Main)
			continue
		}
		if cfg.RemoteExecutor != nil {
			continue // The worker checks its own skill installation after assignment.
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

func checkModels(ctx context.Context, cfg *config.Config, logger *log.Logger) (err error) {
	requested := [2]config.RoleModelConfig{cfg.MainSettings, cfg.CriticSettings}
	base := *cfg
	for i, provider := range []string{cfg.Main, cfg.Adversarial} {
		role := action.RoleSettings{Provider: provider}
		var label, effortName string
		selected := base.RoleModelSettings(i == 1)
		switch provider {
		case "codex":
			settings := cfg.Codex
			settings.Model, settings.ReasoningEffort = selected.Model, selected.Effort
			if err = codexcli.CheckLogin(ctx, settings); err == nil {
				if err = codexcli.ValidateInstructionBoundary(ctx); err == nil {
					settings, err = codexcli.Validate(ctx, settings)
				}
			}
			if err != nil {
				//lint:ignore ST1005 Codex is a proper noun.
				return fmt.Errorf("Codex: %w", err) //nolint:staticcheck // -- Codex is a proper noun.
			}
			role.Model, role.Effort = settings.Model, settings.ReasoningEffort
			if requested[i] == (config.RoleModelConfig{}) {
				cfg.Codex.Model, cfg.Codex.ReasoningEffort = role.Model, role.Effort
			}
			label, effortName = "Codex", "reasoning_effort"
		case "claude":
			settings := cfg.Claude
			settings.Model, settings.Effort = selected.Model, selected.Effort
			if settings, err = claudecli.Validate(ctx, settings); err != nil {
				//lint:ignore ST1005 Claude is a proper noun.
				return fmt.Errorf("Claude: %w", err) //nolint:staticcheck // -- Claude is a proper noun.
			}
			role.Model, role.Effort = settings.Model, settings.Effort
			if requested[i] == (config.RoleModelConfig{}) {
				cfg.Claude.Model, cfg.Claude.Effort = role.Model, role.Effort
			}
			label, effortName = "Claude", "effort"
		case "":
			continue
		default:
			return fmt.Errorf("unsupported AI provider %q", provider)
		}
		if err := role.Validate(); err != nil {
			return fmt.Errorf("%s settings: %w", provider, err)
		}
		if i == 0 {
			cfg.MainSettings = config.RoleModelConfig{Model: role.Model, Effort: role.Effort}
		} else {
			cfg.CriticSettings = config.RoleModelConfig{Model: role.Model, Effort: role.Effort}
		}
		logger.Printf("%s %s settings: model=%s %s=%s", []string{"main", "critic"}[i], label, role.Model, effortName, role.Effort)
		for _, choice := range cfg.UserChoices(i == 1) {
			if choice.Model == role.Model && choice.Effort == role.Effort {
				continue
			}
			switch provider {
			case "codex":
				settings := cfg.Codex
				settings.Model, settings.ReasoningEffort = choice.Model, choice.Effort
				validated, validationErr := codexcli.Validate(ctx, settings)
				if validationErr != nil {
					return fmt.Errorf("%s approved Codex choice %s/%s is unsupported: %v", []string{"main", "critic"}[i], choice.Model, choice.Effort, validationErr)
				}
				if validated.Model != choice.Model || validated.ReasoningEffort != choice.Effort {
					return fmt.Errorf("%s approved Codex choice %s/%s was not resolved exactly", []string{"main", "critic"}[i], choice.Model, choice.Effort)
				}
			case "claude":
				settings := cfg.Claude
				settings.Model, settings.Effort = choice.Model, choice.Effort
				validated, validationErr := claudecli.Validate(ctx, settings)
				if validationErr != nil {
					return fmt.Errorf("%s approved Claude choice %s/%s is unsupported: %v", []string{"main", "critic"}[i], choice.Model, choice.Effort, validationErr)
				}
				if validated.Model != choice.Model || validated.Effort != choice.Effort {
					return fmt.Errorf("%s approved Claude choice %s/%s was not resolved exactly", []string{"main", "critic"}[i], choice.Model, choice.Effort)
				}
			}
		}
	}
	logger.Printf("AI roles: main=%s adversarial=%s inactivity_timeout_minutes=%d max_runtime_minutes=%d publication_grace_seconds=%d", cfg.Main, cfg.Adversarial, cfg.InactivityTimeoutMinutes, cfg.MaxRuntimeMinutes, cfg.PublicationGraceSeconds)
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
		if details.DefaultBranch == "" {
			problems = append(problems, fmt.Errorf("%s has no default branch", repo.Name))
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
