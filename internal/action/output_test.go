package action

import (
	"errors"
	"fmt"
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
	if _, err := command.ParseForLoginCommands(got, "secondbot", defaultActionTestCommands); !errors.Is(err, command.ErrNoCommand) {
		t.Fatalf("model output contains a live bot command: %v", err)
	}
}

func TestInertModelOutputPreservesOverBudgetContent(t *testing.T) {
	input := strings.Repeat("a\n", 25000)
	got := inertModelOutput(input)
	want := "```text\n> " + strings.ReplaceAll(strings.TrimSpace(input), "\n", "\n> ") + "\n```"
	if modelOutputFits(input) || len(got) <= maxOutputBytes || got != want || strings.Contains(got, "[truncated]") {
		t.Fatalf("renderer silently dropped content or size check missed expansion: %d bytes", len(got))
	}
}

func TestInertModelOutputUnicodeBoundary(t *testing.T) {
	input := strings.Repeat("x", maxOutputBytes-18) + "😀"
	got := inertModelOutput(input)
	if !modelOutputFits(input) || len(got) > maxOutputBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "\n```") {
		t.Fatal("malformed bounded Unicode output")
	}
	if modelOutputFits(input + "x") {
		t.Fatal("one-byte rendered overflow accepted")
	}
}

func TestModelOutputFitMatchesNormalizedRendererBoundary(t *testing.T) {
	for _, separator := range []string{"\n", "\r", "\r\n", "\v", "\f", "\x1c", "\x1d", "\x1e", "\u0085", "\u2028", "\u2029"} {
		t.Run(fmt.Sprintf("%x", separator), func(t *testing.T) {
			input := strings.Repeat("x", maxOutputBytes-18) + separator + "a"
			if !modelOutputFits(input) || len(inertModelOutput(input)) != maxOutputBytes {
				t.Fatalf("normalized exact boundary refused or wrong size: fit=%t size=%d", modelOutputFits(input), len(inertModelOutput(input)))
			}
			if modelOutputFits(input+"x") || len(inertModelOutput(input+"x")) != maxOutputBytes+1 {
				t.Fatal("one-byte rendered overflow was accepted or truncated")
			}
		})
	}
}
