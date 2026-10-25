package action

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
)

func testWorkerStartup(t *testing.T) WorkerStartup {
	t.Helper()
	return WorkerStartup{
		Selection: WorkerSelection{
			Commands: map[string]config.CommandMapping{
				"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
			},
			Main:   RoleSettings{Provider: "codex", Model: "main-model", Effort: "high"},
			Critic: &RoleSettings{Provider: "claude", Model: "critic-model", Effort: "medium"},
		},
		WorkRoot:     t.TempDir(),
		SocketPath:   filepath.Join(t.TempDir(), "execution.sock"),
		CodexAPIKey:  "private-codex-key",
		ClaudeAPIKey: "private-claude-key",
	}
}

func TestWorkerStartupRoundTripAndProviderKeys(t *testing.T) {
	startup := testWorkerStartup(t)
	encoded, err := EncodeWorkerStartup(startup)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("github_token")) {
		t.Fatal("GitHub credential appeared in worker startup schema")
	}
	decoded, err := decodeWorkerStartup(bytes.NewReader(encoded))
	if err != nil || !reflect.DeepEqual(decoded, startup) {
		t.Fatalf("worker startup roundtrip: decoded=%+v error=%v", decoded, err)
	}
	runner, err := decoded.runner(nil)
	if err != nil {
		t.Fatal(err)
	}
	if runner.Codex.APIKey != startup.CodexAPIKey || runner.Claude.APIKey != startup.ClaudeAPIKey || runner.selection == nil || runner.WorkRoot != startup.WorkRoot {
		t.Fatal("selected runner lost configured provider credentials, selection, or work root")
	}
}

func TestDecodeWorkerStartupRejectsInvalidInput(t *testing.T) {
	startup := testWorkerStartup(t)
	encoded, err := EncodeWorkerStartup(startup)
	if err != nil {
		t.Fatal(err)
	}
	unselected := startup
	unselected.Selection = WorkerSelection{}
	unselectedBytes, err := json.Marshal(unselected)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := startup
	incomplete.WorkRoot = ""
	incompleteBytes, err := json.Marshal(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	unusedKey := startup
	unusedKey.Selection.Critic = nil
	unusedKeyBytes, err := json.Marshal(unusedKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"truncated", encoded[:len(encoded)-2]},
		{"unknown field", bytes.Replace(encoded, []byte(`"selection":`), []byte(`"github_token":"leak","selection":`), 1)},
		{"unknown nested field", bytes.Replace(encoded, []byte(`"commands":`), []byte(`"unknown":true,"commands":`), 1)},
		{"trailing value", append(bytes.Clone(encoded), []byte("{}")...)},
		{"oversized", bytes.Repeat([]byte{' '}, workerStartupBytes+1)},
		{"invalid UTF-8", append(bytes.Clone(encoded), 0xff)},
		{"unselected", unselectedBytes},
		{"incomplete", incompleteBytes},
		{"unused provider key", unusedKeyBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeWorkerStartup(bytes.NewReader(test.data)); err == nil || strings.Contains(err.Error(), startup.CodexAPIKey) || strings.Contains(err.Error(), startup.ClaudeAPIKey) {
				t.Fatalf("invalid startup accepted or leaked a provider key: %v", err)
			}
		})
	}
	if _, err := decodeWorkerStartup(nil); err == nil {
		t.Fatal("nil startup input accepted")
	}
}

func TestEncodeWorkerStartupRejectsInvalidInput(t *testing.T) {
	startup := testWorkerStartup(t)
	for _, test := range []struct {
		name   string
		change func(*WorkerStartup)
	}{
		{"relative work root", func(s *WorkerStartup) { s.WorkRoot = "relative" }},
		{"missing endpoint", func(s *WorkerStartup) { s.SocketPath = "" }},
		{"unselected", func(s *WorkerStartup) { s.Selection = WorkerSelection{} }},
		{"unused provider key", func(s *WorkerStartup) { s.Selection.Critic = nil }},
		{"oversized", func(s *WorkerStartup) { s.CodexAPIKey = strings.Repeat("x", workerStartupBytes) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := startup
			test.change(&invalid)
			if _, err := EncodeWorkerStartup(invalid); err == nil {
				t.Fatal("invalid worker startup encoded")
			}
		})
	}
}
