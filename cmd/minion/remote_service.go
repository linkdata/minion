package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func attachSelectedRemoteWorker(owner *action.Orchestrator, watcher *monitor.Monitor) (*action.RemoteEndpoint, error) {
	if owner == nil || watcher == nil || watcher.Config.RemoteExecutor == nil {
		return nil, errors.New("selected remote service requires an owner, monitor and remote executor config")
	}
	remote := watcher.Config.RemoteExecutor
	certificate, err := loadTLSCertificate("remote", remote.TLSCertFile, remote.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	commands, endpoint, err := owner.AttachRemoteWorker(watcher.Config, certificate)
	if err != nil {
		return nil, err
	}
	owner.ValidateAssignment = func(ctx context.Context, job monitor.Job) error {
		if owner.Store == nil {
			return fmt.Errorf("%w: accepted state store is unavailable", monitor.ErrAcceptanceState)
		}
		return watcher.RevalidateAssignment(ctx, owner.Store, job)
	}
	bindPublicationValidation(owner, watcher)
	watcher.Handler = owner
	watcher.SelectedExecutorID = remote.ExecutorID
	watcher.SelectedCommands = commands
	watcher.AfterAcceptedCleanup = endpoint.RequireFreshPull
	return endpoint, nil
}

func loadTLSCertificate(kind, certFile, keyFile string) (tls.Certificate, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load %s TLS certificate: %w", kind, err)
	}
	return certificate, nil
}

func runSelectedRemoteService(ctx context.Context, owner *action.Orchestrator, watcher *monitor.Monitor, store *state.Store, loop bool, logger *log.Logger, endpoint *action.RemoteEndpoint, readyTimeout time.Duration) (code int) {
	if endpoint == nil {
		logger.Print("selected remote worker is missing")
		return 1
	}
	defer func() {
		if err := endpoint.Close(); err != nil {
			logger.Printf("close selected remote worker: %s", monitor.InertLogText(err.Error()))
			code = 1
		}
	}()
	if err := watcher.ClearOrphanAcceptedResult(store); err != nil {
		logger.Printf("clear completed remote result: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	if store.Data.Accepted != nil {
		code = recoverAcceptedService(ctx, watcher, store, true, endpoint.Done(), logger)
		if code != 0 || !loop || ctx.Err() != nil {
			return code
		}
	}
	capacityCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	err := owner.WaitCapacity(capacityCtx)
	cancel()
	if err != nil {
		logger.Printf("wait for selected remote worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runMonitorLoop(ctx, watcher, store, loop, endpoint.Done(), logger, false)
}
