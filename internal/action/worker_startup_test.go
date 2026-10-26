package action

import (
	"bytes"
	"encoding/json"
	"os"
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
		t.Fatalf("worker startup roundtrip: selection=%+v work_root=%q socket_path=%q error=%v", decoded.Selection, decoded.WorkRoot, decoded.SocketPath, err)
	}
	runner, err := decoded.runner(nil)
	if err != nil {
		t.Fatal(err)
	}
	if runner.Codex.APIKey != startup.CodexAPIKey || runner.Claude.APIKey != startup.ClaudeAPIKey || runner.selection == nil || runner.WorkRoot != startup.WorkRoot {
		t.Fatal("selected runner lost configured provider credentials, selection, or work root")
	}
}

func TestWorkerStartupNativeCodexCredentials(t *testing.T) {
	startup := testWorkerStartup(t)
	startup.CodexAPIKey = ""
	startup.CodexNativeAPIKey = "private-native-key"
	startup.CodexAccessToken = "private-native-token"
	encoded, err := EncodeWorkerStartup(startup)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWorkerStartup(bytes.NewReader(encoded))
	if err != nil || !reflect.DeepEqual(decoded, startup) {
		t.Fatal("native Codex credentials changed during startup roundtrip")
	}
	runner, err := decoded.runner(nil)
	if err != nil || runner.Codex.APIKey != "" || runner.Codex.NativeAPIKey != startup.CodexNativeAPIKey || runner.Codex.NativeAccessToken != startup.CodexAccessToken {
		t.Fatal("startup runner lost selected native Codex credentials")
	}
}

func TestWorkerStartupAcceptsAbsoluteWorkRootWithTrailingSeparator(t *testing.T) {
	startup := testWorkerStartup(t)
	startup.WorkRoot += string(os.PathSeparator)
	encoded, err := EncodeWorkerStartup(startup)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWorkerStartup(bytes.NewReader(encoded))
	if err != nil || decoded.WorkRoot != startup.WorkRoot {
		t.Fatalf("valid work-root spelling was rejected or changed: root=%q error=%v", decoded.WorkRoot, err)
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
	conflictingCodex := startup
	conflictingCodex.CodexNativeAPIKey = "private-native-key"
	conflictingCodex.CodexAccessToken = "private-native-token"
	conflictingCodexBytes, err := json.Marshal(conflictingCodex)
	if err != nil {
		t.Fatal(err)
	}
	invalidNative := startup
	invalidNative.CodexAPIKey = ""
	invalidNative.CodexNativeAPIKey = "private key"
	invalidNative.CodexAccessToken = "private-native-token"
	invalidNativeBytes, err := json.Marshal(invalidNative)
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
		{"conflicting Codex auth", conflictingCodexBytes},
		{"invalid native Codex auth", invalidNativeBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeWorkerStartup(bytes.NewReader(test.data))
			if err == nil {
				t.Fatal("invalid startup accepted")
			}
			for _, secret := range []string{startup.CodexAPIKey, startup.ClaudeAPIKey, conflictingCodex.CodexNativeAPIKey, invalidNative.CodexNativeAPIKey, invalidNative.CodexAccessToken} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("invalid startup error leaked a credential")
				}
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
		{"unused native Codex key", func(s *WorkerStartup) {
			s.Selection.Main.Provider, s.Selection.Critic = "claude", nil
			s.CodexAPIKey = ""
			s.CodexNativeAPIKey = "private-native-key"
		}},
		{"conflicting Codex auth", func(s *WorkerStartup) { s.CodexNativeAPIKey = "private-native-key" }},
		{"invalid native Codex auth", func(s *WorkerStartup) { s.CodexAPIKey, s.CodexNativeAPIKey = "", "private key" }},
		{"oversized", func(s *WorkerStartup) { s.CodexAPIKey = strings.Repeat("x", workerStartupBytes) }},
		{"aggregate provider keys", func(s *WorkerStartup) {
			key := strings.Repeat("x", workerStartupBytes/2)
			s.CodexAPIKey, s.ClaudeAPIKey = key, key
		}},
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
