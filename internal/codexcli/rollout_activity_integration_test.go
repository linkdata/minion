//go:build linux || darwin

package codexcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
	"golang.org/x/sys/unix"
)

// The parent stdout goes quiet after announcing a helper. The only evidence of
// ongoing work is the helper's native rollout until the test releases the CLI.
func TestSessionCapturesQuietParentHelperBeforeCLIExit(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
cp "$0.child" "$dir/rollout-child.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.started","item":{"id":"spawn","type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["child"]}}'
IFS= read -r release < "$0.gate"
cat "$0.child-tail" >> "$dir/rollout-child.jsonl"
cat "$0.root-tail" >> "$dir/rollout-root.jsonl"
printf '%s\n' '{"type":"item.completed","item":{"id":"spawn","type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["child"]}}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "root", []byte(`{"type":"session_meta","payload":{"id":"root","session_id":"root","source":"exec"}}`+"\n"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, t.TempDir(), "child", "root", &boundary,
		usageEventAt(9, "task_started", "child-turn"),
		`{"ordinal":10,"type":"response_item","payload":{"type":"message","id":"live-message","role":"assistant","content":[{"type":"output_text","text":"live-helper"}]}}`,
		`{"ordinal":11,"type":"response_item","payload":{"type":"reasoning","id":"live-reasoning","summary":[{"type":"summary_text","text":"reasoning-summary"}]}}`,
		`{"ordinal":12,"type":"response_item","payload":{"type":"custom_tool_call","id":"live-tool","call_id":"call-live","name":"exec","input":"echo hello"}}`,
		`{"ordinal":13,"type":"response_item","payload":{"type":"custom_tool_call_output","id":"live-output","call_id":"call-live","output":[{"type":"input_text","text":"hello"}]}}`,
		usageRecordAt(15, "child", "child-turn", "child-response", 20, 0, 1))
	childData, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	putRolloutTemplate(t, binary, "child", childData)
	putRolloutTemplate(t, binary, "child-tail", []byte(`{"ordinal":16,"type":"response_item","payload":{"type":"message","id":"late-message","role":"assistant","content":[{"type":"output_text","text":"late-helper"}]}}`+"\n"+usageEventAt(20, "task_complete", "child-turn")+"\n"))
	putRolloutTemplate(t, binary, "root-tail", []byte(usageEventAt(20, "task_started", "root-turn")+"\n"+usageRecordAt(25, "root", "root-turn", "root-response", 10, 0, 1)+"\n"+usageEventAt(30, "task_complete", "root-turn")+"\n"))
	if err := unix.Mkfifo(binary+".gate", 0600); err != nil {
		t.Fatalf("create test gate: %v", err)
	}
	home := os.Getenv("CODEX_HOME")
	unrelated := filepath.Join(home, "sessions", filepath.FromSlash(day), "unrelated.jsonl")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0700); err != nil {
		t.Fatal(err)
	}
	unrelatedData := []byte(`{"type":"session_meta","payload":{"id":"unrelated","session_id":"unrelated","source":"exec"}}` + "\n")
	if err := os.WriteFile(unrelated, unrelatedData, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	events := make(chan aicli.StreamEvent, 32)
	progress := make(chan struct{}, 8)
	var session Session
	session.OnEvent = func(event aicli.StreamEvent) { events <- event }
	session.OnProgress = func() {
		select {
		case progress <- struct{}{}:
		default:
		}
	}
	type callResult struct {
		result aicli.Result
		err    error
	}
	done := make(chan callResult, 1)
	source := t.TempDir()
	go func() {
		result, err := session.Run(ctx, source, "task", false, "", config.CodexConfig{})
		done <- callResult{result, err}
	}()
	seen := map[string]bool{}
	var captured []aicli.StreamEvent
	for !(seen["assistant"] && seen["reasoning_summary"] && seen["tool"]) {
		select {
		case event := <-events:
			captured = append(captured, event)
			if event.NativeID == "child" {
				seen[event.Kind] = true
			}
		case result := <-done:
			t.Fatalf("CLI exited before live helper capture: result=%+v err=%v events=%+v", result.result, result.err, captured)
		case <-ctx.Done():
			t.Fatalf("live helper capture did not arrive: %v; events=%+v", ctx.Err(), captured)
		}
	}
	select {
	case <-progress:
	case result := <-done:
		t.Fatalf("CLI exited without live progress: result=%+v err=%v", result.result, result.err)
	case <-ctx.Done():
		t.Fatalf("helper activity did not advance progress: %v", ctx.Err())
	}
	var gateFD int
	gateReady := time.NewTicker(10 * time.Millisecond)
	defer gateReady.Stop()
	for {
		gateFD, err = unix.Open(binary+".gate", unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENXIO) {
			t.Fatalf("open fake CLI gate: %v", err)
		}
		select {
		case result := <-done:
			t.Fatalf("fake CLI exited before gate opened: result=%+v err=%v", result.result, result.err)
		case <-ctx.Done():
			t.Fatalf("fake CLI gate did not open: %v", ctx.Err())
		case <-gateReady.C:
		}
	}
	if _, err := unix.Write(gateFD, []byte("release\n")); err != nil {
		_ = unix.Close(gateFD)
		t.Fatalf("release fake CLI: %v", err)
	}
	if err := unix.Close(gateFD); err != nil {
		t.Fatalf("close fake CLI gate: %v", err)
	}
	var completed callResult
	select {
	case completed = <-done:
	case <-ctx.Done():
		t.Fatalf("fake CLI did not finish: %v", ctx.Err())
	}
	if completed.err != nil || completed.result.Answer != "done" || !completed.result.UsageComplete || completed.result.Tokens != (usage.Tokens{Input: 30, Output: 2}) {
		t.Fatalf("completed call = %+v, %v", completed.result, completed.err)
	}
	for len(events) != 0 {
		captured = append(captured, <-events)
	}
	late := 0
	for _, event := range captured {
		if event.NativeID == "child" && strings.Contains(event.Text, "late-helper") {
			late++
		}
	}
	if late != 1 {
		t.Fatalf("late helper record count = %d; events=%+v", late, captured)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, id := range []string{"root", "child"} {
		path := filepath.Join(home, "sessions", filepath.FromSlash(day), "rollout-"+id+".jsonl")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned rollout remains %q: %v", path, err)
		}
	}
	if data, err := os.ReadFile(unrelated); err != nil || !bytes.Equal(data, unrelatedData) {
		t.Fatalf("unrelated rollout changed: %q, %v", data, err)
	}
}
