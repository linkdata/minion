// Package command extracts a single bot directive from a GitHub Markdown body.
package command

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Action is one of the operations the bot recognizes.
type Action string

const (
	// Review selects the pull-request review action.
	Review Action = "review"
	// Fix selects the issue fix action.
	Fix Action = "fix"
	// Comment selects a read-only report posted to the requesting thread.
	Comment Action = "comment"
	// SecurityAudit selects the structured security-audit action.
	SecurityAudit Action = "security-audit"
)

var (
	// ErrNoCommand means the body contains no actionable directive.
	ErrNoCommand = errors.New("no bot command")
	// ErrAmbiguous means the body contains more than one actionable directive.
	ErrAmbiguous = errors.New("multiple bot commands")
	// ErrInvalidLogin means the configured GitHub login is malformed.
	ErrInvalidLogin = errors.New("invalid bot login")
)

// Command is a directive on its own Markdown line.
//
// Key is a deterministic SHA-256 digest of the name; callers should include
// the GitHub item's identity when using it as an idempotency key.
type Command struct {
	Key    string
	Name   string
	Action Action
}

// Parse finds exactly one directive for @linkdataminion in markdown.
func Parse(markdown string) (Command, error) {
	return ParseForLogin(markdown, "linkdataminion")
}

// ParseForLogin finds exactly one directive for login.
//
// It accepts these default standalone forms (ASCII case-insensitively):
//
//	@<login> review
//	@<login> audit
//	@<login> fix
//	@<login> security-audit
//
// The directive must be the first nonblank line. Later text is not a command
// argument; callers may use it as task context after authorization. A second
// configured directive returns [ErrAmbiguous].
func ParseForLogin(markdown, login string) (Command, error) {
	return ParseForLoginCommands(markdown, login, map[string]Action{
		"review": Review, "fix": Fix, "audit": Comment, "security-audit": SecurityAudit,
	})
}

// ParseForLoginCommands finds exactly one configured standalone directive for login.
//
// The directive must be the first nonblank line. Later text may be used as
// task context after authorization; a second configured directive returns
// [ErrAmbiguous]. Inline arguments are rejected.
func ParseForLoginCommands(markdown, login string, actions map[string]Action) (Command, error) {
	if !validLogin(login) {
		return Command{}, ErrInvalidLogin
	}
	handle := "@" + login
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")

	lines := strings.Split(markdown, "\n")
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first == len(lines) || isIndentedCode(lines[first]) {
		return Command{}, ErrNoCommand
	}
	found, ok := parseLine(lines[first], handle, actions)
	if !ok {
		return Command{}, ErrNoCommand
	}
	for _, line := range lines[first+1:] {
		if _, ok := parseLine(line, handle, actions); ok {
			return Command{}, ErrAmbiguous
		}
	}
	return found, nil
}

func parseLine(line, handle string, actions map[string]Action) (Command, bool) {
	line = strings.Trim(line, " \t")
	if len(line) <= len(handle) || !equalASCIIFold(line[:len(handle)], handle) {
		return Command{}, false
	}
	if line[len(handle)] != ' ' && line[len(handle)] != '\t' {
		return Command{}, false
	}

	verb := strings.Trim(line[len(handle):], " \t")
	if strings.ContainsAny(verb, " \t") {
		return Command{}, false
	}
	name := strings.ToLower(verb)
	action, ok := actions[name]
	if !ok || !equalASCIIFold(verb, name) {
		return Command{}, false
	}
	keyBytes := sha256.Sum256([]byte(name))
	return Command{Key: hex.EncodeToString(keyBytes[:]), Name: name, Action: action}, true
}

func validLogin(login string) bool {
	if len(login) >= 5 && equalASCIIFold(login[len(login)-5:], "[bot]") {
		login = login[:len(login)-5]
	}
	if login == "" || login[0] == '-' || login[len(login)-1] == '-' {
		return false
	}
	for i := 0; i < len(login); i++ {
		c := login[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func isIndentedCode(line string) bool {
	columns := 0
	for _, r := range line {
		switch r {
		case ' ':
			columns++
		case '\t':
			columns += 4 - columns%4
		default:
			return columns >= 4
		}
		if columns >= 4 {
			return true
		}
	}
	return false
}
