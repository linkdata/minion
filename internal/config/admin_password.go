package config

import (
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// AdminPasswordHashPlaceholder marks a disabled example account.
const AdminPasswordHashPlaceholder = "PASTE_GENERATED_HASH_HERE"

// AdminAccount is one configured dashboard administrator.
type AdminAccount struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

type adminPasswordHash struct {
	time    uint32
	memory  uint32
	threads uint8
	salt    []byte
	tag     []byte
}

// VerifyPassword checks a configured Argon2id password hash.
func (a AdminAccount) VerifyPassword(password string) bool {
	if !ValidAdminPassword(password) {
		return false
	}
	hash, ok := parseAdminPasswordHash(a.PasswordHash)
	if !ok {
		return false
	}
	actual := argon2.IDKey([]byte(password), hash.salt, hash.time, hash.memory, hash.threads, uint32(len(hash.tag)))
	return subtle.ConstantTimeCompare(actual, hash.tag) == 1
}

// ValidAdminPassword reports whether a password meets the admin sign-in limit.
func ValidAdminPassword(password string) bool {
	return password != "" && utf8.ValidString(password) && utf8.RuneCountInString(password) <= 64
}

func parseAdminPasswordHash(value string) (hash adminPasswordHash, ok bool) {
	if len(value) > 256 {
		return
	}
	parts := strings.Split(value, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 || !strings.HasPrefix(parameters[0], "m=") ||
		!strings.HasPrefix(parameters[1], "t=") || !strings.HasPrefix(parameters[2], "p=") {
		return
	}
	memory, err := strconv.ParseUint(parameters[0][2:], 10, 32)
	if err != nil || memory < 19456 || memory > 65536 {
		return
	}
	timeCost, err := strconv.ParseUint(parameters[1][2:], 10, 32)
	if err != nil || timeCost < 2 || timeCost > 4 {
		return
	}
	threads, err := strconv.ParseUint(parameters[2][2:], 10, 8)
	if err != nil || threads < 1 || threads > 4 {
		return
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 32 {
		return
	}
	tag, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(tag) != 32 {
		return
	}
	return adminPasswordHash{time: uint32(timeCost), memory: uint32(memory), threads: uint8(threads), salt: salt, tag: tag}, true
}
