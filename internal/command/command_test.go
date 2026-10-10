package command

import (
	"encoding/hex"
	"errors"
	"testing"
)

func TestParseCommands(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		action Action
		args   string
	}{
		{"review", "@linkdataminion review\nPlease take a look.", Review, ""},
		{"audit", "@linkdataminion audit", Audit, ""},
		{"audit case", "  @LinkDataMinion\tAUDIT  ", Audit, ""},
		{"fix with text", "  @LinkDataMinion\tFIX\t  update the docs  \t", Fix, "update the docs"},
		{"fix without text", "@linkdataminion fix", Fix, ""},
		{"release", "@linkdataminion release v1.2.3", Release, "v1.2.3"},
		{"CRLF", "\r\n@linkdataminion fix now\r\nDetails follow.", Fix, "now"},
		{"leading blanks", " \t\n\n@linkdataminion release v2\nDetails follow.", Release, "v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.Action != tt.action || got.Args != tt.args {
				t.Fatalf("Parse() = %#v, want action %q and args %q", got, tt.action, tt.args)
			}
			if len(got.Key) != 64 {
				t.Fatalf("key length = %d, want 64", len(got.Key))
			}
			if _, err := hex.DecodeString(got.Key); err != nil {
				t.Fatalf("key is not hex: %v", err)
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
		{"review argument", "@linkdataminion review now"},
		{"audit argument", "@linkdataminion audit now"},
		{"quoted audit", "> @linkdataminion audit"},
		{"fenced audit", "```\n@linkdataminion audit\n```"},
		{"missing release tag", "@linkdataminion release"},
		{"multiple release arguments", "@linkdataminion release v1 v2"},
		{"Unicode space in tag", "@linkdataminion release v1\u00a0v2"},
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

func TestParseRejectsMultipleCommands(t *testing.T) {
	for _, body := range []string{
		"@linkdataminion review\n@linkdataminion fix update tests",
		"@linkdataminion review\n@linkdataminion review",
		"@linkdataminion audit\n@linkdataminion fix",
		"@linkdataminion review\n```\n@linkdataminion fix hidden\n```",
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

func TestCommandKeyIsStableAndSensitiveToArguments(t *testing.T) {
	a, err := Parse("@LINKDATAMINION\tFIX\t update the docs \t")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("  @linkdataminion fix update the docs")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse("@linkdataminion fix update the tests")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatalf("equivalent commands have different keys: %q, %q", a.Key, b.Key)
	}
	if a.Key == c.Key {
		t.Fatal("different arguments have the same key")
	}
}

func TestParseForLogin(t *testing.T) {
	tests := []struct {
		name   string
		login  string
		body   string
		action Action
		args   string
	}{
		{"custom login", "my-bot", "@MY-BOT fix update docs", Fix, "update docs"},
		{"bot suffix", "github-actions[bot]", "@GitHub-Actions[Bot] review", Review, ""},
		{"ignores other bot", "my-bot", "@my-bot release v2\n@linkdataminion review", Release, "v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseForLogin(tt.body, tt.login)
			if err != nil {
				t.Fatalf("ParseForLogin() error = %v", err)
			}
			if got.Action != tt.action || got.Args != tt.args {
				t.Fatalf("ParseForLogin() = %#v, want action %q and args %q", got, tt.action, tt.args)
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
