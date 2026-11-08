package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/linkdata/minion"
	"github.com/linkdata/minion/internal/config"
	"golang.org/x/term"
)

// createInitialConfig writes the embedded template only when no config exists.
func createInitialConfig(path string, logger *log.Logger) (bool, error) {
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = io.WriteString(f, content)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return false, errors.Join(err, os.Remove(path))
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
