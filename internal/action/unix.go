package action

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/linkdata/minion/internal/workspace"
)

// UnixExecutionBaseURL is the HTTP authority used by the fixed Unix client.
const UnixExecutionBaseURL = "http://minion"

const unixExecutionConnectionLimit = 16

type unixExecutionListener struct {
	*net.UnixListener
	slots chan struct{}
}

func (l *unixExecutionListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.UnixListener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &unixExecutionConn{Conn: connection, slots: l.slots}, nil
		default:
			_ = connection.Close()
		}
	}
}

type unixExecutionConn struct {
	net.Conn
	slots     chan struct{}
	closeOnce sync.Once
}

func (c *unixExecutionConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { <-c.slots })
	return err
}

// UnixEndpoint owns HTTP serving and its socket, but borrows the runtime directory.
type UnixEndpoint struct {
	executor   *HTTPExecutor
	listener   *unixExecutionListener
	server     *http.Server
	socketPath string
	directory  os.FileInfo
	socket     os.FileInfo
	serveDone  chan struct{}
	serveErr   error
	closeOnce  sync.Once
	closeErr   error
}

// ListenUnixExecution serves a dedicated executor in a caller-owned, uniquely
// named 0700 directory under trusted host control. The caller checks selected
// model grants against the directory/socket paths before worker readiness.
// Constructor errors close acquired resources and preserve unproved paths.
func ListenUnixExecution(runtimeDir string, executor *HTTPExecutor) (*UnixEndpoint, error) {
	if executor == nil {
		return nil, errors.New("Unix execution requires an HTTP executor")
	}
	directory, err := unixRuntimeDirectory(runtimeDir)
	if err != nil {
		return nil, err
	}
	socketPath := filepath.Join(runtimeDir, "execution.sock")
	if _, err := os.Lstat(socketPath); err == nil {
		return nil, fmt.Errorf("execution socket path %q is occupied", socketPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect execution socket %q: %w", socketPath, err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		// Bind may have created an entry before listen failed; do not adopt it.
		return nil, fmt.Errorf("listen on execution socket %q in %q; unproved entries are retained: %w", socketPath, runtimeDir, err)
	}
	listener.SetUnlinkOnClose(false)
	socket, err := unixSocketInfo(socketPath)
	if err == nil {
		var current os.FileInfo
		current, err = unixRuntimeDirectory(runtimeDir)
		if err == nil && !os.SameFile(directory, current) {
			err = fmt.Errorf("runtime directory %q identity changed", runtimeDir)
		}
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("prove execution socket %q; path retained: %w", socketPath, err), listener.Close())
	}
	e := &UnixEndpoint{
		executor:   executor,
		listener:   &unixExecutionListener{UnixListener: listener, slots: make(chan struct{}, unixExecutionConnectionLimit)},
		server:     &http.Server{Handler: executor, ReadHeaderTimeout: httpControlTimeout},
		socketPath: socketPath, directory: directory, socket: socket,
		serveDone: make(chan struct{}),
	}
	go func() {
		if err := e.server.Serve(e.listener); !errors.Is(err, http.ErrServerClosed) {
			e.serveErr = errors.Join(err, executor.Close())
		}
		close(e.serveDone)
	}()
	return e, nil
}

// SocketPath returns the protected pathname to pass to NewUnixExecutionClient.
func (e *UnixEndpoint) SocketPath() string { return e.socketPath }

// Close drains the executor while HTTP still serves paid receipts, then joins
// HTTP shutdown and removes only the matching socket. The caller retains the directory.
// When a worker can write the runtime path, first call HTTPExecutor.Close while
// HTTP serves, wait for the direct worker process to exit, then call Close.
func (e *UnixEndpoint) Close() error {
	return e.closeContext(context.Background())
}

func (e *UnixEndpoint) closeContext(ctx context.Context) error {
	e.closeOnce.Do(func() {
		coreErr := e.executor.Close()
		shutdownErr := e.server.Shutdown(ctx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, e.server.Close())
		}
		<-e.serveDone
		e.closeErr = errors.Join(coreErr, shutdownErr, e.serveErr, e.removeSocket())
	})
	return e.closeErr
}

func (e *UnixEndpoint) removeSocket() error {
	if err := e.checkIdentity(); err != nil {
		return err
	}
	if err := os.Remove(e.socketPath); err != nil {
		return fmt.Errorf("remove execution socket %q: %w", e.socketPath, err)
	}
	return nil
}

func (e *UnixEndpoint) checkIdentity() error {
	directory, err := unixRuntimeDirectory(filepath.Dir(e.socketPath))
	if err != nil {
		return fmt.Errorf("execution socket %q retained: %w", e.socketPath, err)
	}
	if !os.SameFile(e.directory, directory) {
		return fmt.Errorf("execution socket %q retained: runtime directory identity changed", e.socketPath)
	}
	socket, err := unixSocketInfo(e.socketPath)
	if err != nil {
		return err
	}
	if !os.SameFile(e.socket, socket) {
		return fmt.Errorf("execution socket %q retained: socket identity changed", e.socketPath)
	}
	return nil
}

// NewUnixExecutionClient dials only socketPath. Its caller closes idle
// connections after the final RunHTTP; request lifetimes remain with RunHTTP.
func NewUnixExecutionClient(socketPath string) (*http.Client, error) {
	if _, err := unixRuntimeDirectory(filepath.Dir(socketPath)); err != nil {
		return nil, err
	}
	if _, err := unixSocketInfo(socketPath); err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}}, nil
}

func unixRuntimeDirectory(runtimeDir string) (os.FileInfo, error) {
	canonical, err := workspace.CanonicalPath(runtimeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Unix runtime directory %q: %w", runtimeDir, err)
	}
	if !filepath.IsAbs(runtimeDir) || filepath.Clean(runtimeDir) != runtimeDir || canonical != runtimeDir {
		return nil, fmt.Errorf("Unix runtime directory %q must be a canonical absolute path", runtimeDir)
	}
	info, err := os.Lstat(runtimeDir)
	if err != nil {
		return nil, fmt.Errorf("inspect Unix runtime directory %q: %w", runtimeDir, err)
	}
	if info.Mode() != os.ModeDir|0700 {
		return nil, fmt.Errorf("Unix runtime directory %q must be a 0700 directory", runtimeDir)
	}
	return info, nil
}

func unixSocketInfo(socketPath string) (os.FileInfo, error) {
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return nil, fmt.Errorf("execution socket %q must be a clean absolute path", socketPath)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("inspect execution socket %q: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("execution socket %q is not a socket", socketPath)
	}
	return info, nil
}
