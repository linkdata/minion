package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

type remoteWorkerConfig struct {
	BaseURL    string `json:"base_url"`
	ExecutorID string `json:"executor_id"`
	Token      string `json:"token"`
	WorkRoot   string `json:"work_root"`
	Claude     struct {
		AllowAllUnixSockets bool `json:"allow_all_unix_sockets"`
	} `json:"claude"`
}

const claudeUnixSocketWarning = "Claude Unix-socket opt-in enabled by claude.allow_all_unix_sockets; sandboxed Claude commands can reach host service and AF_VSOCK VM host sockets"

func loadRemoteWorkerConfig(path string) (remoteWorkerConfig, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return remoteWorkerConfig{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return remoteWorkerConfig{}, err
	}
	defer f.Close()
	var cfg remoteWorkerConfig
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return remoteWorkerConfig{}, fmt.Errorf("decode remote worker config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return remoteWorkerConfig{}, errors.New("remote worker config has trailing data")
	}
	if !workspace.ValidIdentitySegment(cfg.ExecutorID) || cfg.WorkRoot == "" {
		return remoteWorkerConfig{}, errors.New("remote worker requires an executor ID and work root")
	}
	if !filepath.IsAbs(cfg.WorkRoot) {
		cfg.WorkRoot = filepath.Join(filepath.Dir(path), cfg.WorkRoot)
	}
	return cfg, nil
}

func runRemoteExecuteMode(path string, logger *log.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	defer stop()
	cfg, err := loadRemoteWorkerConfig(path)
	if err != nil {
		logger.Printf("remote worker config: %s", monitor.InertLogText(err.Error()))
		return 2
	}
	if runtime.GOOS == "linux" && cfg.Claude.AllowAllUnixSockets {
		logger.Print(claudeUnixSocketWarning)
	}
	if err := checkWorkRootPath(cfg.WorkRoot); err != nil {
		logger.Printf("remote worker work root: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	client, err := action.NewRemoteExecutionClient(cfg.BaseURL, cfg.Token)
	if err != nil {
		logger.Printf("remote worker endpoint: %s", monitor.InertLogText(err.Error()))
		return 2
	}
	defer client.CloseIdleConnections()
	if err := gitwork.Check(ctx, cfg.WorkRoot); err != nil {
		logger.Printf("remote worker Git preflight: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runRemoteWorkerPreflightAndLoop(ctx, cfg, client, logger)
}

func runRemoteWorkerPreflightAndLoop(ctx context.Context, cfg remoteWorkerConfig, client *http.Client, logger *log.Logger) int {
	if err := action.PreflightRemoteChoices(ctx, client, cfg.BaseURL, cfg.ExecutorID, cfg.Claude.AllowAllUnixSockets); err != nil {
		logger.Printf("remote worker: selection preflight failed: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runRemoteWorkerLoop(ctx, cfg, client, logger)
}

func runRemoteWorkerLoop(ctx context.Context, cfg remoteWorkerConfig, client *http.Client, logger *log.Logger) int {
	runner := action.NewRemoteRunner(cfg.WorkRoot)
	runner.Claude.AllowAllUnixSockets = cfg.Claude.AllowAllUnixSockets
	if err := action.RunHTTPLoop(ctx, client, cfg.BaseURL, cfg.ExecutorID, runner); err != nil {
		// Returned errors can contain private model text.
		logger.Print("remote worker: execution or protocol failure")
		return 1
	}
	return 0
}
