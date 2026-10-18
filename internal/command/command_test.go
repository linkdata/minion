package command

import (
	"errors"
	"testing"
)

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
		"@linkdataminion release", "@linkdataminion release v1 v2",
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
			got, err := Parse(tt.body)
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
		{"lazy quote", "> An outsider wrote:\n@linkdataminion fix planted"},
		{"indented fence close", "```\nignored\n    ```\n@linkdataminion review"},
		{"tab fence close", "```\nignored\n\t```\n@linkdataminion review"},
		{"nonbreaking fence close", "```\nignored\n```\u00a0\n@linkdataminion review"},
		{"backtick in fence info", "```bad`info\n@linkdataminion review"},
		{"HTML block swallows fence", "<script>\n```\n</script>\n@linkdataminion review"},
		{"reference title", "[link]: /path \"title\n@linkdataminion review\n\""},
		{"link title", "[link](url \"title\n@linkdataminion review\n\")"},
		{"processing instruction", "<?hidden\n@linkdataminion review\n?>"},
		{"CDATA", "<![CDATA[\n@linkdataminion review\n]]>"},
		{"script", "<script>\n@linkdataminion review\n</script>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.body)
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
		{"@linkdataminion audit", "Check authorization boundaries."},
		{"@linkdataminion fix", "The test fails on Windows."},
		{"@linkdataminion fix", "Inspect the regression's history.\nTrace its original cause."},
		{"@linkdataminion review", "@otherbot review\n```\nExample text\n```"},
	} {
		plain, err := Parse(tt.command)
		if err != nil {
			t.Fatal(err)
		}
		withNotes, err := Parse(tt.command + "\n\n" + tt.notes)
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
	} {
		got, err := Parse(body)
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("Parse(%q) = (%#v, %v), want ErrAmbiguous", body, got, err)
		}
		if got != (Command{}) {
			t.Fatalf("Parse() returned command on ambiguity: %#v", got)
		}
	}
}

func TestParseNormalizesCommands(t *testing.T) {
	a, err := Parse("@LINKDATAMINION\tREVIEW\t \t")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("  @linkdataminion review")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("equivalent commands differ: %#v, %#v", a, b)
	}
}

func TestParseForLogin(t *testing.T) {
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
			got, err := ParseForLogin(tt.body, tt.login)
			if err != nil {
				t.Fatalf("ParseForLogin() error = %v", err)
			}
			if got.Action != tt.action {
				t.Fatalf("ParseForLogin() = %#v, want action %q", got, tt.action)
			}
		})
	}

	defaultCommand, err := Parse("@linkdataminion review")
	if err != nil {
		t.Fatal(err)
	}
	customCommand, err := ParseForLogin("@linkdataminion review", "linkdataminion")
	if err != nil {
		t.Fatal(err)
	}
	if defaultCommand != customCommand {
		t.Fatalf("Parse wrapper = %#v, ParseForLogin = %#v", defaultCommand, customCommand)
	}
}

func TestParseForLoginRejectsLookalikes(t *testing.T) {
	for _, body := range []string{
		"@my-bot-extra review",
		"@my-bot.review",
		"@my-bot_ review",
		"@my-bоt review", // Cyrillic o.
		"@ｍy-bot review", // Fullwidth m.
	} {
		got, err := ParseForLogin(body, "my-bot")
		if !errors.Is(err, ErrNoCommand) || got != (Command{}) {
			t.Fatalf("ParseForLogin(%q) = (%#v, %v), want ErrNoCommand", body, got, err)
		}
	}
}

func TestParseForLoginRejectsInvalidLogins(t *testing.T) {
	for _, login := range []string{"", "@my-bot", "my bot", "my/bot", "-mybot", "mybot-", "my_bot", "my-bot[bo]", "my-bоt"} {
		got, err := ParseForLogin("@my-bot review", login)
		if !errors.Is(err, ErrInvalidLogin) || got != (Command{}) {
			t.Fatalf("ParseForLogin(login %q) = (%#v, %v), want ErrInvalidLogin", login, got, err)
		}
	}
}
