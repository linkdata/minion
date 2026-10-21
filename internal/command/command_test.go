package command

import (
	"errors"
	"testing"
)

var defaultActions = map[string]Action{
	"review": Review, "fix": Fix, "audit": Comment, "security-audit": SecurityAudit,
}

func parseDefault(markdown string) (Command, error) {
	return ParseForLoginCommands(markdown, "linkdataminion", defaultActions)
}

func TestConfiguredCommandsUseNames(t *testing.T) {
	actions := map[string]Action{
		"review": Review, "fix": Fix, "audit": Comment,
		"security-audit": SecurityAudit,
		"inspect":        Comment,
	}
	for _, tt := range []struct {
		name   string
		action Action
	}{
		{"review", Review},
		{"fix", Fix},
		{"audit", Comment},
		{"security-audit", SecurityAudit},
		{"inspect", Comment},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := "@linkdataminion " + tt.name
			got, err := ParseForLoginCommands(body, "linkdataminion", actions)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.name || got.Action != tt.action {
				t.Fatalf("command = %+v", got)
			}
		})
	}
	for _, body := range []string{
		"@linkdataminion unknown", "@linkdataminion fix extra text", "@linkdataminion inspect now",
		"@linkdataminion release",
	} {
		if _, err := ParseForLoginCommands(body, "linkdataminion", actions); !errors.Is(err, ErrNoCommand) {
			t.Fatalf("accepted %q: %v", body, err)
		}
	}
	withNotes, err := ParseForLoginCommands("@linkdataminion review\n@linkdataminion fix", "linkdataminion", map[string]Action{"review": Review})
	if err != nil || withNotes.Name != "review" {
		t.Fatalf("disabled directive in notes = (%#v, %v), want review", withNotes, err)
	}
	if _, err := ParseForLoginCommands("@linkdataminion review\n@linkdataminion inspect", "linkdataminion", actions); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("accepted second configured directive: %v", err)
	}
	if _, err := ParseForLoginCommands("@linkdataminion review", "linkdataminion", nil); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("accepted command with no mappings: %v", err)
	}
}

func TestConfiguredCommandsRejectNamesWithWhitespace(t *testing.T) {
	for _, name := range []string{"review now", "review\tnow"} {
		got, err := ParseForLoginCommands("@linkdataminion "+name, "linkdataminion", map[string]Action{name: Review})
		if !errors.Is(err, ErrNoCommand) || got != (Command{}) {
			t.Fatalf("configured command %q = (%#v, %v), want ErrNoCommand", name, got, err)
		}
	}
}

func TestParseCommands(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		action Action
	}{
		{"review", "@linkdataminion review", Review},
		{"audit", "@linkdataminion audit", Comment},
		{"audit case", "  @LinkDataMinion\tAUDIT  ", Comment},
		{"fix", "  @LinkDataMinion\tFIX  \t", Fix},
		{"security audit", "@linkdataminion security-audit", SecurityAudit},
		{"CRLF", "\r\n@linkdataminion fix\r\n", Fix},
		{"surrounding blanks", " \t\n\n@linkdataminion security-audit\n\t", SecurityAudit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDefault(tt.body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.Action != tt.action {
				t.Fatalf("Parse() = %#v, want action %q", got, tt.action)
			}
		})
	}
}

func TestParseIgnoresNonCommands(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no mention", "Please review this change."},
		{"mention in prose", "Could @linkdataminion review this?"},
		{"command after prose", "Please take a look.\n@linkdataminion review"},
		{"lookalike handle", "@linkdataminion2 review"},
		{"handle suffix", "@linkdataminion-review"},
		{"non ASCII at sign", "＠linkdataminion review"},
		{"unknown verb", "@linkdataminion deploy"},
		{"dotted capital I verb", "@linkdataminion FİX"},
		{"Unicode handle separator", "@linkdataminion\u00a0review"},
		{"Unicode trailing whitespace", "@linkdataminion review\u3000"},
		{"review argument", "@linkdataminion review now"},
		{"audit argument", "@linkdataminion audit now"},
		{"fix argument", "@linkdataminion fix update docs"},
		{"history argument", "@linkdataminion review HiStOrY"},
		{"tab argument", "@linkdataminion review\tlater"},
		{"quote", "> @linkdataminion review"},
		{"indented code", "    @linkdataminion review\n\t@linkdataminion fix\n   \t@linkdataminion review"},
		{"fenced code", "```\n@linkdataminion review\n```"},
		{"HTML comment", "<!--\n@linkdataminion review\n-->"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDefault(tt.body)
			if !errors.Is(err, ErrNoCommand) {
				t.Fatalf("Parse() = (%#v, %v), want ErrNoCommand", got, err)
			}
			if got != (Command{}) {
				t.Fatalf("Parse() returned command on error: %#v", got)
			}
		})
	}
}

