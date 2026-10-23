package action

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHTTPShapeTextByteBounds(t *testing.T) {
	for _, field := range []struct {
		name     string
		limit    int
		validate func(string) error
	}{
		{"job ID", 100, func(s string) error { return httpBindingShape(httpBinding{Identity: Identity{JobID: s}}) }},
		{"attempt ID", 100, func(s string) error { return httpBindingShape(httpBinding{Identity: Identity{AttemptID: s}}) }},
		{"executor ID", 100, func(s string) error { return httpBindingShape(httpBinding{Identity: Identity{ExecutorID: s}}) }},
		{"repository", 201, func(s string) error { return httpBindingShape(httpBinding{Identity: Identity{Repository: s}}) }},
		{"requester", 105, func(s string) error { return httpBindingShape(httpBinding{Identity: Identity{Requester: s}}) }},
		{"subject type", 11, func(s string) error { return httpBindingShape(httpBinding{Identity: Identity{SubjectType: s}}) }},
		{"source", 40, func(s string) error { return httpBindingShape(httpBinding{SourceRef: s}) }},
		{"provider", config.MaxModelSettingBytes, func(s string) error { return httpCallShape(CallUsage{Provider: s}) }},
		{"role", config.MaxModelSettingBytes, func(s string) error { return httpCallShape(CallUsage{Role: s}) }},
		{"review phase", config.MaxModelSettingBytes, func(s string) error { return httpCallShape(CallUsage{ReviewPhase: s}) }},
		{"model", config.MaxModelSettingBytes, func(s string) error { return httpCallShape(CallUsage{Model: s}) }},
		{"effort", config.MaxModelSettingBytes, func(s string) error { return httpCallShape(CallUsage{Effort: s}) }},
		{"decision", 32, func(s string) error { return httpReviewShape(ReviewCheckpoint{CriticStopReason: s}) }},
		{"refutation", maxOutputBytes, func(s string) error { return httpReviewShape(ReviewCheckpoint{Refutations: []Refutation{{Text: s}}}) }},
		{"answer", maxOutputBytes, func(s string) error { return httpResultShape(Result{Answer: s}) }},
		{"change path", workspace.MaxPathBytes, func(s string) error { return httpResultShape(Result{Changes: []workspace.Change{{Path: s}}}) }},
		{"artifact path", workspace.MaxPathBytes, func(s string) error { return httpResultShape(Result{Artifacts: []workspace.File{{Path: s}}}) }},
		{"diagnostic code", 100, func(s string) error { return httpResultShape(Result{Diagnostics: []Diagnostic{{Code: s}}}) }},
		{"diagnostic message", maxDiagnosticMessageBytes, func(s string) error { return httpResultShape(Result{Diagnostics: []Diagnostic{{Message: s}}}) }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, test := range []struct {
				name  string
				value string
				valid bool
			}{
				{"empty", "", true},
				{"at byte limit", strings.Repeat("\x01", field.limit), true},
				{"UTF-8 at byte limit", strings.Repeat("é", field.limit/2) + strings.Repeat("x", field.limit%2), true},
				{"limit plus one", strings.Repeat("x", field.limit+1), false},
				{"invalid UTF-8", strings.Repeat("x", field.limit-1) + "\xff", false},
				{"NUL", strings.Repeat("x", field.limit-1) + "\x00", field.name == "refutation"},
			} {
				t.Run(test.name, func(t *testing.T) {
					err := field.validate(test.value)
					if (err == nil) != test.valid || err != nil && !errors.Is(err, errHTTPShape) {
						t.Fatalf("text byte bound changed: bytes=%d valid=%t error=%v", len(test.value), test.valid, err)
					}
				})
			}
		})
	}
}

func TestHTTPPrivateRefutationControlsMatchLocalContract(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations = []Refutation{{Round: 2, Text: "Private refutation\x00\n\t\"quoted\"\\path"}}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("fixture violates the local private-refutation contract: %v", err)
	}
	review := ReviewCheckpoint{Version: result.Version, Identity: result.Identity, SourceRef: result.SourceRef, Calls: result.Calls, Refutations: result.Refutations, CriticStopReason: result.CriticStopReason}
	if err := httpReviewShape(review); err != nil {
		t.Fatalf("HTTP rejected locally valid private review controls: %v", err)
	}
	if err := httpResultShape(result); err != nil {
		t.Fatalf("HTTP rejected locally valid private Result controls: %v", err)
	}
	encoded, err := httpEncode(review, httpReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReview ReviewCheckpoint
	if err := httpDecode(bytes.NewReader(encoded), httpReviewBytes, &decodedReview); err != nil || !reflect.DeepEqual(decodedReview, review) {
		t.Fatalf("private Review bytes changed on the wire: got=%+v error=%v", decodedReview, err)
	}
	encoded, err = httpEncode(result, httpOutcomeBytes)
	if err != nil {
		t.Fatal(err)
	}
	var decodedResult Result
	if err := httpDecode(bytes.NewReader(encoded), httpOutcomeBytes, &decodedResult); err != nil || !reflect.DeepEqual(decodedResult, result) || decodedResult.validate(job, false) != nil {
		t.Fatalf("private Result bytes changed on the wire: got=%+v error=%v", decodedResult, err)
	}
}

