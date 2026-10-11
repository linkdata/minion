package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workroot"
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
	check := flag.Bool("check", false, "verify Codex settings, authentication, and repository access, then exit")
	flag.Parse()
	logger := log.New(os.Stderr, "minion: ", log.LstdFlags|log.LUTC)
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Printf("config: %v", err)
		return 2
	}
	checkWorkRoot := func() error {
		return workroot.Check(cfg.WorkRoot, cfg.StatePath, cfg.WorkRootMaxBytes)
	}
	if err := checkWorkRoot(); err != nil {
		logger.Printf("work root: %v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := checkCodexCLI(ctx); err != nil {
		logger.Printf("Codex CLI: %v", err)
		return 1
	}
	cfg.Codex, err = codexcli.Validate(ctx, cfg.Codex)
	if err != nil {
		logger.Printf("Codex settings: %v", err)
		return 1
	}
	logger.Printf("Codex settings: model=%s reasoning_effort=%s skills=%d", cfg.Codex.Model, cfg.Codex.ReasoningEffort, len(cfg.Codex.Skills))
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

	initial := time.Now().UTC()
	if cfg.StartAt != "" {
		initial, _ = time.Parse(time.RFC3339, cfg.StartAt)
	}
	store, err := state.Open(cfg.StatePath, initial)
	if errors.Is(err, state.ErrLocked) && !*loop {
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

	executor := &action.Executor{
		GitHub:        client,
		BotLogin:      cfg.BotLogin,
		Store:         store,
		Usage:         &usage.Store{Dir: filepath.Join(filepath.Dir(cfg.StatePath), "usage")},
		WorkRoot:      cfg.WorkRoot,
		CheckWorkRoot: checkWorkRoot,
		Timeout:       time.Duration(cfg.JobTimeoutMinutes) * time.Minute,
		Codex:         cfg.Codex,
		Logger:        logger,
	}
	watcher := &monitor.Monitor{Config: cfg, ConfigPath: *configPath, GitHub: client, Executor: executor, Logger: logger}
	for {
		next, err := watcher.RunOnce(ctx, store)
		if err != nil {
			logger.Printf("poll: %v", err)
			if !*loop {
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

func githubToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token, nil
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token failed: %w", err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gh auth token returned an empty token")
	}
	return token, nil
}

func checkCodexCLI(ctx context.Context) error {
	if _, err := exec.LookPath("codex"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "codex", "login", "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex login status failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func checkRepositories(ctx context.Context, client *github.Client, cfg config.Config, logger *log.Logger) error {
	var problems []error
	if _, _, err := client.ListNotifications(ctx, time.Now().UTC()); err != nil {
		problems = append(problems, fmt.Errorf("cannot read account notifications: %w", err))
	}
	for _, repo := range cfg.Repositories {
		details, err := client.Repository(ctx, repo.Name)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", repo.Name, err))
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
	}
	return errors.Join(problems...)
}
