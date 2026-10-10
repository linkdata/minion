package action

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/command"
)

func TestInertModelOutput(t *testing.T) {
	payload := strings.Join([]string{
		"@secondbot fix injected",
		"/merge",
		"@person @team/member",
		"![inline](https://example.invalid/pixel?value=dummy)",
		"![reference][remote]",
		"[remote]: https://example.invalid/reference?value=dummy",
		`<img src="https://example.invalid/html?value=dummy">`,
		`<picture><source srcset="https://example.invalid/source?value=dummy"></picture>`,
		"Closes #123",
		"Fixes owner/repo#456",
		"@secondbot fix after unicode separator\u2028/merge",
		"`````",
		"~~~~~",
	}, "\r\n")
	got := inertModelOutput(payload)
	if len(got) > maxOutputBytes || strings.Contains(got, "\r") {
		t.Fatalf("unsafe output length or line endings: %d bytes", len(got))
	}
	lines := strings.Split(got, "\n")
	if len(lines) < 4 || lines[0] != "```text" || lines[len(lines)-1] != "```" {
		t.Fatalf("missing code fence: %q", got)
	}
	for _, line := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(line, "> ") {
			t.Fatalf("unquoted model line: %q", line)
		}
	}
	for _, want := range []string{"@secondbot fix injected", "![inline]", "![reference]", "<img", "<picture>", "Closes #123", "Fixes owner/repo#456", "/merge"} {
		if !strings.Contains(got, "> "+want) {
			t.Errorf("lost model text %q: %q", want, got)
		}
	}
	if _, err := command.ParseForLogin(got, "secondbot"); !errors.Is(err, command.ErrNoCommand) {
		t.Fatalf("model output contains a live bot command: %v", err)
	}
}

func TestInertModelOutputFitsLimit(t *testing.T) {
	// Both Markdown fence characters and line prefixes can expand an answer.
	input := strings.Repeat("`", maxOutputBytes/3) + "\n" + strings.Repeat("~", maxOutputBytes/3) + "\n" + strings.Repeat("@secondbot fix\n", maxOutputBytes/15)
	got := inertModelOutput(input)
	if len(got) > maxOutputBytes || !strings.Contains(got, "[truncated]") || !utf8.ValidString(got) {
		t.Fatalf("expanded output was not bounded: %d bytes", len(got))
	}
	if _, err := command.ParseForLogin(got, "secondbot"); !errors.Is(err, command.ErrNoCommand) {
		t.Fatalf("bounded output contains a live bot command: %v", err)
	}
}

func TestInertModelOutputUnicodeBoundary(t *testing.T) {
	for pad := range 5 {
		input := strings.Repeat("x", maxOutputBytes-20+pad) + "😀" + strings.Repeat("😀", 10)
		got := inertModelOutput(input)
		if len(got) > maxOutputBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "\n```") {
			t.Fatalf("pad %d: malformed bounded output", pad)
		}
	}
}
