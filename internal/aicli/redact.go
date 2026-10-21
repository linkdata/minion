package aicli

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/bounded"
)

// RedactText replaces literal, Go-quoted and JSON-encoded secret content.
func RedactText(text, secret string) string {
	return redactText(text, secret, false)
}

func redactionMarker(secret string) string {
	const marker = "[redacted]"
	if secret != "" && strings.Contains(marker, secret) {
		// '#' cannot contain any nonempty substring of the normal marker.
		return "#"
	}
	return marker
}

// A replacement marker can join untouched text into another key occurrence.
func removeReformedSecrets(text string, forms [4]string) string {
	for _, form := range forms {
		if !strings.Contains(text, form) {
			continue
		}
		if marker := safeRedactionSeparator(forms); marker != 0 {
			for _, known := range forms {
				text = strings.ReplaceAll(text, known, string(marker))
			}
			return text
		}
		// No printable marker is safe; discard the diagnostic rather than expose a key.
		return ""
	}
	return text
}

func safeRedactionSeparator(forms [4]string) byte {
	for marker := byte('!'); marker <= '~'; marker++ {
		// The separator may be inserted inside a JSON string.
		if marker == '"' || marker == '\\' {
			continue
		}
		safe := true
		for _, known := range forms {
			if strings.IndexByte(known, marker) >= 0 {
				safe = false
				break
			}
		}
		if safe {
			return marker
		}
	}
	return 0
}

func redactText(text, secret string, truncated bool) string {
	if secret == "" {
		return text
	}
	marker := redactionMarker(secret)
	quoted := strconv.Quote(secret)
	encoded, _ := json.Marshal(secret)
	jsonContent := string(encoded[1 : len(encoded)-1])
	forms := [4]string{jsonContent, javascriptJSONContent(jsonContent), quoted[1 : len(quoted)-1], secret}
	if truncated {
		// Reserve NUL for masks; normalize literal NULs before matching.
		text = strings.ReplaceAll(text, "\x00", "?")
		forms[3] = strings.ReplaceAll(secret, "\x00", "?")
	}
	jsonDocument := !truncated && json.Valid([]byte(text))
	if jsonDocument && text == secret {
		// Callers also pass standalone scalar text before encoding it into JSON.
		switch strings.TrimSpace(text)[0] {
		case '{', '[', '"':
		default:
			return marker
		}
	}
	text = redactJSONEscapes(text, secret, marker, truncated, jsonDocument)
	if jsonDocument {
		// A marker can join adjacent content into another decoded key.
		if redactJSONEscapes(text, secret, marker, false, true) != text {
			separator := safeRedactionSeparator(forms)
			if separator == 0 {
				return ""
			}
			text = redactJSONEscapes(text, secret, string(separator), false, true)
		}
		// Raw matches left across JSON syntax cannot be replaced without corrupting it.
		for _, form := range forms {
			if strings.Contains(text, form) {
				return ""
			}
		}
		return text
	}
	var replacements [8]string
	longest := 0
	for i, form := range forms {
		mask := marker
		if truncated {
			mask = strings.Repeat("\x00", len(form))
		}
		replacements[2*i], replacements[2*i+1] = form, mask
		longest = max(longest, len(form))
	}
	if truncated {
		// Every Unicode scalar can occupy six bytes per UTF-16 code unit.
		encodedLength := 0
		for _, r := range secret {
			width := 6
			if r > 0xffff {
				width = 12
			}
			encodedLength += width
			if encodedLength > len(text) {
				encodedLength = len(text) + 1
				break
			}
		}
		longest = max(longest, encodedLength)
	}
	// Encoded forms precede literals; replacement text is not rescanned.
	text = strings.NewReplacer(replacements[:]...).Replace(text)
	if truncated {
		// Equal-length masks keep complete keys hidden while the cut removes partial ones.
		discard := longest - 1
		if discard >= len(text) {
			return ""
		}
		text = text[discard:]
		for strings.Contains(text, "\x00\x00") {
			text = strings.ReplaceAll(text, "\x00\x00", "\x00")
		}
		text = strings.ReplaceAll(text, "\x00", marker)
	}
	text = removeReformedSecrets(text, forms)
	// A replacement can join fragments into an escaped key unknown to the fixed forms.
	if strings.ContainsRune(text, '\\') && redactJSONEscapes(text, secret, marker, false, json.Valid([]byte(text))) != text {
		return ""
	}
	return text
}