func TestHTTPShapeCountAndDataBounds(t *testing.T) {
	for _, test := range []struct {
		name     string
		valid    bool
		validate func() error
	}{
		{"calls at limit", true, func() error {
			return httpReviewShape(ReviewCheckpoint{Calls: make([]CallUsage, 3*config.MaxCriticMaxRounds-1)})
		}},
		{"calls over limit", false, func() error {
			return httpReviewShape(ReviewCheckpoint{Calls: make([]CallUsage, 3*config.MaxCriticMaxRounds)})
		}},
		{"refutations at limit", true, func() error {
			return httpReviewShape(ReviewCheckpoint{Refutations: make([]Refutation, config.MaxCriticMaxRounds-1)})
		}},
		{"refutations over limit", false, func() error {
			return httpReviewShape(ReviewCheckpoint{Refutations: make([]Refutation, config.MaxCriticMaxRounds)})
		}},
		{"changes at limit", true, func() error { return httpResultShape(Result{Changes: make([]workspace.Change, workspace.MaxChanges)}) }},
		{"changes over limit", false, func() error {
			return httpResultShape(Result{Changes: make([]workspace.Change, workspace.MaxChanges+1)})
		}},
		{"change data at limit", true, func() error {
			return httpResultShape(Result{Changes: []workspace.Change{{Data: make([]byte, workspace.MaxChangeBytes)}}})
		}},
		{"change data over limit", false, func() error {
			return httpResultShape(Result{Changes: []workspace.Change{{Data: make([]byte, workspace.MaxChangeBytes+1)}}})
		}},
		{"combined change data at limit", true, func() error {
			data := make([]byte, workspace.MaxChangeBytes/2)
			return httpResultShape(Result{Changes: []workspace.Change{{Data: data}, {Data: data}}})
		}},
		{"combined change data over limit", false, func() error {
			data := make([]byte, workspace.MaxChangeBytes/2+1)
			return httpResultShape(Result{Changes: []workspace.Change{{Data: data}, {Data: data}}})
		}},
		{"artifacts at limit", true, func() error { return httpResultShape(Result{Artifacts: make([]workspace.File, len(auditArtifacts))}) }},
		{"artifacts over limit", false, func() error { return httpResultShape(Result{Artifacts: make([]workspace.File, len(auditArtifacts)+1)}) }},
		{"artifact data at limit", true, func() error {
			return httpResultShape(Result{Artifacts: []workspace.File{{Data: make([]byte, maxAuditArtifactBytes)}}})
		}},
		{"artifact data over limit", false, func() error {
			return httpResultShape(Result{Artifacts: []workspace.File{{Data: make([]byte, maxAuditArtifactBytes+1)}}})
		}},
		{"diagnostics at limit", true, func() error { return httpResultShape(Result{Diagnostics: make([]Diagnostic, maxDiagnostics)}) }},
		{"diagnostics over limit", false, func() error { return httpResultShape(Result{Diagnostics: make([]Diagnostic, maxDiagnostics+1)}) }},
		{"nested call bound", false, func() error {
			return httpReviewShape(ReviewCheckpoint{Calls: []CallUsage{{Model: strings.Repeat("x", config.MaxModelSettingBytes+1)}}})
		}},
		{"nested binding bound", false, func() error {
			return httpReviewShape(ReviewCheckpoint{Identity: Identity{JobID: strings.Repeat("x", 101)}})
		}},
		{"result nested review bound", false, func() error { return httpResultShape(Result{Calls: make([]CallUsage, 3*config.MaxCriticMaxRounds)}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate()
			if (err == nil) != test.valid || err != nil && !errors.Is(err, errHTTPShape) {
				t.Fatalf("count/data bound changed: valid=%t error=%v", test.valid, err)
			}
		})
	}
}

func TestHTTPShapeAndWirePreserveRawFailureFacts(t *testing.T) {
	for _, tokens := range []usage.Tokens{
		{Input: -1, CachedInput: -2, Output: -3},
		{Input: 1, CachedInput: 2, Output: 3},
		{Input: math.MaxInt64, CachedInput: math.MaxInt64, Output: math.MaxInt64},
	} {
		call := CallUsage{Provider: "unrecognized", Role: roleCritic, Round: -7, ReviewPhase: "unrecognized", Model: "pinned", Effort: "high",
			Tokens: tokens, Approved: true, NoBlockers: true, HasBlockers: true, ReviewComplete: false, Complete: false}
		if tokens.Valid() || httpCallShape(call) != nil {
			t.Fatalf("finite failed receipt rejected before accounting: %+v", call)
		}
		result := Result{Version: -1, Identity: Identity{RepositoryID: -2, OwnerID: -3, RequesterID: -4, Number: -5}, SourceRef: "not-a-sha",
			Calls: []CallUsage{call}, Refutations: []Refutation{{Round: -1, Text: "Private counterevidence"}}, CriticStopReason: "unrecognized",
			Changes:   []workspace.Change{{Path: "../invalid", Mode: math.MaxUint32, Delete: true, Data: []byte{0, 0xff}}},
			Artifacts: []workspace.File{{Path: "unrecognized", Mode: math.MaxUint32, Data: []byte{0xff, 0}}}}
		if err := httpResultShape(result); err != nil {
			t.Fatalf("shape gate performed semantic acceptance: %v", err)
		}
		encoded, err := httpEncode(result, httpOutcomeBytes)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Result
		if err := httpDecode(bytes.NewReader(encoded), httpOutcomeBytes, &decoded); err != nil || !reflect.DeepEqual(decoded, result) {
			t.Fatalf("raw failure facts changed on wire: got=%+v want=%+v error=%v", decoded, result, err)
		}
	}
}
