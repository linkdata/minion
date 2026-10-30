package action

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
)

type preparedInputPayload struct {
	Representation string   `json:"representation"`
	Stage          string   `json:"stage"`
	Mode           string   `json:"mode"`
	Path           string   `json:"path"`
	Argv           []string `json:"argv"`
	Dir            string   `json:"dir"`
	Stdin          string   `json:"stdin"`
	Schema         string   `json:"schema"`
}

func preparedInputText(input aicli.PreparedInput, secrets ...string) (string, string) {
	if input.Mode != "fresh" && input.Mode != "resume" || input.Path == "" || input.Dir == "" || len(input.Args) == 0 {
		return "", "invalid_input"
	}
	size := int64(len(input.Path)) + int64(len(input.Dir)) + int64(len(input.Stdin)) + int64(len(input.Schema))
	for _, arg := range input.Args {
		size += int64(len(arg))
	}
	if size > maxAttemptEventBytes {
		return "", "event_limit"
	}
	for _, value := range []string{input.NativeID, input.Mode, input.Path, input.Dir, input.Stdin, input.Schema} {
		if !utf8.ValidString(value) {
			return "", "invalid_utf8"
		}
	}
	for _, arg := range input.Args {
		if !utf8.ValidString(arg) {
			return "", "invalid_utf8"
		}
	}
	payload := preparedInputPayload{Representation: "redacted", Stage: "prepared", Mode: input.Mode,
		Path: input.Path, Argv: input.Args, Dir: input.Dir, Stdin: input.Stdin, Schema: input.Schema}
	data, err := json.Marshal(payload)
	if err != nil || len(data) > maxAttemptEventBytes {
		return "", "event_limit"
	}
	text := redactActionText(string(data), secrets...)
	if text == "" || len(text) > maxAttemptEventBytes {
		return "", "redaction_unavailable"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil || len(fields) != 8 {
		return "", "redaction_unavailable"
	}
	for _, name := range []string{"representation", "stage", "mode", "path", "dir", "stdin", "schema"} {
		value, ok := fields[name]
		if !ok || len(value) == 0 || value[0] != '"' {
			return "", "redaction_unavailable"
		}
	}
	if value, ok := fields["argv"]; !ok || len(value) == 0 || value[0] != '[' {
		return "", "redaction_unavailable"
	}
	var represented preparedInputPayload
	if err := json.Unmarshal([]byte(text), &represented); err != nil || represented.Representation != "redacted" ||
		represented.Stage != "prepared" || represented.Mode != input.Mode || len(represented.Argv) != len(input.Args) {
		return "", "redaction_unavailable"
	}
	for _, secret := range secrets {
		if secret != "" && containsConfiguredKey([]byte(text), secret) {
			return "", "redaction_unavailable"
		}
	}
	return text, ""
}

func unavailableInputText(reason string) string {
	data, _ := json.Marshal(struct {
		Representation string `json:"representation"`
		Stage          string `json:"stage"`
		Available      bool   `json:"available"`
		Reason         string `json:"reason"`
	}{Representation: "redacted", Stage: "prepared", Reason: reason})
	return string(data)
}
