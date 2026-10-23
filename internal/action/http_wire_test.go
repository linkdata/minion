package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHTTPJSONEncodedLimitIncludesEscapingPaddingAndNewline(t *testing.T) {
	type message struct {
		Text string `json:"text"`
		Data []byte `json:"data"`
	}
	for _, test := range []struct {
		name  string
		value message
	}{
		{"empty", message{}},
		{"one byte base64", message{Data: []byte{0}}},
		{"two byte base64", message{Data: []byte{0, 1}}},
		{"three byte base64", message{Data: []byte{0, 1, 2}}},
		{"four byte base64", message{Data: []byte{0, 1, 2, 3}}},
		{"escaped text", message{Text: "\x00\x01<&>\"\\\n\u2028\u2029"}},
		{"UTF-8", message{Text: "å界"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')
			encoded, err := httpEncode(test.value, len(want))
			if err != nil || !bytes.Equal(encoded, want) {
				t.Fatalf("encoded boundary changed: got=%q want=%q error=%v", encoded, want, err)
			}
			if encoded, err := httpEncode(test.value, len(want)-1); !errors.Is(err, errHTTPBodyLimit) || encoded != nil {
				t.Fatalf("oversized encoded body exposed: body=%q error=%v", encoded, err)
			}
			var decoded message
			if err := httpDecode(bytes.NewReader(want), len(want), &decoded); err != nil || decoded.Text != test.value.Text || !bytes.Equal(decoded.Data, test.value.Data) {
				t.Fatalf("exact-limit body refused or changed: got=%+v error=%v", decoded, err)
			}
			if err := httpDecode(bytes.NewReader(want), len(want)-1, &decoded); !errors.Is(err, errHTTPBodyLimit) {
				t.Fatalf("limit+1 encoded body accepted: %v", err)
			}
			if err := httpDecode(bytes.NewReader(append(want, ' ')), len(want), &decoded); !errors.Is(err, errHTTPBodyLimit) {
				t.Fatalf("trailing whitespace bypassed encoded limit: %v", err)
			}
		})
	}
}

func TestHTTPDecodeRejectsInvalidSingleJSONBodies(t *testing.T) {
	for _, body := range []string{
		"", " ", "{", `{"text":`, `{"unknown":true}`, `{"text":1}`,
		`{} {}`, `{} null`, `{} false`, `{} garbage`,
		"{\"text\":\"" + string([]byte{0xff}) + "\"}",
		"{}" + string([]byte{0xff}),
	} {
		t.Run(fmt.Sprintf("%q", body), func(t *testing.T) {
			var value struct {
				Text string `json:"text"`
			}
			if err := httpDecode(strings.NewReader(body), len(body), &value); err == nil {
				t.Fatal("invalid body accepted")
			}
		})
	}
	var nested struct {
		Child struct{} `json:"child"`
	}
	body := `{"child":{"unknown":true}}`
	if err := httpDecode(strings.NewReader(body), len(body), &nested); err == nil {
		t.Fatal("nested unknown field accepted")
	}
}

func TestHTTPJSONRejectsReadAndEncodingFailures(t *testing.T) {
	readErr := errors.New("fixture body read failed")
	reader := io.MultiReader(strings.NewReader(`{}`), iotest.ErrReader(readErr))
	var value struct{}
	if err := httpDecode(reader, 16, &value); !errors.Is(err, readErr) {
		t.Fatalf("valid prefix hid body read failure: %v", err)
	}
	oversized := strings.NewReader(strings.Repeat(" ", 32))
	if err := httpDecode(oversized, 7, &value); !errors.Is(err, errHTTPBodyLimit) || oversized.Len() != 24 {
		t.Fatalf("decoder did not stop at limit+1: remaining=%d error=%v", oversized.Len(), err)
	}
	if encoded, err := httpEncode(make(chan int), 64); err == nil || encoded != nil {
		t.Fatalf("unsupported value exposed an encoded body: body=%q error=%v", encoded, err)
	}
	if _, err := httpEncode(value, -1); !errors.Is(err, errHTTPBodyLimit) {
		t.Fatalf("negative encoding limit accepted: %v", err)
	}
	if err := httpDecode(strings.NewReader(`{}`), -1, &value); !errors.Is(err, errHTTPBodyLimit) {
		t.Fatalf("negative decoding limit accepted: %v", err)
	}
}

func TestHTTPDecodeBoundsContainersBeforeTypedAllocation(t *testing.T) {
	for _, body := range []string{
		"[" + strings.Repeat("{},", 100_000) + "{}]",
		strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65),
	} {
		var value any
		if err := httpDecode(strings.NewReader(body), len(body), &value); !errors.Is(err, errHTTPComplexity) || value != nil {
			t.Fatalf("container limit allocated destination or failed open: value=%T error=%v", value, err)
		}
	}
}

