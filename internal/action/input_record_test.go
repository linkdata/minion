package action

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
)

func TestPreparedInputTextPreservesNonsecretInvocation(t *testing.T) {
	input := aicli.PreparedInput{
		NativeID: "session-17", Mode: "resume", Path: "/bin/claude",
		Args: []string{"claude", "--resume", "session-17", "--json-schema", `{"type":"object"}`},
		Dir:  "/work/tree", Stdin: "skill path: /work/skills/review\nReview this change.\n",
		Schema: `{"type":"object","properties":{"answer":{"type":"string"}}}`,
	}
	text, reason := preparedInputText(input)
	if reason != "" {
		t.Fatalf("prepared input unavailable: %s", reason)
	}
	var got preparedInputPayload
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Representation != "redacted" || got.Stage != "prepared" || got.Mode != input.Mode ||
		got.Path != input.Path || got.Dir != input.Dir || got.Stdin != input.Stdin || got.Schema != input.Schema ||
		!slices.Equal(got.Argv, input.Args) {
		t.Fatalf("prepared invocation changed: %+v", got)
	}
}

func TestPreparedInputTextRedactsConfiguredKey(t *testing.T) {
	const key = "private-token-123"
	input := aicli.PreparedInput{
		Mode: "fresh", Path: "/bin/codex", Dir: "/work/tree",
		Args:  []string{"codex", "--flag=" + key},
		Stdin: "prompt " + key, Schema: `{"key":"` + key + `"}`,
	}
	text, reason := preparedInputText(input, key)
	if reason != "" || strings.Contains(text, key) {
		t.Fatalf("configured key was not safely redacted: reason=%q text=%q", reason, text)
	}
	var got preparedInputPayload
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Representation != "redacted" || !strings.Contains(got.Stdin, "[redacted]") ||
		!strings.Contains(got.Argv[1], "[redacted]") || !strings.Contains(got.Schema, "[redacted]") {
		t.Fatalf("redaction did not cover invocation fields: %+v", got)
	}
}

func TestPreparedInputTextMarksUnrepresentableInvocationsUnavailable(t *testing.T) {
	base := aicli.PreparedInput{Mode: "fresh", Path: "/bin/codex", Args: []string{"codex"}, Dir: "/work", Stdin: "prompt"}
	for _, test := range []struct {
		name, reason string
		change       func(*aicli.PreparedInput)
	}{
		{"invalid UTF-8", "invalid_utf8", func(in *aicli.PreparedInput) { in.Stdin = string([]byte{0xff}) }},
		{"oversized input", "event_limit", func(in *aicli.PreparedInput) { in.Stdin = strings.Repeat("x", aicli.MaxEventBytes) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := base
			test.change(&input)
			text, reason := preparedInputText(input)
			if text != "" || reason != test.reason {
				t.Fatalf("text length=%d reason=%q, want empty text and %q", len(text), reason, test.reason)
			}
		})
	}
}

func TestPreparedInputTextPreservesNULThroughJSON(t *testing.T) {
	input := aicli.PreparedInput{
		Mode: "fresh", Path: "/bin/codex", Args: []string{"codex", "before\x00after"},
		Dir: "/work", Stdin: "before\x00after",
	}
	text, reason := preparedInputText(input)
	if reason != "" {
		t.Fatalf("prepared input unavailable: %s", reason)
	}
	var got preparedInputPayload
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Stdin != input.Stdin || !slices.Equal(got.Argv, input.Args) {
		t.Fatalf("NUL-bearing invocation changed: %+v", got)
	}
}

func TestProviderEventQueueAcceptsPreparedInputBeforeNativeIdentity(t *testing.T) {
	var sent []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		sent = append(sent, event)
		return nil
	})
	queue.add(attemptRecord{Kind: attemptRecordInput, Call: "1", Text: `{"stage":"prepared"}`})
	queue.add(attemptRecord{Kind: attemptRecordInput, Call: "1", Agent: "root-session", EventSource: "codex.identity"})
	if !queue.finish() || len(sent) != 2 || sent[0].Agent != "" || sent[1].Agent != "root-session" {
		t.Fatalf("prepared input and subsequent identity were not preserved: %+v", sent)
	}

	var leaked []attemptRecord
	queue = newProviderEventQueue(func(event attemptRecord) error {
		leaked = append(leaked, event)
		return nil
	})
	queue.add(attemptRecord{Kind: attemptRecordDiscussion, Text: "unbound provider output"})
	if queue.finish() || len(leaked) != 0 {
		t.Fatalf("unbound discussion falsely claimed coverage: %+v", leaked)
	}
}
