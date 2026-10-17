package usage

import (
	"math"
	"strings"
	"testing"
)

func TestSummarizeAggregateBounds(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		first, second Tokens
		codex, claude [2]int
		wantTokens    int64
		wantCodex     int
		wantClaude    int
		wantError     string
	}{
		{name: "exact token maximum", first: Tokens{Input: math.MaxInt64 / 2}, second: Tokens{Input: math.MaxInt64/2 + 1}, wantTokens: math.MaxInt64},
		{name: "token maximum plus one", first: Tokens{Input: math.MaxInt64/2 + 1}, second: Tokens{Input: math.MaxInt64/2 + 1}, wantError: "token counts"},
		{name: "total overflow without component overflow", first: Tokens{Input: math.MaxInt64}, second: Tokens{Output: 1}, wantError: "token counts"},
		{name: "invalid tokens", second: Tokens{Input: -1}, wantError: "token counts"},
		{name: "exact codex call maximum", codex: [2]int{math.MaxInt / 2, math.MaxInt/2 + 1}, wantCodex: math.MaxInt},
		{name: "codex call overflow", codex: [2]int{math.MaxInt, 1}, wantError: "call counts"},
		{name: "negative codex calls", codex: [2]int{0, -1}, wantError: "call counts"},
		{name: "exact claude call maximum", claude: [2]int{math.MaxInt / 2, math.MaxInt/2 + 1}, wantClaude: math.MaxInt},
		{name: "claude call overflow", claude: [2]int{math.MaxInt, 1}, wantError: "call counts"},
		{name: "negative claude calls", claude: [2]int{0, -1}, wantError: "call counts"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			first, second := testRecord(), testRecord()
			first.Tokens, second.Tokens = scenario.first, scenario.second
			first.CodexCalls, second.CodexCalls = scenario.codex[0], scenario.codex[1]
			first.ClaudeCalls, second.ClaudeCalls = scenario.claude[0], scenario.claude[1]
			groups, err := Summarize([]Record{first, second}, Filter{}, nil)
			if scenario.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), scenario.wantError) || groups != nil {
					t.Fatalf("invalid aggregate emitted: groups=%+v err=%v", groups, err)
				}
				// A filtered-out record cannot poison an otherwise representable report.
				second.User = "excluded"
				groups, err = Summarize([]Record{first, second}, Filter{User: first.User}, nil)
				if err != nil || len(groups) != 1 || groups[0].Tokens != first.Tokens || groups[0].CodexCalls != first.CodexCalls || groups[0].ClaudeCalls != first.ClaudeCalls {
					t.Fatalf("excluded record poisoned report: groups=%+v err=%v", groups, err)
				}
				return
			}
			if err != nil || len(groups) != 1 || groups[0].TotalTokens != scenario.wantTokens || groups[0].CodexCalls != scenario.wantCodex || groups[0].ClaudeCalls != scenario.wantClaude || !groups[0].Tokens.Valid() {
				t.Fatalf("exact aggregate boundary failed: groups=%+v err=%v", groups, err)
			}
		})
	}
}
