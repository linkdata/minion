package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/config"
	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

var errInvalidAdminPassword = errors.New("admin password must contain 1 to 64 Unicode code points in valid UTF-8")

func runHashMode() int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(os.Stderr, "-mode=hash requires an interactive terminal")
		return 2
	}
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, "-mode=hash requires a terminal on stderr for the prompt")
		return 2
	}
	stdinInfo, stdinErr := os.Stdin.Stat()
	stderrInfo, stderrErr := os.Stderr.Stat()
	if stdinErr != nil || stderrErr != nil || !os.SameFile(stdinInfo, stderrInfo) {
		fmt.Fprintln(os.Stderr, "-mode=hash requires stdin and stderr to use the same terminal")
		return 2
	}
	password, err := readAdminPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		if errors.Is(err, errInvalidAdminPassword) {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Fprintln(os.Stderr, "read admin password:", err)
		return 1
	}
	hash, err := hashAdminPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if _, err := fmt.Fprintln(os.Stdout, hash); err != nil {
		fmt.Fprintln(os.Stderr, "write admin password hash:", err)
		return 1
	}
	return 0
}

func readAdminPassword(fd int) (password string, err error) {
	return readAdminPasswordWithPrompt(fd, "Password to hash: ", bufio.NewReader(os.Stdin))
}

func readAdminPasswordWithPrompt(fd int, prompt string, reader *bufio.Reader) (password string, err error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
			select {
			case <-done:
				return
			default:
			}
			term.Restore(fd, state)
			os.Exit(1)
		case <-done:
		}
	}()
	defer func() {
		if restoreErr := term.Restore(fd, state); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore terminal: %w", restoreErr)
		}
		close(done)
	}()
	if _, err := fmt.Fprint(os.Stderr, prompt); err != nil {
		return "", err
	}
	// x/term.NewTerminal.ReadPassword stalls on a valid U+FFFD rune.
	runes := make([]rune, 0, 64)
	count := 0
	cancelled := false
	var inputErr error
	for {
		r, size, err := reader.ReadRune()
		if err != nil {
			return "", err
		}
		if r == utf8.RuneError && size == 1 {
			inputErr = errInvalidAdminPassword
			continue
		}
		switch r {
		case '\r', '\n':
			if cancelled {
				return "", io.EOF
			}
			if inputErr != nil {
				return "", inputErr
			}
			if count > 64 {
				return "", errInvalidAdminPassword
			}
			return string(runes), nil
		case '\x03', '\x04':
			if !cancelled {
				cancelled = true
				fmt.Fprint(os.Stderr, "\r\nPress Enter to cancel: ")
			}
		case '\b', '\x7f':
			if count > 0 {
				count--
				if count < len(runes) {
					runes = runes[:len(runes)-1]
				}
			}
		case '\x1b':
			inputErr = errors.New("unsupported terminal control key")
		default:
			count++
			if len(runes) < 64 {
				runes = append(runes, r)
			}
		}
	}
}

func hashAdminPassword(password string) (string, error) {
	if !config.ValidAdminPassword(password) {
		return "", errInvalidAdminPassword
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	tag := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(tag)), nil
}
