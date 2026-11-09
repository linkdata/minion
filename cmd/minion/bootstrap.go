package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/linkdata/minion"
	"github.com/linkdata/minion/internal/config"
	"golang.org/x/term"
)

// createInitialConfig writes the embedded template only when no config exists.
func createInitialConfig(path string, logger *log.Logger) (created bool, resultErr error) {
	return createInitialConfigWithPrompt(path, logger, true, nil)
}

func createInitialConfigWithPrompt(path string, logger *log.Logger, prompt bool, output *os.File) (created bool, resultErr error) {
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
	if prompt && output == nil {
		opened, err := adminPasswordTerminal()
		if err != nil {
			logger.Printf("admin password setup skipped: %v", err)
		} else if opened != nil {
			output = opened
			defer output.Close()
		}
	}
	if prompt && output != nil {
		reader := bufio.NewReader(os.Stdin)
		for {
			password, err := readAdminPasswordWithPrompt(int(os.Stdin.Fd()), "Set admin password: ", reader, output)
			fmt.Fprintln(output)
			if errors.Is(err, errInvalidAdminPassword) {
				fmt.Fprintln(output, err)
				continue
			}
			if err != nil {
				logger.Printf("admin password setup skipped: %v", err)
				break
			}
			hash, err := hashAdminPassword(password)
			if errors.Is(err, errInvalidAdminPassword) {
				fmt.Fprintln(output, err)
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
	if err := config.RemoveInheritedConfigACL(f); err != nil {
		return false, errors.Join(err, f.Close())
	}
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

func adminPasswordTerminal() (*os.File, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, nil
	}
	path := os.Stdin.Name()
	if runtime.GOOS == "windows" {
		path = "CONOUT$"
	}
	output, err := os.OpenFile(path, os.O_WRONLY, 0)
	return output, err
}
