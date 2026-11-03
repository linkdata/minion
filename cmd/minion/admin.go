package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/webserv"
)

// adminServer owns the dashboard listener and the JaWS processing loop.
type adminServer struct {
	listener  net.Listener
	server    *http.Server
	jaws      *jaws.Jaws
	cancel    context.CancelFunc
	serveDone chan struct{}
	jawsDone  chan struct{}
	serveErr  error // read only after serveDone closes
}

func startAdminServer(ctx context.Context, cfg config.AdminConfig, onFailure func()) (*adminServer, error) {
	service := &webserv.Config{Address: cfg.Listen}
	var fallbackTLS *tls.Config
	if cfg.TLSCertFile != "" {
		certDir := filepath.Dir(cfg.TLSCertFile)
		relKey, err := filepath.Rel(certDir, cfg.TLSKeyFile)
		// webserv expands CertDir and cleans paths; Windows volumes may differ.
		if err != nil || strings.Contains(certDir, "$") ||
			filepath.Clean(cfg.TLSCertFile) != cfg.TLSCertFile || filepath.Clean(cfg.TLSKeyFile) != cfg.TLSKeyFile {
			certificate, err := loadTLSCertificate("admin", cfg.TLSCertFile, cfg.TLSKeyFile)
			if err != nil {
				return nil, err
			}
			fallbackTLS = &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{certificate},
				// A manually wrapped listener cannot follow net/http's HTTP/2 policy.
				NextProtos: []string{"http/1.1"},
			}
		} else {
			service.CertDir = certDir
			service.FullchainPem = filepath.Base(cfg.TLSCertFile)
			service.PrivkeyPem = relKey
		}
	}
	listener, err := service.Listen()
	if err != nil {
		return nil, fmt.Errorf("prepare listener: %w", err)
	}
	if fallbackTLS != nil {
		listener = tls.NewListener(listener, fallbackTLS)
	}
	jw, err := jaws.New()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("initialize admin dashboard: %w", err)
	}
	jw.Logger = slog.Default()
	server := &http.Server{
		Handler:                      newAdminHandler(jw, cfg),
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  30 * time.Second,
		IdleTimeout:                  time.Minute,
		MaxHeaderBytes:               128 << 10,
		DisableGeneralOptionsHandler: true,
	}
	serveCtx, cancel := context.WithCancel(ctx)
	a := &adminServer{listener: listener, server: server, jaws: jw, cancel: cancel, serveDone: make(chan struct{}), jawsDone: make(chan struct{})}
	go func() {
		defer close(a.jawsDone)
		jw.Serve()
	}()
	go func() {
		// The parent also handles signals; ServeWith treats a signal-only stop as clean.
		if err := service.ServeWith(serveCtx, server, listener); err != nil && serveCtx.Err() == nil {
			a.serveErr = err
			if onFailure != nil {
				onFailure()
			}
		}
		close(a.serveDone)
	}()
	return a, nil
}

// adminGuard is the single entry gate for all dashboard routes.
func adminGuard(next http.Handler, allowed func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// JaWS's year-long HSTS policy includes subdomains and could affect other services.
		w.Header().Del("Strict-Transport-Security")
		if allowed == nil || !allowed(r) {
			closeUnreadAdminBody(w, r)
			controller := http.NewResponseController(w)
			_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Go's HTTP/1 server otherwise waits to drain an unread request body before
// writing the response, even when a handler has already rejected the request.
func closeUnreadAdminBody(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 && r.ContentLength != 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	}
}

func (a *adminServer) Close() error {
	a.jaws.Close()
	a.cancel()
	<-a.serveDone
	// Force-close connections left after webserv's graceful shutdown deadline.
	err := a.server.Close()
	<-a.jawsDone
	return errors.Join(err, a.serveErr)
}
