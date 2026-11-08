package main

import (
	"context"
	"log"

	"github.com/linkdata/minion/internal/config"
)

func runSetupMode(ctx context.Context, adminConfig config.AdminConfig, configPath string, logger *log.Logger) int {
	setupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server, err := startAdminServerWithSetup(setupCtx, adminConfig, nil, configPath, cancel, true)
	if err != nil {
		logger.Printf("setup UI: %v", err)
		return 1
	}
	entry := server.entryURL
	if entry == "" {
		entry = server.listener.Addr().String() + " (no certificate-valid URL could be inferred)"
	}
	logger.Printf("setup UI: %s (config %s)", entry, configPath)
	<-setupCtx.Done()
	if err := server.Close(); err != nil {
		logger.Printf("close setup UI: %v", err)
		return 1
	}
	if ctx.Err() == nil {
		return 1
	}
	return 0
}
