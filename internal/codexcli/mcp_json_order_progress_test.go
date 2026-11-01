package codexcli

import "testing"

func TestRootMCPResultObjectOrderReplayDoesNotRenewProgress(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"id":"mcp","type":"mcp_tool_call","result":{"content":[{"type":"text","text":"same"}],"structuredContent":{"nested":{"first":1,"second":2}}}}}`, 1},
		{`{"type":"item.updated","item":{"id":"mcp","type":"mcp_tool_call","result":{"structuredContent":{"nested":{"second":2,"first":1}},"content":[{"text":"same","type":"text"}]}}}`, 1},
		{`{"type":"item.updated","item":{"id":"mcp","type":"mcp_tool_call","result":{"content":[{"type":"text","text":"same"}],"structuredContent":{"nested":{"first":1,"second":3}}}}}`, 2},
	} {
		if _, err := output.Write([]byte(step.line + "\n")); err != nil {
			t.Fatal(err)
		}
		if progress != step.want {
			t.Fatalf("step %d: progress=%d, want %d", i, progress, step.want)
		}
	}
}

func TestHelperMCPResultObjectOrderReplayDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		apply   func(*rolloutActivity, string, *bool)
		first   string
		replay  string
		changed string
	}{
		{
			name: "completed item",
			apply: func(a *rolloutActivity, raw string, progressed *bool) {
				a.newItemProgress("child", "current", []byte(raw), progressed)
			},
			first:   `{"type":"McpToolCall","id":"call","status":"completed","result":{"content":[{"type":"text","text":"same"}],"structuredContent":{"nested":{"first":1,"second":2}}}}`,
			replay:  `{"type":"McpToolCall","id":"call","status":"completed","result":{"structuredContent":{"nested":{"second":2,"first":1}},"content":[{"text":"same","type":"text"}]}}`,
			changed: `{"type":"McpToolCall","id":"call","status":"completed","result":{"content":[{"type":"text","text":"same"}],"structuredContent":{"nested":{"first":1,"second":3}}}}`,
		},
		{
			name: "tool event",
			apply: func(a *rolloutActivity, raw string, progressed *bool) {
				a.newToolProgress("child", []byte(raw), progressed)
			},
			first:   `{"type":"mcp_tool_call_end","turn_id":"current","call_id":"call","result":{"Ok":{"content":[{"type":"text","text":"same"}],"structuredContent":{"nested":{"first":1,"second":2}}}}}`,
			replay:  `{"type":"mcp_tool_call_end","turn_id":"current","call_id":"call","result":{"Ok":{"structuredContent":{"nested":{"second":2,"first":1}},"content":[{"text":"same","type":"text"}]}}}`,
			changed: `{"type":"mcp_tool_call_end","turn_id":"current","call_id":"call","result":{"Ok":{"content":[{"type":"text","text":"same"}],"structuredContent":{"nested":{"first":1,"second":3}}}}}`,
		},
		{
			name: "tool invocation",
			apply: func(a *rolloutActivity, raw string, progressed *bool) {
				a.newToolProgress("child", []byte(raw), progressed)
			},
			first:   `{"type":"mcp_tool_call_end","turn_id":"current","call_id":"call","invocation":{"server":"repo","tool":"search","arguments":{"filter":{"first":1,"second":2}}},"result":"same"}`,
			replay:  `{"type":"mcp_tool_call_end","turn_id":"current","call_id":"call","invocation":{"server":"repo","tool":"search","arguments":{"filter":{"second":2,"first":1}}},"result":"same"}`,
			changed: `{"type":"mcp_tool_call_end","turn_id":"current","call_id":"call","invocation":{"server":"repo","tool":"search","arguments":{"filter":{"first":1,"second":3}}},"result":"same"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &rolloutActivity{progress: func() {}}
			for i, step := range []struct {
				raw  string
				want bool
			}{{tc.first, true}, {tc.replay, false}, {tc.changed, true}} {
				progressed := false
				tc.apply(a, step.raw, &progressed)
				if progressed != step.want {
					t.Fatalf("step %d: progressed=%t, want %t", i, progressed, step.want)
				}
			}
		})
	}
}

func TestHelperResponseItemObjectOrderReplayDoesNotRenewProgress(t *testing.T) {
	a := &rolloutActivity{progress: func() {}}
	for i, step := range []struct {
		raw  string
		want bool
	}{
		{`{"type":"tool_search_output","id":"search","results":[{"tool":{"name":"search","parameters":{"first":1,"second":2}}}]}`, true},
		{`{"type":"tool_search_output","id":"search","results":[{"tool":{"parameters":{"second":2,"first":1},"name":"search"}}]}`, false},
		{`{"type":"tool_search_output","id":"search","results":[{"tool":{"name":"search","parameters":{"first":1,"second":3}}}]}`, true},
	} {
		progressed := false
		a.newResponseProgress("child", []byte(step.raw), &progressed)
		if progressed != step.want {
			t.Fatalf("step %d: progressed=%t, want %t", i, progressed, step.want)
		}
	}
}
