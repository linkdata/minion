package action

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
)

// RemoteEndpoint serves the existing executor protocol over configured HTTPS.
type RemoteEndpoint struct {
	executor  *HTTPExecutor
	listener  *boundedExecutionListener
	server    *http.Server
	serveDone chan struct{}
	serveErr  error
	closeOnce sync.Once
	closeErr  error
}

// ListenRemoteExecution binds one configured executor with an operator-provided certificate and token.
func ListenRemoteExecution(executor *HTTPExecutor, address string, certificate tls.Certificate, token string) (*RemoteEndpoint, error) {
	if executor == nil || len(certificate.Certificate) == 0 || certificate.PrivateKey == nil || !validExecutionToken(token) {
		return nil, errors.New("remote execution requires an executor, TLS certificate and token")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	bounded := &boundedExecutionListener{Listener: listener, slots: make(chan struct{}, executionConnectionLimit)}
	server := &http.Server{
		Handler:           authenticatedExecutionHandler(executor, token),
		ReadHeaderTimeout: httpControlTimeout,
		MaxHeaderBytes:    httpControlBytes,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{certificate},
		},
	}
	endpoint := &RemoteEndpoint{executor: executor, listener: bounded, server: server, serveDone: make(chan struct{})}
	go func() {
		if err := server.ServeTLS(bounded, "", ""); !errors.Is(err, http.ErrServerClosed) {
			endpoint.serveErr = errors.Join(err, executor.Close())
		}
		close(endpoint.serveDone)
	}()
	return endpoint, nil
}

// Addr is the bound listener address.
func (e *RemoteEndpoint) Addr() net.Addr { return e.listener.Addr() }

// Done closes when the HTTPS listener stops serving.
func (e *RemoteEndpoint) Done() <-chan struct{} { return e.serveDone }

// Close stops assignment and joins the HTTPS server.
func (e *RemoteEndpoint) Close() error {
	e.closeOnce.Do(func() {
		coreErr := e.executor.Close()
		ctx, cancel := context.WithTimeout(context.Background(), httpControlTimeout)
		serverErr := e.server.Shutdown(ctx)
		cancel()
		if serverErr != nil {
			serverErr = errors.Join(serverErr, e.server.Close())
		}
		<-e.serveDone
		e.closeErr = errors.Join(coreErr, serverErr, e.serveErr)
	})
	return e.closeErr
}

// NewRemoteExecutionClient sends the token only to one HTTPS origin and uses normal system TLS trust.
func NewRemoteExecutionClient(origin, token string) (*http.Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || !validExecutionToken(token) {
		return nil, errors.New("remote execution requires a bare HTTPS origin and token")
	}
	transport := &authenticatedExecutionTransport{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: httpControlTimeout}).DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
			TLSHandshakeTimeout: httpControlTimeout,
			DisableCompression:  true,
		},
		host: u.Host, token: token,
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
