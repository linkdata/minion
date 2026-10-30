package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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
}

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
	if err := action.RunHTTPLoop(ctx, client, cfg.BaseURL, cfg.ExecutorID, action.NewRemoteRunner(cfg.WorkRoot)); err != nil {
		logger.Printf("remote worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return 0
}
