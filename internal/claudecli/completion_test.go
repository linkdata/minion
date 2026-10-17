package claudecli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestOutputStoppedTaskNeedsNewRootResponseAndResult(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"new"}}`
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"tests"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const stop = `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command tests was stopped"}`
	notice, _ := json.Marshal(`<task-notification><task-id>tests</task-id><status>killed</status><summary>Tests stopped</summary></task-notification>`)
	synthetic := `{"type":"user","isSynthetic":true,"message":{"content":` + string(notice) + `}}`
	noticeEvent := func(summary string) string {
		value, _ := json.Marshal("<task-notification><status>killed</status><summary>" + summary + "</summary></task-notification>")
		return `{"type":"user","isSynthetic":true,"message":{"content":` + string(value) + `}}`
	}
	for _, tc := range []struct {
		name, after string
		fails       bool
	}{
		{"wind down then held waiting result", pending + "\n" + stop + "\n" + clear + "\n" + successfulResult, true},
		{"stop after final", stop, true},
		{"new result without root response", stop + "\n" + successfulResult, true},
		{"repeated old root ID", stop + "\n" + old + "\n" + successfulResult, true},
		{"unknown parent cannot assess stop", stop + "\n" + strings.Replace(fresh, `"parent_tool_use_id":null,`, "", 1) + "\n" + successfulResult, true},
		{"delegate cannot assess stop", stop + "\n" + strings.Replace(fresh, `"parent_tool_use_id":null`, `"parent_tool_use_id":"parent"`, 1) + "\n" + successfulResult, true},
		{"new root without final result", stop + "\n" + fresh, true},
		{"new root assessment and final", stop + "\n" + fresh + "\n" + successfulResult, false},
		{"memory pressure remains fatal after new root", strings.Replace(stop, `"summary":"Background command tests was stopped"`, `"summary":"Background command \"tests\" was stopped because the system is running low on memory"`, 1) + "\n" + fresh + "\n" + successfulResult, true},
		{"ambient stop after final", strings.Replace(stop, `"status":"stopped"`, `"status":"stopped","ambient":true`, 1), false},
		{"synthetic killed string", synthetic + "\n" + successfulResult, true},
		{"synthetic stopped block", `{"type":"user","isSynthetic":true,"message":{"content":[{"type":"text","text":` + strings.Replace(string(notice), "killed", "stopped", 1) + `}]}}` + "\n" + successfulResult, true},
		{"synthetic stop assessed", synthetic + "\n" + fresh + "\n" + successfulResult, false},
		{"raw ampersand notice", noticeEvent(`Background command "go test && go vet" was stopped`), true},
		{"raw less-than notice", noticeEvent(`Background command "grep x <file" was stopped`), true},
		{"entity-escaped notice", noticeEvent(`Background command &quot;go test &amp;&amp; go vet&quot; was stopped`), true},
		{"killed system event", strings.Replace(stop, `"status":"stopped"`, `"status":"killed"`, 1), true},
		{"raw memory-pressure summary", noticeEvent(`Background command "go test && go vet" was stopped because the system is running low on memory`) + "\n" + fresh + "\n" + successfulResult, true},
		{"literal closing summary tag in command", noticeEvent(`Background command "echo </summary> && go vet" was stopped because the system is running low on memory`) + "\n" + fresh + "\n" + successfulResult, true},
		{"ordinary user quote", strings.Replace(synthetic, `"isSynthetic":true`, `"isSynthetic":false`, 1), false},
		{"ordinary failed command assessed", strings.Replace(stop, `"status":"stopped"`, `"status":"failed"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			data := numberResultEvents(old+"\n"+successfulResult+"\n"+tc.after) + "\n"
			writeChunks(t, &o, data)
			o.finish()
			if (o.err != nil) != tc.fails || o.completed == tc.fails || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("result=%+v completed=%v error=%v", o.result, o.completed, o.err)
			}
		})
	}
}

func TestOutputTerminalReasonRequiresCompletion(t *testing.T) {
	for _, reason := range []string{`"completed"`, `"tool_deferred"`, `"aborted_tools"`, `"aborted_streaming"`, `"background_requested"`, `"hook_stopped"`, `"stop_hook_prevented"`, `""`, `null`, `42`} {
		t.Run(reason, func(t *testing.T) {
			terminal := strings.TrimSuffix(successfulResult, "}") + `,"terminal_reason":` + reason + `}`
			for _, recover := range []bool{false, true} {
				o := jsonlOutput{}
				data := successfulResult + "\n" + terminal
				if recover {
					data += "\n" + strings.TrimSuffix(successfulResult, "}") + `,"terminal_reason":"completed"}`
				}
				_, _ = o.Write([]byte(numberResultEvents(data)))
				o.finish()
				fails := !recover && reason != `"completed"`
				if (o.err != nil) != fails || o.result.UsageComplete == fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
					t.Fatalf("recover=%v result=%+v error=%v", recover, o.result, o.err)
				}
			}
		})
	}
}

func TestOutputUsageGraceStartsOnlyOnce(t *testing.T) {
	const rejected = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`
	const paid = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","isUsingOverage":true}}`
	const pressure = `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command \"tests\" was stopped because the system is running low on memory"}`
	for _, events := range []string{rejected + "\n" + rejected, pressure + "\n" + pressure, rejected + "\n" + pressure, pressure + "\n" + rejected} {
		calls := 0
		o := jsonlOutput{fatal: func() { calls++ }}
		_, _ = o.Write([]byte(paid + "\n"))
		if calls != 0 {
			t.Fatal("usable overage started cancellation grace")
		}
		_, _ = o.Write([]byte(events + "\n"))
		if calls != 1 || o.err != nil {
			t.Fatalf("grace calls=%d error=%v", calls, o.err)
		}
		_, _ = o.Write([]byte(successfulResult + "\n"))
		o.finish()
		if o.err == nil || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
			t.Fatalf("late result usage lost: %+v error=%v", o.result, o.err)
		}
	}
}

func TestRunFatalGraceStopsWatchdogAndKeepsObservedUsage(t *testing.T) {
	for _, scenario := range []struct{ name, event, failure string }{
		{"quota", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, "provider usage limit reached"},
		{"pressure", `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command \"tests\" was stopped because the system is running low on memory"}`, "provider memory pressure"},
	} {
		for _, resultFollows := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/result=%t", scenario.name, resultFollows), func(t *testing.T) {
				script := "cat >/dev/null\ncat <<'EVENTS'\n" + assistantUsageEvent("root", 3, 5, 7, 2) + "\n" + `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}` + "\n" + scenario.event + "\nEVENTS\n"
				if resultFollows {
					script += "sleep 0.1\ncat <<'RESULT'\n" + successfulResult + "\nRESULT\n"
				}
				script += "sleep 30\n"
				fakeExecutable(t, "claude", script)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				start := time.Now()
				result, err := Run(ctx, t.TempDir(), "task", false, "", config.ClaudeConfig{})
				want := usage.Tokens{Input: 15, CachedInput: 7, Output: 2}
				if resultFollows {
					want.Output = 11
				}
				if err == nil || !strings.Contains(err.Error(), scenario.failure) || ctx.Err() != nil || result.Tokens != want || result.Answer != "" || result.UsageComplete || time.Since(start) > 5*time.Second {
					t.Fatalf("result=%+v elapsed=%v error=%v parent=%v", result, time.Since(start), err, ctx.Err())
				}
			})
		}
	}
}
