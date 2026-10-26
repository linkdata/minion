package action

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// LocalEndpointAccess is trusted startup input for one direct worker.
type LocalEndpointAccess struct {
	BaseURL     string `json:"base_url"`
	Certificate []byte `json:"certificate"`
	Token       string `json:"token"`
}

func (a LocalEndpointAccess) validate() error {
	u, err := url.Parse(a.BaseURL)
	if err != nil || u == nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("local worker endpoint must be a loopback HTTPS origin")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host != "127.0.0.1" {
		return errors.New("local worker endpoint must use IPv4 loopback")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return errors.New("local worker endpoint port is invalid")
	}
	if len(a.Certificate) == 0 || len(a.Certificate) > 16<<10 {
		return errors.New("local worker endpoint certificate is invalid")
	}
	if _, err := x509.ParseCertificate(a.Certificate); err != nil {
		return fmt.Errorf("local worker endpoint certificate: %w", err)
	}
	token, err := base64.RawURLEncoding.DecodeString(a.Token)
	if err != nil || len(token) != 32 || base64.RawURLEncoding.EncodeToString(token) != a.Token {
		return errors.New("local worker endpoint token is invalid")
	}
	return nil
}

// LocalEndpoint serves the existing bounded HTTP protocol on one private TLS origin.
type LocalEndpoint struct {
	executor  *HTTPExecutor
	listener  net.Listener
	server    *http.Server
	access    LocalEndpointAccess
	serveDone chan struct{}
	serveErr  error
	closeOnce sync.Once
	closeErr  error
}

// ListenLocalExecution creates a new origin and credentials for one direct worker.
func ListenLocalExecution(executor *HTTPExecutor) (*LocalEndpoint, error) {
	if executor == nil {
		return nil, errors.New("local execution requires an HTTP executor")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on local execution loopback: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return nil, errors.Join(errors.New("local execution listener is not IPv4 loopback"), listener.Close())
	}
	access := LocalEndpointAccess{
		BaseURL: "https://" + listener.Addr().String(), Certificate: certificate,
		Token: base64.RawURLEncoding.EncodeToString(token[:]),
	}
	if err := access.validate(); err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := r.Header.Values("Authorization")
			want := "Bearer " + access.Token
			if len(values) != 1 || len(values[0]) != len(want) || subtle.ConstantTimeCompare([]byte(values[0]), []byte(want)) != 1 {
				// Reject from headers: a foreign local process must not reach protocol decoding or hold body drain.
				w.Header().Set("Connection", "close")
				_ = http.NewResponseController(w).SetReadDeadline(time.Now())
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			executor.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: httpControlTimeout,
		MaxHeaderBytes:    httpControlBytes,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{certificate}, PrivateKey: privateKey}},
		},
	}
	e := &LocalEndpoint{
		executor: executor, listener: listener,
		server: server, access: access, serveDone: make(chan struct{}),
	}
	go func() {
		if err := server.ServeTLS(e.listener, "", ""); !errors.Is(err, http.ErrServerClosed) {
			e.serveErr = errors.Join(err, executor.Close())
		}
		close(e.serveDone)
	}()
	return e, nil
}

// Access returns the origin and credentials to send through the startup pipe.
func (e *LocalEndpoint) Access() LocalEndpointAccess {
	a := e.access
	a.Certificate = append([]byte(nil), a.Certificate...)
	return a
}

func (e *LocalEndpoint) checkAlive() error {
	select {
	case <-e.serveDone:
		return errors.Join(errors.New("local execution listener stopped"), e.serveErr)
	default:
		return nil
	}
}

// Close stops assignment first, then closes the listener after callbacks drain.
func (e *LocalEndpoint) Close() error {
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

type localAuthenticatedTransport struct {
	*http.Transport
	host  string
	token string
}

func (t *localAuthenticatedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != t.host || request.URL.User != nil {
		return nil, errors.New("local execution client refused another origin")
	}
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.Transport.RoundTrip(copy)
}

// NewLocalExecutionClient pins both the destination and the one-use server cert.
func NewLocalExecutionClient(access LocalEndpointAccess) (*http.Client, error) {
	if err := access.validate(); err != nil {
		return nil, err
	}
	certificate, _ := x509.ParseCertificate(access.Certificate)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	u, _ := url.Parse(access.BaseURL)
	transport := &localAuthenticatedTransport{
		Transport: &http.Transport{
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1"},
			DisableCompression: true,
		},
		host: u.Host, token: access.Token,
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
