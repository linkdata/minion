package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/webserv"
)

// adminServer owns the dashboard listener and the JaWS processing loop.
type adminServer struct {
	listener      net.Listener
	entryURL      string
	signInEnabled bool
	server        *http.Server
	jaws          *jaws.Jaws
	cancel        context.CancelFunc
	serveDone     chan struct{}
	jawsDone      chan struct{}
	tickDone      chan struct{}
	serveErr      error // read only after serveDone closes
	preferences   *userLinks
}

func startAdminServer(ctx context.Context, cfg config.AdminConfig, owner *action.Orchestrator, statePath string, onFailure func()) (*adminServer, error) {
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
		// webserv saw the plaintext listener before the manual TLS wrap.
		service.ListenURL = fallbackTLSListenURL(service.ListenURL, listener.Addr().String(), &fallbackTLS.Certificates[0])
	}
	jw, err := jaws.New()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("initialize admin dashboard: %w", err)
	}
	jw.Logger = slog.Default()
	oauth, err := newOAuthAccess(jw, cfg, statePath, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		jw.Close()
		_ = listener.Close()
		return nil, err
	}
	handler, err := newAdminHandler(jw, cfg, owner, oauth)
	if err != nil {
		jw.Close()
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{
		Handler:                      handler,
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  30 * time.Second,
		IdleTimeout:                  time.Minute,
		MaxHeaderBytes:               128 << 10,
		DisableGeneralOptionsHandler: true,
	}
	serveCtx, cancel := context.WithCancel(ctx)
	signInEnabled := oauth != nil
	for _, account := range cfg.Administrators {
		if account.PasswordHash != config.AdminPasswordHashPlaceholder {
			signInEnabled = true
			break
		}
	}
	a := &adminServer{listener: listener, entryURL: service.ListenURL, signInEnabled: signInEnabled, server: server, jaws: jw, cancel: cancel, serveDone: make(chan struct{}), jawsDone: make(chan struct{}), tickDone: make(chan struct{})}
	if oauth != nil {
		a.preferences = oauth.links
	}
	go func() {
		defer close(a.tickDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-ticker.C:
				jw.Dirty(adminDashboardDirty)
			}
		}
	}()
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

func fallbackTLSListenURL(rawURL, address string, certificate *tls.Certificate) string {
	rawURL = strings.Replace(rawURL, "http://", "https://", 1)
	host, port, err := net.SplitHostPort(address)
	if err != nil || certificate.Leaf == nil || len(certificate.Leaf.DNSNames) == 0 {
		return rawURL
	}
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsLoopback() || ip.IsUnspecified()) {
		return rawURL
	}
	name := certificate.Leaf.DNSNames[0]
	if host, _, err := net.SplitHostPort(name); err == nil {
		name = host
	}
	if name != "" && !strings.Contains(name, "*") {
		return (&url.URL{Scheme: "https", Host: net.JoinHostPort(name, port)}).String()
	}
	return rawURL
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
			w.WriteHeader(http.StatusForbidden)
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
	a.cancel()
	<-a.tickDone
	a.jaws.Close()
	<-a.serveDone
	// Force-close connections left after webserv's graceful shutdown deadline.
	err := a.server.Close()
	<-a.jawsDone
	return errors.Join(err, a.serveErr)
}
