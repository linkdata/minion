package action

import (
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func TestRunnerCodexRetainsSafePartialProgressAfterFailure(t *testing.T) {
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
		case "codex.assistant_incomplete":
			if event.Agent != "native" || !strings.Contains(event.Text, "ordinary progress") {
				t.Fatalf("safe assistant prefix was lost: %+v", event)
			}
		case "codex.content_missing":
			if event.Text != "" {
				t.Fatalf("unsafe open tool content was retained: %+v", event)
			}
		case "codex.tool":
			if !strings.Contains(event.Text, "[redacted]") {
				t.Fatalf("completed tool content was not redacted: %+v", event)
			}
		}
	}
	for _, source := range []string{"codex.tool", "codex.assistant_incomplete", "codex.content_missing", "codex.call.failed"} {
		if !seen[source] {
			t.Fatalf("missing %s in records: %+v", source, saved)
		}
	}
}