func TestParseAcceptsTrailingNotes(t *testing.T) {
	for _, tt := range []struct {
		command string
		notes   string
	}{
		{"@linkdataminion review", "Please check the locking change."},
		{"@linkdataminion review", "@linkdataminion fix later"},
		{"@linkdataminion audit", "Check authorization boundaries."},
		{"@linkdataminion fix", "The test fails on Windows."},
		{"@linkdataminion fix", "Inspect the regression's history.\nTrace its original cause."},
		{"@linkdataminion review", "@otherbot review\n```\nExample text\n```"},
		{"@linkdataminion review", "> @linkdataminion fix"},
	} {
		plain, err := parseDefault(tt.command)
		if err != nil {
			t.Fatal(err)
		}
		withNotes, err := parseDefault(tt.command + "\n\n" + tt.notes)
		if err != nil || withNotes != plain {
			t.Fatalf("Parse(%q) = (%#v, %v), want %#v", tt.command+"\n\n"+tt.notes, withNotes, err, plain)
		}
	}
}

func TestParseRejectsMultipleCommands(t *testing.T) {
	for _, body := range []string{
		"@linkdataminion review\n@linkdataminion fix",
		"@linkdataminion review\n@linkdataminion review",
		"@linkdataminion review\n@LinkDataMinion\tFIX",
		"@linkdataminion audit\n@linkdataminion fix",
		"@linkdataminion review\n```\n@linkdataminion fix\n```",
		"@linkdataminion review\n<!--\n@linkdataminion fix\n-->",
		"@linkdataminion review\n    @linkdataminion fix",
	} {
		got, err := parseDefault(body)
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("Parse(%q) = (%#v, %v), want ErrAmbiguous", body, got, err)
		}
		if got != (Command{}) {
			t.Fatalf("Parse() returned command on ambiguity: %#v", got)
		}
	}
}

func TestParseNormalizesCommands(t *testing.T) {
	a, err := parseDefault("@LINKDATAMINION\tREVIEW\t \t")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseDefault("  @linkdataminion review")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("equivalent commands differ: %#v, %#v", a, b)
	}
}

func TestParseConfiguredLogin(t *testing.T) {
	tests := []struct {
		name   string
		login  string
		body   string
		action Action
	}{
		{"custom login", "my-bot", "@MY-BOT fix", Fix},
		{"bot suffix", "github-actions[bot]", "@GitHub-Actions[Bot] review", Review},
		{"custom security audit", "my-bot", "@my-bot security-audit", SecurityAudit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseForLoginCommands(tt.body, tt.login, defaultActions)
			if err != nil {
				t.Fatalf("ParseForLoginCommands() error = %v", err)
			}
			if got.Action != tt.action {
				t.Fatalf("ParseForLoginCommands() = %#v, want action %q", got, tt.action)
			}
		})
	}
}

func TestParseConfiguredLoginRejectsLookalikes(t *testing.T) {
	for _, body := range []string{
		"@my-bot-extra review",
		"@my-bot.review",
		"@my-bot_ review",
		"@my-bоt review", // Cyrillic o.
		"@ｍy-bot review", // Fullwidth m.
	} {
		got, err := ParseForLoginCommands(body, "my-bot", defaultActions)
		if !errors.Is(err, ErrNoCommand) || got != (Command{}) {
			t.Fatalf("ParseForLoginCommands(%q) = (%#v, %v), want ErrNoCommand", body, got, err)
		}
	}
}

func TestParseConfiguredLoginRejectsInvalidLogins(t *testing.T) {
	for _, login := range []string{"", "@my-bot", "my bot", "my/bot", "-mybot", "mybot-", "my_bot", "my-bot[bo]", "my-bоt"} {
		got, err := ParseForLoginCommands("@my-bot review", login, defaultActions)
		if !errors.Is(err, ErrInvalidLogin) || got != (Command{}) {
			t.Fatalf("ParseForLoginCommands(login %q) = (%#v, %v), want ErrInvalidLogin", login, got, err)
		}
	}
}
