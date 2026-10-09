// Package command extracts a single bot directive from a GitHub Markdown body.
package command

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

// Action is one of the operations the bot recognizes.
type Action string

const (
	Review  Action = "review"
	Audit   Action = "audit"
	Fix     Action = "fix"
	Release Action = "release"
)

var (
	// ErrNoCommand means the body contains no actionable directive.
	ErrNoCommand = errors.New("no bot command")
	// ErrAmbiguous means the body contains more than one actionable directive.
	ErrAmbiguous = errors.New("multiple bot commands")
	// ErrInvalidLogin means the configured GitHub login is malformed.
	ErrInvalidLogin = errors.New("invalid bot login")
)

// Command is a directive on its own Markdown line. Key is a deterministic
// SHA-256 digest of the action and argument; callers should include the GitHub
// item's identity when using it as an idempotency key.
type Command struct {
	Key    string
	Action Action
	Args   string
}

// Parse finds exactly one directive for @linkdataminion in markdown.
func Parse(markdown string) (Command, error) {
	return ParseForLogin(markdown, "linkdataminion")
}

// ParseForLogin finds exactly one directive for login.
//
// It accepts these full-line forms (ASCII case-insensitively):
//
//	@<login> review
//	@<login> audit
//	@<login> fix [text]
//	@<login> release <tag>
//
// It ignores quoted lines, indented code, fenced code, HTML comments, prose,
// unknown verbs, and malformed directives. Multiple valid directives fail
// closed with [ErrAmbiguous], even when they are identical.
func ParseForLogin(markdown, login string) (Command, error) {
	if !validLogin(login) {
		return Command{}, ErrInvalidLogin
	}
	handle := "@" + login
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")

	var found Command
	var hasCommand bool
	var fence fenceState
	var inComment bool

	for _, line := range strings.Split(markdown, "\n") {
		if fence.marker != 0 {
			if fence.closes(line) {
				fence = fenceState{}
			}
			continue
		}

		// A comment can begin in prose on the preceding line. Skip an entire
		// line containing comment markup so invisible text cannot become a
		// command after either delimiter.
		if inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				continue
			}
			inComment = false
			line = line[end+3:]
		}
		if isQuoted(line) || isIndentedCode(line) {
			continue
		}
		if opening, ok := openFence(line); ok {
			fence = opening
			continue
		}
		if start := strings.Index(line, "<!--"); start >= 0 {
			if !strings.Contains(line[start+4:], "-->") {
				inComment = true
			}
			continue
		}

		candidate, ok := parseLine(line, handle)
		if !ok {
			continue
		}
		if hasCommand {
			return Command{}, ErrAmbiguous
		}
		found, hasCommand = candidate, true
	}

	if !hasCommand {
		return Command{}, ErrNoCommand
	}
	return found, nil
}

func parseLine(line, handle string) (Command, bool) {
	line = strings.Trim(line, " \t")
	if len(line) <= len(handle) || !equalASCIIFold(line[:len(handle)], handle) {
		return Command{}, false
	}
	if line[len(handle)] != ' ' && line[len(handle)] != '\t' {
		return Command{}, false
	}

	verb, args := splitFirst(strings.Trim(line[len(handle):], " \t"))
	args = strings.Trim(args, " \t")
	var action Action
	switch {
	case equalASCIIFold(verb, string(Review)) && args == "":
		action = Review
	case equalASCIIFold(verb, string(Audit)) && args == "":
		action = Audit
	case equalASCIIFold(verb, string(Fix)):
		action = Fix
	case equalASCIIFold(verb, string(Release)) && args != "" && strings.IndexFunc(args, unicode.IsSpace) < 0:
		action = Release
	default:
		return Command{}, false
	}

	keyBytes := sha256.Sum256([]byte(string(action) + "\x00" + args))
	return Command{Key: hex.EncodeToString(keyBytes[:]), Action: action, Args: args}, true
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

func splitFirst(s string) (string, string) {
	for i, c := range s {
		if c == ' ' || c == '\t' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func isQuoted(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " \t"), ">")
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

type fenceState struct {
	marker byte
	length int
}

func openFence(line string) (fenceState, bool) {
	start := len(line) - len(strings.TrimLeft(line, " "))
	if start > 3 {
		return fenceState{}, false
	}
	line = line[start:]
	line = afterListMarker(line)
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return fenceState{}, false
	}
	marker := line[0]
	i := 0
	for i < len(line) && line[i] == marker {
		i++
	}
	if i < 3 {
		return fenceState{}, false
	}
	return fenceState{marker: marker, length: i}, true
}

func afterListMarker(line string) string {
	if len(line) >= 2 && strings.ContainsRune("-+*", rune(line[0])) && (line[1] == ' ' || line[1] == '\t') {
		return strings.TrimLeft(line[2:], " \t")
	}
	i := 0
	for i < len(line) && i < 9 && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && (line[i+1] == ' ' || line[i+1] == '\t') {
		return strings.TrimLeft(line[i+2:], " \t")
	}
	return line
}

func (f fenceState) closes(line string) bool {
	line = strings.TrimLeft(line, " \t")
	if len(line) < f.length {
		return false
	}
	i := 0
	for i < len(line) && line[i] == f.marker {
		i++
	}
	return i >= f.length && strings.TrimSpace(line[i:]) == ""
}
