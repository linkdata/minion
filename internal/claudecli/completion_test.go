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

func TestNewAssistantParentageCannotPreserveStaleResult(t *testing.T) {
	for _, parent := range []string{"", `null`, `"delegate"`, `""`, `0`, `false`, `{}`, `[]`} {
		for _, stopped := range []bool{false, true} {
			for _, freshResult := range []bool{false, true} {
				name := fmt.Sprintf("parent=%s/stopped=%t/result=%t", parent, stopped, freshResult)
				t.Run(name, func(t *testing.T) {
					data := successfulResult + "\n"
					if stopped {
						data += `{"type":"system","subtype":"task_notification","status":"stopped"}` + "\n"
					}
					field := ""
					if parent != "" {
						field = `,"parent_tool_use_id":` + parent
					}
					data += `{"type":"assistant"` + field + `,"message":{"id":"new"}}` + "\n"
					if freshResult {
						data += successfulResult + "\n"
					}
					o := jsonlOutput{}
					writeChunks(t, &o, numberResultEvents(data))
					o.finish()
					wantComplete := (!stopped && (freshResult || parent == `"delegate"`)) || (stopped && parent == `null` && freshResult)
					if o.completed != wantComplete || (o.err == nil) != wantComplete || !wantComplete && o.result.Answer != "" || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
						t.Fatalf("stale completion or lost usage: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
					}
				})
			}
		}
	}
}

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
	const pressureSummary = `Background command "tests" was stopped because the system is running low on memory`
	pressureNotice, _ := json.Marshal(`<task-notification><task-id>worker</task-id><status>killed</status><summary>Background command &quot;tests&quot; was stopped because the system is running low on memory</summary></task-notification>`)
	pressureSystem := fmt.Sprintf(`{"type":"system","subtype":"task_notification","status":"stopped","summary":%q}`, pressureSummary)
	pressureSynthetic := `{"type":"user","isSynthetic":true,"message":{"content":` + string(pressureNotice) + `}}`
	later := strings.Replace(successfulResult, `"outputTokens":11`, `"outputTokens":22`, 1)
	for _, tc := range []struct {
		name, after string
		fails       bool
		failure     string
		output      int64
	}{
		{"wind down then held waiting result", pending + "\n" + stop + "\n" + clear + "\n" + successfulResult, true, "", 11},
		{"stop after final", stop, true, "", 11},
		{"new result without root response", stop + "\n" + successfulResult, true, "", 11},
		{"repeated old root ID", stop + "\n" + old + "\n" + successfulResult, true, "", 11},
		{"unknown parent cannot assess stop", stop + "\n" + strings.Replace(fresh, `"parent_tool_use_id":null,`, "", 1) + "\n" + successfulResult, true, "", 11},
		{"delegate cannot assess stop", stop + "\n" + strings.Replace(fresh, `"parent_tool_use_id":null`, `"parent_tool_use_id":"parent"`, 1) + "\n" + successfulResult, true, "", 11},
		{"new root without final result", stop + "\n" + fresh, true, "", 11},
		{"new root assessment and final", stop + "\n" + fresh + "\n" + successfulResult, false, "", 11},
		{"memory pressure remains fatal after new root", strings.Replace(stop, `"summary":"Background command tests was stopped"`, `"summary":"Background command \"tests\" was stopped because the system is running low on memory"`, 1) + "\n" + fresh + "\n" + successfulResult, true, "provider memory pressure", 11},
		{"ambient stop after final", strings.Replace(stop, `"status":"stopped"`, `"status":"stopped","ambient":true`, 1), false, "", 11},
		{"synthetic killed string", synthetic + "\n" + successfulResult, true, "", 11},
		{"synthetic stopped block", `{"type":"user","isSynthetic":true,"message":{"content":[{"type":"text","text":` + strings.Replace(string(notice), "killed", "stopped", 1) + `}]}}` + "\n" + successfulResult, true, "", 11},
		{"synthetic stop assessed", synthetic + "\n" + fresh + "\n" + successfulResult, false, "", 11},
		{"raw ampersand notice", noticeEvent(`Background command "go test && go vet" was stopped`), true, "", 11},
		{"raw less-than notice", noticeEvent(`Background command "grep x <file" was stopped`), true, "", 11},
		{"entity-escaped notice", noticeEvent(`Background command &quot;go test &amp;&amp; go vet&quot; was stopped`), true, "", 11},
		{"killed system event", strings.Replace(stop, `"status":"stopped"`, `"status":"killed"`, 1), true, "", 11},
		{"raw memory-pressure summary", noticeEvent(`Background command "go test && go vet" was stopped because the system is running low on memory`) + "\n" + fresh + "\n" + successfulResult, true, "provider memory pressure", 11},
		{"literal closing summary tag in command", noticeEvent(`Background command "echo </summary> && go vet" was stopped because the system is running low on memory`) + "\n" + fresh + "\n" + successfulResult, true, "provider memory pressure", 11},
		{"ordinary user quote", strings.Replace(synthetic, `"isSynthetic":true`, `"isSynthetic":false`, 1), false, "", 11},
		{"ordinary failed command assessed", strings.Replace(stop, `"status":"stopped"`, `"status":"failed"`, 1), false, "", 11},
		{"memory pressure/native system", pressureSystem + "\n" + later, true, "provider memory pressure", 22},
		{"memory pressure/native synthetic string", pressureSynthetic + "\n" + later, true, "provider memory pressure", 22},
		{"memory pressure/native synthetic text block", `{"type":"user","isSynthetic":true,"message":{"content":[{"type":"text","text":` + string(pressureNotice) + `}]}}` + "\n" + later, true, "provider memory pressure", 22},
		{"memory pressure/ordinary user quote", strings.Replace(pressureSynthetic, `"isSynthetic":true`, `"isSynthetic":false`, 1) + "\n" + later, false, "", 22},
		{"memory pressure/ambient server stopped", `{"type":"system","subtype":"task_notification","status":"stopped","ambient":true,"summary":"Background server deliberately stopped"}` + "\n" + later, false, "", 22},
		{"memory pressure/ordinary completed task", strings.Replace(pressureSystem, `"stopped"`, `"completed"`, 1) + "\n" + later, false, "", 22},
		{"memory pressure/unrelated assistant summary", strings.Replace(pressureSystem, `"type":"system"`, `"type":"assistant"`, 1) + "\n" + later, false, "", 22},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			data := numberResultEvents(old+"\n"+successfulResult+"\n"+tc.after) + "\n"
			writeChunks(t, &o, data)
			o.finish()
			if tc.failure != "" && (o.err == nil || !strings.Contains(o.err.Error(), tc.failure)) {
				t.Fatalf("lost native failure %q: %v", tc.failure, o.err)
			}
			if (o.err != nil) != tc.fails || o.completed == tc.fails || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: tc.output}) {
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
