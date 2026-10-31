package action

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func TestRunnerCodexMarksConfiguredCredentialPartialContentMissing(t *testing.T) {
	const secret = "fixture-very-private-key"
	progress := strings.Repeat("ordinary progress ", 40)
	lines := []string{
		`{"type":"thread.started","thread_id":"native"}`,
		`{"type":"item.started","item":{"id":"message","type":"agent_message","text":"first snapshot"}}`,
		`{"type":"item.updated","item":{"id":"message","type":"agent_message","text":` + strconv.Quote(progress+secret[:len(secret)-3]) + `}}`,
		`{"type":"item.completed","item":{"id":"complete-tool","type":"command_execution","command":"echo ` + secret + `","aggregated_output":"done"}}`,
		`{"type":"item.started","item":{"id":"open-tool","type":"command_execution","command":"echo ` + secret[:len(secret)-3] + `"}}`,
		`{"type":"turn.failed","error":{"message":"provider stopped"}}`,
	}
	script := "#!/bin/sh\ncat >/dev/null\n"
	for _, line := range lines {
		script += "printf '%s\\n' " + shellQuote(line) + "\n"
	}
	installActionTestSkills(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	fakeOnPath(t, "codex", script)
	job := testJob(command.Comment)
	job.Main.Provider, job.Main.Model = "codex", "fixture-codex"
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: secret}, eventSink: queue.add}
	result, err := runner.Run(t.Context(), job)
	if err == nil || result.Answer != "" || queue.finish() {
		t.Fatalf("failed partial call was accepted: result=%+v error=%v", result, err)
	}
	seen := map[string]bool{}
	for _, event := range saved {
		seen[event.EventSource] = true
		if strings.Contains(event.Text, secret) || strings.Contains(event.Text, secret[:len(secret)-3]) ||
			strings.HasSuffix(event.EventSource, ".started") || strings.HasSuffix(event.EventSource, ".updated") {
			t.Fatalf("raw progress or credential reached a record: %+v", event)
		}
		switch event.EventSource {
		case "codex.content_missing":
			if event.Text != "" {
				t.Fatalf("unsafe open content was retained: %+v", event)
			}
		case "codex.tool":
			if !strings.Contains(event.Text, "[redacted]") {
				t.Fatalf("completed tool content was not redacted: %+v", event)
			}
		}
	}
	for _, source := range []string{"codex.tool", "codex.content_missing", "codex.call.failed"} {
		if !seen[source] {
			t.Fatalf("missing %s in records: %+v", source, saved)
		}
	}
	if seen["codex.assistant_incomplete"] || seen["codex.tool_incomplete"] {
		t.Fatalf("configured-key call retained open content: %+v", saved)
	}
}

func TestRunnerSuppressesIncompleteContentForOtherProviderCredential(t *testing.T) {
	const secret = "other-provider-private-key"
	job := testJob(command.Comment)
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	failure := errors.New("provider stopped")
	runner := Runner{WorkRoot: t.TempDir(), Claude: config.ClaudeConfig{APIKey: secret}, eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant_incomplete", Text: "ordinary work " + secret[:10] + " then more text"})
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "tool_incomplete", Text: `{"type":"command_execution","command":"echo ` + secret[:10] + ` then more"}`})
			return aicli.Result{}, failure
		}}
	_, err := runner.Run(t.Context(), job)
	if !errors.Is(err, failure) || queue.finish() {
		t.Fatalf("failed call was accepted: %v", err)
	}
	missing := 0
	for _, event := range saved {
		if strings.Contains(event.Text, secret[:10]) || strings.HasSuffix(event.EventSource, "_incomplete") {
			t.Fatalf("other provider credential fragment escaped: %+v", event)
		}
		if event.EventSource == "codex.content_missing" {
			if event.Text != "" || event.Agent != "native" {
				t.Fatalf("incomplete content was not converted to metadata: %+v", event)
			}
			missing++
		}
	}
	if missing != 2 {
		t.Fatalf("missing content records = %d, want 2: %+v", missing, saved)
	}
}