func TestHTTPJobBoundCoversEncodedJobAndAssignmentEnvelope(t *testing.T) {
	for _, shape := range []string{"comment", "comparison", "roles and identity", "escaped text", "long paths", "maximum path counts", "base64 padding"} {
		t.Run(shape, func(t *testing.T) {
			job := testJob(command.Review)
			switch shape {
			case "comment":
				job = testJob(command.Comment)
			case "roles and identity":
				job = withClaudeCritic(job)
				job.Identity.JobID, job.Identity.AttemptID, job.Identity.ExecutorID = strings.Repeat("j", 100), strings.Repeat("a", 100), strings.Repeat("e", 100)
				job.Identity.Repository, job.Identity.Requester = strings.Repeat("o", 100)+"/"+strings.Repeat("r", 100), strings.Repeat("b", 100)+"[bot]"
				job.Identity.RepositoryID, job.Identity.OwnerID, job.Identity.RequesterID = math.MaxInt64, math.MaxInt64, math.MaxInt64
				job.Identity.Number = int(^uint(0) >> 1)
				job.Command, job.RuntimeBudget = strings.Repeat("c", 100), config.MaxJobTimeout
				for _, role := range []*RoleSettings{&job.Main, job.Critic} {
					role.Model, role.Effort = strings.Repeat("\x01", config.MaxModelSettingBytes), strings.Repeat("&", config.MaxModelSettingBytes)
					role.AlwaysOnSkills = make([]string, config.MaxSkillsPerRole-len(role.Skills))
					for i := range role.AlwaysOnSkills {
						role.AlwaysOnSkills[i] = "https://github.com/o/r/tree/main/skills/" + strings.Repeat("s", config.MaxSkillURLBytes-len("https://github.com/o/r/tree/main/skills/"))
					}
				}
			case "escaped text":
				job.Title = strings.Repeat("<&>\x01", maxActionTitleBytes/4)
				job.Description = strings.Repeat("\x00<&>\"\\\n\u2028\u2029", 4096)
				job.Notes = strings.Repeat("\x01", 32<<10)
			case "long paths", "maximum path counts":
				count := 32
				if shape == "maximum path counts" {
					count = workspace.MaxFiles
				}
				job.Source.Files, job.Source.BaseFiles, job.Source.Omitted, job.Source.ChangedPaths = nil, nil, nil, nil
				for i := range count {
					path := fmt.Sprintf("file-%05d", i)
					if shape == "long paths" {
						prefix := fmt.Sprintf("dir-%05d/", i)
						path = prefix + strings.Repeat(strings.Repeat("\x01<&>\\", 50)+"/", 16)
						path += strings.Repeat("z", workspace.MaxPathBytes-1-len(path))
					}
					job.Source.Files = append(job.Source.Files, workspace.File{Path: "h/" + path[2:], Mode: workspace.Regular})
					job.Source.BaseFiles = append(job.Source.BaseFiles, workspace.File{Path: "b/" + path[2:], Mode: workspace.Executable})
					job.Source.Omitted = append(job.Source.Omitted, "o/"+path[2:])
					job.Source.ChangedPaths = append(job.Source.ChangedPaths, "c/"+path[2:])
				}
			case "base64 padding":
				job.Source.Files, job.Source.BaseFiles = nil, nil
				for i := range 64 {
					file := workspace.File{Path: fmt.Sprintf("file-%02d", i), Mode: workspace.Regular, Data: bytes.Repeat([]byte{0xff}, i%5)}
					job.Source.Files = append(job.Source.Files, file)
					job.Source.BaseFiles = append(job.Source.BaseFiles, file)
				}
				job.Source.Repository, job.Source.Diff = []byte{0, 1}, []byte{0}
			}
			if err := job.Validate(); err != nil {
				t.Fatalf("invalid bounded Job shape: %v", err)
			}
			bound := httpJobBound(job)
			envelope := struct {
				Job     Job    `json:"job"`
				Expires int64  `json:"expires_unix_ns"`
				Control string `json:"control"`
			}{Job: job, Expires: math.MaxInt64, Control: strings.Repeat("x", httpControlBytes-1024)}
			encoded, err := httpEncode(envelope, httpJobBytes)
			if err != nil || bound < int64(len(encoded)) || bound > httpJobBytes {
				t.Fatalf("Job size bound failed: bound=%d encoded=%d error=%v", bound, len(encoded), err)
			}
			decoded := envelope
			decoded.Job = Job{}
			if err := httpDecode(bytes.NewReader(encoded), httpJobBytes, &decoded); err != nil {
				t.Fatalf("bounded canonical Job exceeded decoder admission: %v", err)
			}
		})
	}
}

func TestHTTPJobBoundCountsEachPaddedFile(t *testing.T) {
	job := testJob(command.Comment)
	job.Source.Files = make([]workspace.File, 64)
	for i := range job.Source.Files {
		job.Source.Files[i] = workspace.File{Path: fmt.Sprintf("file-%02d", i), Mode: workspace.Regular}
	}
	empty := httpJobBound(job)
	for i := range job.Source.Files {
		job.Source.Files[i].Data = []byte{0}
	}
	if growth := httpJobBound(job) - empty; growth != 64*4 {
		t.Fatalf("per-file base64 padding was aggregated: growth=%d", growth)
	}
}

func TestHTTPJobBoundRefusesLargeDecodedShapesBeforeEncoding(t *testing.T) {
	for _, shape := range []string{"text", "four path lists"} {
		t.Run(shape, func(t *testing.T) {
			job := testJob(command.Review)
			if shape == "text" {
				job.Title, job.Notes = "", ""
				job.Description = strings.Repeat("x", workspace.MaxBytes)
			} else {
				files := make([]workspace.File, workspace.MaxFiles)
				paths := make([]string, workspace.MaxFiles)
				prefix := strings.Repeat(strings.Repeat("\x01", 250)+"/", 5)
				for i := range files {
					paths[i] = prefix + fmt.Sprintf("file-%05d", i)
					files[i] = workspace.File{Path: paths[i], Mode: workspace.Regular}
				}
				job.Source.Files, job.Source.BaseFiles = files, files
				job.Source.Omitted, job.Source.ChangedPaths = paths, paths
			}
			if err := job.Validate(); err != nil {
				t.Fatalf("invalid decoded Job shape: %v", err)
			}
			if bound := httpJobBound(job); bound <= httpJobBytes {
				t.Fatalf("valid decoded Job bypassed conservative pre-encoding refusal: bound=%d", bound)
			}
		})
	}
}
