package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

// adminServer owns the dashboard listener and the JaWS processing loop.
type adminServer struct {
	listener  net.Listener
	server    *http.Server
	jaws      *jaws.Jaws
	serveDone chan struct{}
	jawsDone  chan struct{}
	serveErr  error // read only after serveDone closes
}

func startAdminServer(cfg config.AdminConfig, onFailure func()) (*adminServer, error) {
	certificate, err := loadAdminCertificate(cfg)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen for admin dashboard: %w", err)
	}
	jw, err := jaws.New()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("initialize admin dashboard: %w", err)
	}
	jw.Logger = slog.Default()
	jw.MakeAuth = func(*jaws.Request) jaws.Auth { return deniedAdminAuth{} }
	mux := http.NewServeMux()
	mux.Handle("GET /jaws/", jw)
	server := &http.Server{
		Handler:           jw.SecureHeadersMiddleware(adminGuard(mux, nil)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	if certificate != nil {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*certificate}}
	}
	a := &adminServer{listener: listener, server: server, jaws: jw, serveDone: make(chan struct{}), jawsDone: make(chan struct{})}
	go func() {
		defer close(a.jawsDone)
		jw.Serve()
	}()
	go func() {
		var err error
		if server.TLSConfig != nil {
			err = server.ServeTLS(listener, "", "")
		} else {
			err = server.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.serveErr = err
			if onFailure != nil {
				onFailure()
			}
		}
		close(a.serveDone)
	}()
	return a, nil
}

func loadAdminCertificate(cfg config.AdminConfig) (*tls.Certificate, error) {
	if cfg.TLSCertFile == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load admin TLS certificate: %w", err)
	}
	return &certificate, nil
}

// adminGuard is the single entry gate for all dashboard routes. The authorizer
// is nil until admin sessions are implemented.
func adminGuard(next http.Handler, allowed func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed == nil || !allowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type deniedAdminAuth struct{}

func (deniedAdminAuth) Data() map[string]any { return nil }
func (deniedAdminAuth) Email() string        { return "" }
func (deniedAdminAuth) IsAdmin() bool        { return false }

func (a *adminServer) Close() error {
	a.jaws.Close()
	err := a.server.Close()
	<-a.serveDone
	<-a.jawsDone
	return errors.Join(err, a.serveErr)
}