// redactJSONEscapes compares decoded JSON string content with the secret while
// replacing original spans, so unrelated diagnostics keep their exact spelling.
func redactJSONEscapes(text, secret, marker string, truncated, jsonDocument bool) string {
	if secret == "" || (!jsonDocument && (len(secret) > len(text) || !strings.ContainsRune(text, '\\'))) {
		return text
	}
	needle := []byte(secret)
	prefix := make([]int, len(needle))
	for i, matched := 1, 0; i < len(needle); i++ {
		for matched > 0 && needle[i] != needle[matched] {
			matched = prefix[matched-1]
		}
		if needle[i] == needle[matched] {
			matched++
		}
		prefix[i] = matched
	}
	starts := make([]int, len(needle))
	var result strings.Builder
	matched, decoded, last := 0, 0, 0
	inString := false
	changed := false
	for i := 0; i < len(text); {
		if jsonDocument {
			if text[i] == '"' {
				// Raw quotes delimit JSON strings; escaped quotes are content.
				inString = !inString
				matched = 0
				i++
				continue
			}
			if !inString {
				i++
				continue
			}
		}
		begin := i
		var part [utf8.UTFMax]byte
		width := 0
		if text[i] == '\\' {
			if r, consumed := jsonEscape(text[i:]); consumed != 0 {
				width = utf8.EncodeRune(part[:], r)
				i += consumed
			}
		}
		if width == 0 {
			if jsonDocument && text[i] >= utf8.RuneSelf {
				// JSON decoding replaces malformed UTF-8 with the replacement rune.
				r, size := utf8.DecodeRuneInString(text[i:])
				width = utf8.EncodeRune(part[:], r)
				i += size
			} else {
				part[0] = text[i]
				i++
				width = 1
			}
		}
		for j := 0; j < width; j++ {
			b := part[j]
			starts[decoded%len(needle)] = begin
			for matched > 0 && needle[matched] != b {
				matched = prefix[matched-1]
			}
			if needle[matched] == b {
				matched++
			}
			if matched == len(needle) {
				sourceStart := starts[(decoded+1)%len(needle)]
				// Freeform plain matches stay with the existing form replacer.
				if sourceStart >= last && (jsonDocument || i-sourceStart > len(needle)) {
					if !changed {
						result.Grow(len(text))
						changed = true
					}
					result.WriteString(text[last:sourceStart])
					if truncated {
						for n := sourceStart; n < i; n++ {
							result.WriteByte(0)
						}
					} else {
						result.WriteString(marker)
					}
					last = i
				}
				matched = prefix[matched-1]
			}
			decoded++
		}
	}
	if !changed {
		return text
	}
	result.WriteString(text[last:])
	return result.String()
}

func jsonEscape(text string) (r rune, width int) {
	if len(text) < 2 || text[0] != '\\' {
		return 0, 0
	}
	switch text[1] {
	case '"', '\\', '/':
		return rune(text[1]), 2
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'u':
		if len(text) < 6 {
			return 0, 0
		}
		r, ok := jsonHex4(text[2:6])
		if !ok {
			return 0, 0
		}
		if r >= 0xd800 && r <= 0xdbff && len(text) >= 12 && text[6:8] == `\u` {
			low, ok := jsonHex4(text[8:12])
			if ok && low >= 0xdc00 && low <= 0xdfff {
				return 0x10000 + (r-0xd800)*0x400 + low - 0xdc00, 12
			}
		}
		if r >= 0xd800 && r <= 0xdfff {
			return utf8.RuneError, 6
		}
		return r, 6
	}
	return 0, 0
}

func jsonHex4(text string) (rune, bool) {
	var value rune
	for i := 0; i < 4; i++ {
		var digit byte
		switch c := text[i]; {
		case c >= '0' && c <= '9':
			digit = c - '0'
		case c >= 'a' && c <= 'f':
			digit = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			digit = c - 'A' + 10
		default:
			return 0, false
		}
		value = value<<4 | rune(digit)
	}
	return value, true
}

// javascriptJSONContent removes escapes Go adds to otherwise valid JSON strings.
func javascriptJSONContent(encoded string) string {
	var result strings.Builder
	result.Grow(len(encoded))
	for i := 0; i < len(encoded); {
		if encoded[i] == '\\' && i+1 < len(encoded) {
			if encoded[i+1] == '\\' {
				result.WriteString(encoded[i : i+2])
				i += 2
				continue
			}
			if encoded[i+1] == 'u' && i+6 <= len(encoded) {
				switch encoded[i+2 : i+6] {
				case "003c":
					result.WriteByte('<')
				case "003e":
					result.WriteByte('>')
				case "0026":
					result.WriteByte('&')
				case "2028":
					result.WriteRune('\u2028')
				case "2029":
					result.WriteRune('\u2029')
				default:
					result.WriteString(encoded[i : i+6])
				}
				i += 6
				continue
			}
		}
		result.WriteByte(encoded[i])
		i++
	}
	return result.String()
}

// RedactTail keeps useful trailing diagnostics without exposing a key cut at the tail boundary.
func RedactTail(tail *bounded.Tail, secret string) string {
	marker := redactionMarker(secret)
	text := strings.TrimSpace(redactText(tail.String(), secret, tail.Truncated))
	if len(text) > bounded.DiagnosticBytes {
		cut := len(text) - bounded.DiagnosticBytes
		if start := strings.LastIndex(text[:min(len(text), cut+len(marker))], marker); start >= 0 && start < cut && start+len(marker) > cut {
			cut = start + len(marker)
		}
		text = text[cut:]
	}
	return text
}

// RedactError redacts secret content in an error's displayed message.
//
// The wrapped cause remains unchanged for error matching. A nil error or empty
// secret leaves err unchanged.
func RedactError(err error, secret string) error {
	if err != nil && secret != "" {
		message := err.Error()
		if redacted := RedactText(message, secret); redacted != message {
			return &errRedacted{cause: err, message: redacted}
		}
	}
	return err
}

type errRedacted struct {
	cause   error
	message string
}

func (e *errRedacted) Error() string { return e.message }

func (e *errRedacted) Unwrap() error { return e.cause }
