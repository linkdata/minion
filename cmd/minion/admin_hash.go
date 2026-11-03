package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/linkdata/minion/internal/config"
	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

func runHashMode() int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(os.Stderr, "-mode=hash requires an interactive terminal")
		return 2
	}
	password, err := readAdminPassword(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nread admin password:", err)
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
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer func() {
		if restoreErr := term.Restore(fd, state); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore terminal: %w", restoreErr)
		}
	}()
	console := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stderr}, "")
	return console.ReadPassword("Admin password: ")
}

func hashAdminPassword(password string) (string, error) {
	if !config.ValidAdminPassword(password) {
		return "", errors.New("admin password must be 1 to 64 Unicode characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate admin password salt: %w", err)
	}
	tag := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(tag)), nil
}
