package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

func startAdminServer(ctx context.Context, cfg config.AdminConfig, owner *action.Orchestrator, statePath string, onFailure func(), liveConfigs ...*adminLiveConfig) (*adminServer, error) {
	return startAdminServerWithSetup(ctx, cfg, owner, statePath, onFailure, false, liveConfigs...)
}

func startAdminServerWithSetup(ctx context.Context, cfg config.AdminConfig, owner *action.Orchestrator, statePath string, onFailure func(), setup bool, liveConfigs ...*adminLiveConfig) (*adminServer, error) {
	if err := validateWebservAdminAddress(cfg); err != nil {
		return nil, err
	}
	service := adminWebservConfig(cfg)
	var fallbackTLS *tls.Config
	var certificate *tls.Certificate
	if cfg.WebservCertDir != "" {
		var err error
		certificate, _, err = webserv.LoadCert(cfg.WebservCertDir, "", "")
		if err != nil {
			return nil, err
		}
		if certificate == nil {
			return nil, errors.New("admin certificate directory expanded to an empty path")
		}
	}
	if cfg.WebservCertDir == "" && cfg.TLSCertFile != "" {
		var err error
		var loaded tls.Certificate
		loaded, err = loadTLSCertificate("admin", cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, err
		}
		certificate = &loaded
		certDir := filepath.Dir(cfg.TLSCertFile)
		relKey, err := filepath.Rel(certDir, cfg.TLSKeyFile)
		// webserv expands CertDir and cleans paths; Windows volumes may differ.
		if err != nil || strings.Contains(certDir, "$") ||
			filepath.Clean(cfg.TLSCertFile) != cfg.TLSCertFile || filepath.Clean(cfg.TLSKeyFile) != cfg.TLSKeyFile {
			fallbackTLS = &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*certificate},
				// A manually wrapped listener cannot follow net/http's HTTP/2 policy.
				NextProtos: []string{"http/1.1"},
			}
		} else {
			service.CertDir = certDir
			service.FullchainPem = filepath.Base(cfg.TLSCertFile)
			service.PrivkeyPem = relKey
		}
	}
	if err := checkAdminCertificate(certificate); err != nil {
		return nil, err
	}
	if service.ListenURL == "" {
		for _, client := range []*config.OAuthClient{cfg.GitHubOAuth, cfg.GitLabOAuth} {
			if client != nil {
				callback, _ := url.Parse(client.RedirectURL) // Validated before service startup.
				service.ListenURL = (&url.URL{Scheme: callback.Scheme, Host: callback.Host}).String()
				break
			}
		}
	}
	inferredFallbackURL := fallbackTLS != nil && service.ListenURL == ""
	listener, err := service.Listen()
	if err != nil {
		return nil, fmt.Errorf("prepare listener: %w", err)
	}
	if fallbackTLS != nil {
		listener = tls.NewListener(listener, fallbackTLS)
	}
	entryURL := service.ListenURL
	if inferredFallbackURL {
		entryURL = strings.Replace(entryURL, "http://", "https://", 1)
	}
	service.ListenURL = entryURL
	jw, err := jaws.New()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("initialize admin dashboard: %w", err)
	}
	jw.Logger = slog.Default()
	var oauth *oauthAccess
	if !setup {
		oauth, err = newOAuthAccess(jw, cfg, statePath, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			jw.Close()
			_ = listener.Close()
			return nil, err
		}
	}
	handler, err := newAdminHandlerWithSetupPath(jw, cfg, owner, oauth, setup, statePath, liveConfigs...)
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
		if account.PasswordHash != "" && account.PasswordHash != config.AdminPasswordHashPlaceholder {
			signInEnabled = true
			break
		}
	}
	a := &adminServer{listener: listener, entryURL: entryURL, signInEnabled: signInEnabled, server: server, jaws: jw, cancel: cancel, serveDone: make(chan struct{}), jawsDone: make(chan struct{}), tickDone: make(chan struct{})}
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

func adminWebservConfig(cfg config.AdminConfig) *webserv.Config {
	address := cfg.Listen
	if cfg.WebservAddress != "" {
		address = cfg.WebservAddress
	}
	if cfg.TLSCertFile != "" && cfg.WebservCertDir == "" {
		if _, _, err := net.SplitHostPort(address); err != nil {
			host := address
			if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
				if ip, err := netip.ParseAddr(host[1 : len(host)-1]); err == nil {
					host = ip.String()
				}
			}
			port := "8443"
			if os.Geteuid() == 0 {
				port = "443"
			}
			address = net.JoinHostPort(host, port)
		}
	}
	return &webserv.Config{Address: address, CertDir: cfg.WebservCertDir,
		DataDir: cfg.WebservDataDir, ListenURL: cfg.WebservListenURL}
}

func validateWebservAdminAddress(cfg config.AdminConfig) error {
	if err := cfg.ValidateWebservOptions(); err != nil {
		return err
	}
	if cfg.WebservCertDir != "" || cfg.TLSCertFile != "" {
		return nil
	}
	host := cfg.Listen
	if cfg.WebservAddress != "" {
		host = cfg.WebservAddress
	}
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Unmap().IsLoopback() {
		return errors.New("webserv address outside literal loopback requires a certificate directory or admin TLS files")
	}
	return nil
}

func checkAdminServerConfig(cfg config.AdminConfig) error {
	if err := validateWebservAdminAddress(cfg); err != nil {
		return err
	}
	if cfg.WebservCertDir != "" {
		certificate, _, err := webserv.LoadCert(cfg.WebservCertDir, "", "")
		if err != nil {
			return err
		}
		if certificate == nil {
			return errors.New("admin certificate directory expanded to an empty path")
		}
		return checkAdminCertificate(certificate)
	}
	if cfg.TLSCertFile != "" {
		certificate, err := loadTLSCertificate("admin", cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		return checkAdminCertificate(&certificate)
	}
	return nil
}

func checkAdminCertificate(certificate *tls.Certificate) error {
	if certificate == nil {
		return nil
	}
	if certificate.Leaf == nil {
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return fmt.Errorf("parse admin TLS certificate: %w", err)
		}
		certificate.Leaf = leaf
	}
	now := time.Now()
	if now.Before(certificate.Leaf.NotBefore) || now.After(certificate.Leaf.NotAfter) {
		return errors.New("admin TLS certificate is not currently valid")
	}
	usages := certificate.Leaf.ExtKeyUsage
	if (len(usages) != 0 || len(certificate.Leaf.UnknownExtKeyUsage) != 0) &&
		!slices.Contains(usages, x509.ExtKeyUsageServerAuth) && !slices.Contains(usages, x509.ExtKeyUsageAny) {
		return errors.New("admin TLS certificate does not permit server authentication")
	}
	if certificate.Leaf.KeyUsage != 0 && certificate.Leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return errors.New("admin TLS certificate cannot sign TLS handshakes")
	}
	return nil
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
