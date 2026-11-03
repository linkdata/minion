package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

func runHashMode() int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(os.Stderr, "-mode=hash requires an interactive terminal")
		return 2
	}
	fmt.Fprint(os.Stderr, "Admin password: ")
	password, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read admin password:", err)
		return 1
	}
	defer clear(password)
	hash, err := hashAdminPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Fprintln(os.Stdout, hash)
	return 0
}

func hashAdminPassword(password []byte) (string, error) {
	if len(password) < 1 || len(password) > 4096 || !utf8.Valid(password) {
		return "", errors.New("admin password must be 1 to 4096 UTF-8 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate admin password salt: %w", err)
	}
	tag := argon2.IDKey(password, salt, 2, 19456, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(tag)), nil
}
