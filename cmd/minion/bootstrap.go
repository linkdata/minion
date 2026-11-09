package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion"
	"github.com/linkdata/minion/internal/config"
	"golang.org/x/term"
)

// createInitialConfig writes the embedded template only when no config exists.
func createInitialConfig(path string, logger *log.Logger) (created bool, resultErr error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	content := minion.DefaultConfig()
	if strings.Count(content, config.AdminPasswordHashPlaceholder) != 1 {
		return false, errors.New("embedded config has no unique admin password placeholder")
	}
	passwordSet := false
	if fd, ok := adminPasswordTerminal(); ok {
		reader := bufio.NewReader(os.Stdin)
		for {
			password, err := readAdminPasswordWithPrompt(fd, "Set admin password: ", reader)
			fmt.Fprintln(os.Stderr)
			if errors.Is(err, errInvalidAdminPassword) {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			if err != nil {
				logger.Printf("admin password setup skipped: %v", err)
				break
			}
			hash, err := hashAdminPassword(password)
			if errors.Is(err, errInvalidAdminPassword) {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			if err != nil {
				logger.Printf("admin password setup skipped: %v", err)
				break
			}
			content = strings.Replace(content, config.AdminPasswordHashPlaceholder, hash, 1)
			passwordSet = true
			break
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".minion-config-*")
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.Remove(f.Name())) }()
	_, err = io.WriteString(f, content)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return false, err
	}
	// Link publishes complete content without replacing a config created meanwhile.
	if err = os.Link(f.Name(), path); errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
		_ = dir.Sync() // Best effort; some filesystems do not support directory sync.
		_ = dir.Close()
	}
	logger.Printf("created config %s", path)
	if !passwordSet {
		logger.Print("admin password is not set; run minion -mode=hash and put the hash in the config")
	}
	return true, nil
}

func adminPasswordTerminal() (int, bool) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return 0, false
	}
	input, inputErr := os.Stdin.Stat()
	output, outputErr := os.Stderr.Stat()
	return fd, inputErr == nil && outputErr == nil && os.SameFile(input, output)
}
