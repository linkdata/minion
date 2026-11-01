package codexcli

import "testing"

func TestRootMCPResultMetadataReplayDoesNotRenewProgress(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for _, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"id":"mcp","type":"mcp_tool_call","result":{"_meta":{"trace":"first"},"content":[{"type":"text","text":"one","annotations":{"priority":1},"_meta":{"trace":"first"}}],"structuredContent":{"state":"same"}}}}`, 1},
		{`{"type":"item.updated","item":{"id":"mcp","type":"mcp_tool_call","result":{"_meta":{"trace":"second"},"content":[{"type":"text","text":"one","annotations":{"priority":2},"_meta":{"trace":"second"}}],"structuredContent":{"state":"same"}}}}`, 1},
		{`{"type":"item.updated","item":{"id":"mcp","type":"mcp_tool_call","result":{"_meta":{"trace":"second"},"content":[{"type":"text","text":"two","annotations":{"priority":2},"_meta":{"trace":"second"}}],"structuredContent":{"state":"same"}}}}`, 2},
		{`{"type":"item.completed","item":{"id":"mcp","type":"mcp_tool_call","result":{"_meta":{"trace":"second"},"content":[{"type":"text","text":"two","annotations":{"priority":2},"_meta":{"trace":"second"}}],"structuredContent":{"state":"same"}}}}`, 3},
	} {
		if _, err := output.Write([]byte(step.line + "\n")); err != nil {
			t.Fatal(err)
		}
		if progress != step.want {
			t.Fatalf("after %s: progress=%d, want %d", step.line, progress, step.want)
		}
	}
}

func TestRootEmbeddedResourceMetadataReplayDoesNotRenewProgress(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"id":"mcp","type":"mcp_tool_call","result":{"content":[{"type":"resource","resource":{"uri":"file:///one","text":"same","_meta":{"trace":"one"}}}]}}}`, 1},
		{`{"type":"item.updated","item":{"id":"mcp","type":"mcp_tool_call","result":{"content":[{"type":"resour\u0063e","resource":{"uri":"file:///one","text":"same","_meta":{"trace":"two"}}}]}}}`, 1},
		{`{"type":"item.updated","item":{"id":"mcp","type":"mcp_tool_call","result":{"content":[{"type":"resource","resource":{"uri":"file:///one","text":"changed","_meta":{"trace":"two"}}}]}}}`, 2},
	} {
		if _, err := output.Write([]byte(step.line + "\n")); err != nil {
			t.Fatal(err)
		}
		if progress != step.want {
			t.Fatalf("step %d: progress=%d, want %d", i, progress, step.want)
		}
	}
}

func TestIDLessRootMCPCallsUseInvocationIdentity(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"repo-a","tool":"lookup","arguments":{"query":"one","limit":2},"result":"same"}}`, 1},
		{`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"repo-a","tool":"lookup","arguments":{"query":"one","limit":2},"result":"same"}}`, 1},
		{`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"repo-a","tool":"lookup","arguments":{"limit":2,"query":"one"},"result":"same"}}`, 1},
		{`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"repo-b","tool":"lookup","arguments":{"query":"one","limit":2},"result":"same"}}`, 2},
		{`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"repo-b","tool":"lookup","arguments":{"query":"two"},"result":"same"}}`, 3},
	} {
		if _, err := output.Write([]byte(step.line + "\n")); err != nil {
			t.Fatal(err)
		}
		if progress != step.want {
			t.Fatalf("step %d: progress=%d, want %d", i, progress, step.want)
		}
	}
}
