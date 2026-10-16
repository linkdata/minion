package command

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestConfiguredCommandsPreserveKeysAndUseNames(t *testing.T) {
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
			oldDigest := sha256.Sum256([]byte(tt.name + "\x00"))
			if got.Key != hex.EncodeToString(oldDigest[:]) || got.Name != tt.name || got.Action != tt.action || got.History {
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
	withHistory, err := ParseForLoginCommands("@linkdataminion inspect history\nCheck the previous design.", "linkdataminion", actions)
	if err != nil || withHistory.Name != "inspect" || withHistory.Action != Comment || !withHistory.History {
		t.Fatalf("configured history command = (%#v, %v)", withHistory, err)
	}
	if _, err := ParseForLoginCommands("@linkdataminion review", "linkdataminion", nil); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("accepted command with no mappings: %v", err)
	}
}

func TestParseCommands(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		action  Action
		history bool
	}{
		{"review", "@linkdataminion review", Review, false},
		{"audit", "@linkdataminion audit", Comment, false},
		{"audit case", "  @LinkDataMinion\tAUDIT  ", Comment, false},
		{"fix", "  @LinkDataMinion\tFIX  \t", Fix, false},
		{"security audit", "@linkdataminion security-audit", SecurityAudit, false},
		{"CRLF", "\r\n@linkdataminion fix\r\n", Fix, false},
		{"surrounding blanks", " \t\n\n@linkdataminion security-audit\n\t", SecurityAudit, false},
		{"review history", "@linkdataminion review history", Review, true},
		{"audit history", "@linkdataminion audit history", Comment, true},
		{"fix history", "\r\n @LinkDataMinion\tFIX\tHiStOrY \t\r\n", Fix, true},
		{"security audit history", "@linkdataminion security-audit history", SecurityAudit, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.Action != tt.action || got.History != tt.history {
				t.Fatalf("Parse() = %#v, want action %q and history %t", got, tt.action, tt.history)
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
		{"fix argument", "@linkdataminion fix update docs"},
		{"quoted audit", "> @linkdataminion audit"},
		{"fenced audit", "```\n@linkdataminion audit\n```"},
		{"removed release", "@linkdataminion release"},
		{"removed release with tag", "@linkdataminion release v1"},
		{"unknown modifier", "@linkdataminion review full"},
		{"duplicate modifier", "@linkdataminion review history history"},
		{"extra modifier", "@linkdataminion review history now"},
		{"misplaced modifier", "@linkdataminion history review"},
		{"Unicode lookalike modifier", "@linkdataminion review hіstory"},
		{"Unicode space before modifier", "@linkdataminion review\u00a0history"},
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
		{"@linkdataminion fix history", "Inspect the regression's history."},
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
		"@linkdataminion review\n@linkdataminion fix history",
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

func TestCommandKeyIsStableAndSensitiveToHistory(t *testing.T) {
	a, err := Parse("@LINKDATAMINION\tREVIEW\t HiStOrY \t")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("  @linkdataminion review history")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse("@linkdataminion review")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatalf("equivalent commands have different keys: %q, %q", a.Key, b.Key)
	}
	if a.Key == c.Key {
		t.Fatal("history and no-history have the same key")
	}
	digest := sha256.Sum256([]byte("review\x00history"))
	if !a.History || a.Key != hex.EncodeToString(digest[:]) {
		t.Fatalf("history command = %#v", a)
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
			if got.Action != tt.action || got.History {
				t.Fatalf("ParseForLogin() = %#v, want action %q without history", got, tt.action)
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
