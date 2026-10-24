package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestResultArtifactContracts(t *testing.T) {
	type artifactCase struct {
		name              string
		mutate            func(*Job, *Result)
		complete, partial string
	}
	cases := []artifactCase{
		{name: "valid audit"},
		{"non-audit", func(j *Job, _ *Result) { j.Action, j.Command = command.Comment, "comment" }, "unexpected action report artifacts", "unexpected action report artifacts"},
		{"missing all", func(_ *Job, r *Result) { r.Artifacts = nil }, "missing report artifacts", ""},
		{"missing may-be-empty artifact", func(_ *Job, r *Result) {
			for i, file := range r.Artifacts {
				if file.Path == "FINDINGS-DETAIL.md" {
					r.Artifacts = append(r.Artifacts[:i], r.Artifacts[i+1:]...)
					break
				}
			}
		}, "incomplete audit artifacts", ""},
		{"executable artifact", func(_ *Job, r *Result) { r.Artifacts[0].Mode = workspace.Executable }, "invalid action report artifact", "invalid action report artifact"},
		{"oversized artifact", func(_ *Job, r *Result) { r.Artifacts[0].Data = make([]byte, maxAuditArtifactBytes+1) }, "invalid action report artifact", "invalid action report artifact"},
		{"NUL in completed report", func(_ *Job, r *Result) {
			for i := range r.Artifacts {
				if r.Artifacts[i].Path == "REPORT.md" {
					r.Artifacts[i].Data = []byte("Report\x00text")
				}
			}
		}, "contains NUL", ""},
		{"different answer", func(_ *Job, r *Result) { r.Answer = "Different unreviewed text." }, "", ""},
		{"blank answer", func(_ *Job, r *Result) { r.Answer = "" }, "", ""},
		{"private report may exceed public rendering budget", func(_ *Job, r *Result) {
			r.Answer = strings.Repeat("a\u0085", 19000)
			for i := range r.Artifacts {
				if r.Artifacts[i].Path == "REPORT.md" {
					r.Artifacts[i].Data = []byte(r.Answer)
				}
			}
		}, "", ""},
		{"partial report exact limit", func(_ *Job, r *Result) {
			r.Answer, r.Calls = "", nil
			r.Artifacts = []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: bytes.Repeat([]byte("x"), maxOutputBytes)}}
		}, "incomplete audit artifacts", ""},
		{"partial report over limit", func(_ *Job, r *Result) {
			r.Answer, r.Calls = "", nil
			r.Artifacts = []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: bytes.Repeat([]byte("x"), maxOutputBytes+1)}}
		}, "incomplete audit artifacts", "report artifacts exceed limits"},
	}
	for _, empty := range auditArtifacts {
		if !auditArtifactMayBeEmpty(empty) {
			cases = append(cases, artifactCase{"empty " + empty, func(_ *Job, r *Result) {
				for i := range r.Artifacts {
					if r.Artifacts[i].Path == empty {
						r.Artifacts[i].Data = nil
					}
				}
			}, "empty action report artifact", ""})
		}
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.SecurityAudit)
			result := completeAuditResult(job)
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid positive control: %v", err)
			}
			if test.mutate != nil {
				test.mutate(&job, &result)
			}
			if err := result.Validate(job); (err == nil) != (test.complete == "") || (err != nil && !strings.Contains(err.Error(), test.complete)) {
				t.Fatalf("complete artifacts=%v want%q", err, test.complete)
			}
			if test.name == "different answer" {
				projected, err := projectPublication(job, result)
				if err != nil || strings.Contains(projected.answer, result.Answer) {
					t.Fatalf("unpublished audit answer reached public summary: %q, %v", projected.answer, err)
				}
			}
			if err := result.validate(job, false); (err == nil) != (test.partial == "") || (err != nil && !strings.Contains(err.Error(), test.partial)) {
				t.Fatalf("partial artifacts=%v want%q", err, test.partial)
			}
		})
	}
}

func TestResultValidateRejectsDuplicateAuditMetadataKeys(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "run-metadata.json" {
			data := string(result.Artifacts[i].Data)
			result.Artifacts[i].Data = []byte(strings.Replace(data, `"run_id":"test-run",`, `"run_id":"test-run","run_id":"test-run",`, 1))
			break
		}
	}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "duplicate object keys") {
		t.Fatalf("duplicate metadata accepted: %v", err)
	}
}

func TestAuditCompanionsStayInsideInstalledSkillAndPortableResult(t *testing.T) {
	job := testJob(command.SecurityAudit)
	for _, companion := range []string{"DOMAIN.md", "/installed/security-audit/DOMAIN.md", "../outside.md", "/etc/passwd", "bad\\path"} {
		t.Run(companion, func(t *testing.T) {
			result := completeAuditResult(job)
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid positive control: %v", err)
			}
			for i := range result.Artifacts {
				if result.Artifacts[i].Path != "run-metadata.json" {
					continue
				}
				var metadata map[string]json.RawMessage
				if err := json.Unmarshal(result.Artifacts[i].Data, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata["selected_companion_files"], _ = json.Marshal([]string{companion})
				result.Artifacts[i].Data, _ = json.Marshal(metadata)
			}
			portable := companion == "DOMAIN.md"
			err := result.Validate(job)
			if (err == nil) != portable || (err != nil && !strings.Contains(err.Error(), "companion file path is not portable")) {
				t.Fatalf("import companion %q: %v", companion, err)
			}
			normalized, err := normalizeAuditArtifacts(result.Artifacts, job, "/installed/security-audit/SKILL.md")
			inside := portable || companion == "/installed/security-audit/DOMAIN.md"
			if (err == nil) != inside {
				t.Fatalf("normalize companion %q: %v", companion, err)
			}
			if inside {
				result.Artifacts = normalized
				if err := result.Validate(job); err != nil {
					t.Fatalf("normalized companion refused: %v", err)
				}
			} else if normalized != nil {
				t.Fatal("invalid companion returned artifacts")
			}
		})
	}
}

func TestJobValidate(t *testing.T) {
	text := strings.Repeat("x", workspace.MaxBytes+1)
	roleSkills := make([]string, config.MaxSkillsPerRole)
	for i := range roleSkills {
		roleSkills[i] = skills.BuiltinAntiPromptInjection
	}
	critic := &RoleSettings{Provider: "claude"}
	for _, test := range []struct {
		name   string
		mutate func(*Job)
		want   string
	}{
		{"job identity", func(j *Job) { j.Identity.JobID = "a/b" }, "action identity is missing or invalid"},
		{"attempt identity", func(j *Job) { j.Identity.AttemptID = "a/b" }, "action identity is missing or invalid"},
		{"executor identity", func(j *Job) { j.Identity.ExecutorID = "a/b" }, "action identity is missing or invalid"},
		{"requester separator", func(j *Job) { j.Identity.Requester = "a/b" }, "action identity is missing or invalid"},
		{"empty bot name", func(j *Job) { j.Identity.Requester = "[bot]" }, "action identity is missing or invalid"},
		{"repeated bot suffix", func(j *Job) { j.Identity.Requester = "app[bot][bot]" }, "action identity is missing or invalid"},
		{"bot suffix case", func(j *Job) { j.Identity.Requester = "app[Bot]" }, "action identity is missing or invalid"},
		{"trailing bot suffix", func(j *Job) { j.Identity.Requester = "app[bot]suffix" }, "action identity is missing or invalid"},
		{"combined text over", func(j *Job) { j.Title, j.Description, j.Notes = "t", text[:workspace.MaxBytes-1], "n" }, "action text is invalid or exceeds limits"},
		{"description over", func(j *Job) { j.Title, j.Description, j.Notes = "", text, "" }, "action text is invalid or exceeds limits"},
		{"notes over", func(j *Job) { j.Title, j.Description, j.Notes = "", "", text }, "action text is invalid or exceeds limits"},
		{"combined role skills over", func(j *Job) { j.Main.Skills, j.Main.AlwaysOnSkills = roleSkills[:1], roleSkills }, "invalid action model or skills"},
		{"inactive critic budget", func(j *Job) { j.CriticMaxRounds = 1 }, "invalid critic round limit"},
		{"inactive critic threshold", func(j *Job) { j.CriticNonblockingRounds = 1 }, "invalid critic round limit"},
		{"inactive critic streak", func(j *Job) { j.CriticConsecutiveNoBlockerRounds = 1 }, "invalid critic round limit"},
		{"zero critic budget", func(j *Job) { j.Critic = critic }, "invalid critic round limit"},
		{"critic budget over maximum", func(j *Job) { j.Critic, j.CriticMaxRounds = critic, config.MaxCriticMaxRounds+1 }, "invalid critic round limit"},
		{"negative critic threshold", func(j *Job) { j.Critic, j.CriticMaxRounds, j.CriticNonblockingRounds = critic, 2, -1 }, "invalid critic round limit"},
		{"negative critic streak", func(j *Job) { j.Critic, j.CriticMaxRounds, j.CriticConsecutiveNoBlockerRounds = critic, 2, -1 }, "invalid critic round limit"},
		{"critic streak exceeds maximum", func(j *Job) {
			j.Critic, j.CriticMaxRounds, j.CriticConsecutiveNoBlockerRounds = critic, 2, config.MaxCriticConsecutiveNoBlockerRounds+1
		}, "invalid critic round limit"},
		{"critic threshold exceeds budget", func(j *Job) { j.Critic, j.CriticMaxRounds, j.CriticNonblockingRounds = critic, 2, 3 }, "invalid critic round limit"},
		{"version", func(j *Job) { j.Version++ }, "unsupported action protocol version"},
		{"source", func(j *Job) { j.Source.Head = "untrusted" }, "snapshot has invalid source commits"},
		{"missing repository", func(j *Job) { j.Source.Repository = nil }, "complete repository bundle"},
		{"requester identity", func(j *Job) { j.Identity.RequesterID = 0 }, "action repository or requester identity is invalid"},
		{"repository identity zero", func(j *Job) { j.Identity.RepositoryID = 0 }, "action repository or requester identity is invalid"},
		{"owner identity zero", func(j *Job) { j.Identity.OwnerID = 0 }, "action repository or requester identity is invalid"},
		{"number zero", func(j *Job) { j.Identity.Number = 0 }, "action repository or requester identity is invalid"},
		{"subject type", func(j *Job) { j.Identity.SubjectType = "Discussion" }, "action repository or requester identity is invalid"},
		{"repository shape", func(j *Job) { j.Identity.Repository = "a/b/c" }, "action repository or requester identity is invalid"},
		{"runtime", func(j *Job) { j.RuntimeBudget = 0 }, "invalid action runtime budget"},
		{"title bound", func(j *Job) { j.Title = strings.Repeat("x", maxActionTitleBytes+1) }, "action text is invalid or exceeds limits"},
		{"action", func(j *Job) { j.Action = "unknown" }, "unsupported action"},
		{"skill path", func(j *Job) { j.Main.Skills = []string{"/tmp/skill/SKILL.md"} }, "skill URL"},
		{"provider", func(j *Job) { j.Main.Provider = "unknown" }, "unsupported action provider"},
		{"file bound", func(j *Job) { j.Source.Files[0].Data = make([]byte, workspace.MaxBytes+1) }, "source exceeds byte limit"},
		{"model bound", func(j *Job) { j.Main.Model = strings.Repeat("m", config.MaxModelSettingBytes+1) }, "invalid action model or skills"},
		{"model NUL", func(j *Job) { j.Main.Model = "a\x00b" }, "invalid action model or skills"},
		{"model CR", func(j *Job) { j.Main.Model = "a\rb" }, "invalid action model or skills"},
		{"model LF", func(j *Job) { j.Main.Model = "a\nb" }, "invalid action model or skills"},
		{"model space", func(j *Job) { j.Main.Model = "a b" }, "invalid action model or skills"},
		{"model tab", func(j *Job) { j.Main.Model = "a\tb" }, "invalid action model or skills"},
		{"effort bound", func(j *Job) { j.Main.Effort = strings.Repeat("e", config.MaxModelSettingBytes+1) }, "invalid action model or skills"},
		{"effort CR", func(j *Job) { j.Main.Effort = "a\rb" }, "invalid action model or skills"},
		{"effort LF", func(j *Job) { j.Main.Effort = "a\nb" }, "invalid action model or skills"},
		{"effort space", func(j *Job) { j.Main.Effort = "a b" }, "invalid action model or skills"},
		{"effort tab", func(j *Job) { j.Main.Effort = "a\tb" }, "invalid action model or skills"},
		{"effort NUL", func(j *Job) { j.Main.Effort = "a\x00b" }, "invalid action model or skills"},
		{"negative runtime", func(j *Job) { j.RuntimeBudget = -1 }, "invalid action runtime budget"},
		{"invalid UTF8 title", func(j *Job) { j.Title = "\xff" }, "action text is invalid or exceeds limits"},
		{"invalid UTF8 description", func(j *Job) { j.Description = "\xff" }, "action text is invalid or exceeds limits"},
		{"invalid UTF8 notes", func(j *Job) { j.Notes = "\xff" }, "action text is invalid or exceeds limits"},
		{"critic model", func(j *Job) { j.Critic = &RoleSettings{Provider: "claude", Model: "a\x00b"}; j.CriticMaxRounds = 1 }, "invalid action model or skills"},
		{"critic model space", func(j *Job) { j.Critic = &RoleSettings{Provider: "claude", Model: "a b"}; j.CriticMaxRounds = 1 }, "invalid action model or skills"},
		{"critic model bound", func(j *Job) {
			j.Critic = &RoleSettings{Provider: "claude", Model: strings.Repeat("m", config.MaxModelSettingBytes+1)}
			j.CriticMaxRounds = 1
		}, "invalid action model or skills"},
		{"critic always-on skill", func(j *Job) {
			j.Critic = &RoleSettings{Provider: "claude", AlwaysOnSkills: []string{"/tmp/private/SKILL.md"}}
			j.CriticMaxRounds = 1
		}, "skill URL"},
		{"missing main skills", func(j *Job) { j.Main.Skills = nil }, "requires at least one skill"},
		{"skill length", func(j *Job) {
			j.Main.Skills = []string{"https://github.com/o/r/tree/main/skills/" + strings.Repeat("a", config.MaxSkillURLBytes)}
		}, "skill source exceeds limit"},
		{"command", func(j *Job) { j.Command = "../command" }, "invalid action command"},
		{"main workflow", func(j *Job) { j.Action = command.SecurityAudit; j.Main.Provider = "claude" }, "unsupported by main provider"},
		{"critic provider", func(j *Job) { j.Critic = &RoleSettings{Provider: "unknown"} }, "unsupported action provider"},
		{"critic required skills", func(j *Job) {
			j.Critic = &RoleSettings{Provider: "claude", Skills: []string{skills.BuiltinAntiPromptInjection}}
		}, "only always-on skills"},
		{"same provider", func(j *Job) { j.Critic = &RoleSettings{Provider: j.Main.Provider} }, "require different providers"},
		{"runtime maximum", func(j *Job) { j.RuntimeBudget = config.MaxJobTimeout + time.Nanosecond }, "invalid action runtime budget"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Comment)
			if err := job.Validate(); err != nil {
				t.Fatal(err)
			}
			test.mutate(&job)
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("settings validation=%v want%q", err, test.want)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*Job)
	}{
		{"job identity", func(j *Job) { j.Identity.JobID = strings.Repeat("a", 100) }},
		{"attempt identity", func(j *Job) { j.Identity.AttemptID = strings.Repeat("a", 100) }},
		{"executor identity", func(j *Job) { j.Identity.ExecutorID = strings.Repeat("a", 100) }},
		{"requester identity", func(j *Job) { j.Identity.Requester = strings.Repeat("a", 100) }},
		{"bot requester", func(j *Job) { j.Identity.Requester = "app[bot]" }},
		{"combined text exact", func(j *Job) { j.Title, j.Description, j.Notes = "t", text[:workspace.MaxBytes-2], "n" }},
		{"description exact", func(j *Job) { j.Title, j.Description, j.Notes = "", text[:workspace.MaxBytes], "" }},
		{"notes exact", func(j *Job) { j.Title, j.Description, j.Notes = "", "", text[:workspace.MaxBytes] }},
		{"combined role skills exact", func(j *Job) {
			j.Main.Skills, j.Main.AlwaysOnSkills = roleSkills[:config.MaxSkillsPerRole/2], roleSkills[:config.MaxSkillsPerRole-config.MaxSkillsPerRole/2]
		}},
		{"critic one round", func(j *Job) { j.Critic, j.CriticMaxRounds = critic, 1 }},
		{"critic maximum rounds", func(j *Job) { j.Critic, j.CriticMaxRounds = critic, config.MaxCriticMaxRounds }},
		{"critic threshold at budget", func(j *Job) { j.Critic, j.CriticMaxRounds, j.CriticNonblockingRounds = critic, 2, 2 }},
		{"critic streak above budget", func(j *Job) { j.Critic, j.CriticMaxRounds, j.CriticConsecutiveNoBlockerRounds = critic, 2, 3 }},
		{"critic maximum streak", func(j *Job) {
			j.Critic, j.CriticMaxRounds, j.CriticConsecutiveNoBlockerRounds = critic, 2, config.MaxCriticConsecutiveNoBlockerRounds
		}},
		{"maximum settings", func(j *Job) {
			j.Main.Model, j.Main.Effort = strings.Repeat("m", config.MaxModelSettingBytes), strings.Repeat("e", config.MaxModelSettingBytes)
			j.RuntimeBudget = config.MaxJobTimeout
		}},
	} {
		t.Run("valid/"+test.name, func(t *testing.T) {
			job := testJob(command.Comment)
			test.mutate(&job)
			if err := job.Validate(); err != nil {
				t.Fatalf("valid boundary refused: %v", err)
			}
		})
	}
}

func TestAuditJobRejectsUncitableSourcePathsBeforeModel(t *testing.T) {
	for _, test := range []struct{ name, path string }{
		{"backslash", `literal\backslash`},
		{"colon", "segment:name"},
		{"reserved", "CON.txt"},
		{"trailing dot", "source."},
		{"leading tilde", "~source"},
		{"default ignorable", "so\u200burce"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.SecurityAudit)
			job.Source.Files[0].Path = test.path
			if err := workspace.ValidateSnapshot(job.Source); err != nil {
				t.Fatalf("fixture source path is not admitted by the portable snapshot: %v", err)
			}
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "audit source path cannot be cited") || strings.Contains(err.Error(), test.path) {
				t.Fatalf("audit path was accepted or echoed: %v", err)
			}
			called := false
			runner := Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
				called = true
				return aicli.Result{}, nil
			}}
			if _, err := runner.Run(t.Context(), job); !errors.Is(err, monitor.ErrTerminalJob) || called {
				t.Fatalf("uncitable source reached model: called=%t error=%v", called, err)
			}
		})
	}
	job := testJob(command.SecurityAudit)
	job.Source.Files[0].Path = "src/ordinary.go"
	if err := job.Validate(); err != nil {
		t.Fatalf("ordinary audit source rejected: %v", err)
	}
	job = testJob(command.Comment)
	job.Source.Files[0].Path = `literal\backslash`
	if err := job.Validate(); err != nil {
		t.Fatalf("other action source acceptance changed: %v", err)
	}
	job = testJob(command.SecurityAudit)
	job.Source.Omitted = []string{`link\source`}
	job.Source.HeadOmitted = []string{`link\source`}
	if err := workspace.ValidateSnapshot(job.Source); err != nil {
		t.Fatalf("fixture omitted head path is not admitted by the portable snapshot: %v", err)
	}
	if err := job.Validate(); err == nil || !strings.Contains(err.Error(), "audit source path cannot be cited") {
		t.Fatalf("uncitable current link admitted: %v", err)
	}
	job = testJob(command.SecurityAudit)
	job.Identity.SubjectType = "PullRequest"
	job.Source.Base, job.Source.MergeBase = portableHead, portableHead
	job.Source.BaseFiles = []workspace.File{{Path: `base\source`, Mode: workspace.Regular, Data: []byte("base\n")}}
	if err := job.Validate(); err != nil {
		t.Fatalf("uncitable base-only source blocked current audit: %v", err)
	}
}

func TestResultJSONRoundTripPreservesValidatedPayload(t *testing.T) {
	job := testJob(command.Fix)
	job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 3
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.", CriticStopReason: "approved",
		Changes:     []workspace.Change{{Path: "binary", Mode: workspace.Executable, Data: []byte{0, 0xff, '\n'}}},
		Refutations: []Refutation{{Round: 2, Text: "Private refutation."}},
		Diagnostics: []Diagnostic{{Code: "incomplete_usage", Message: "Provider omitted usage."}},
		Calls: []CallUsage{
			{Provider: job.Main.Provider, Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 11, CachedInput: 2, Output: 3}},
			{Provider: job.Critic.Provider, Role: roleCritic, ReviewComplete: true, Round: 1, ReviewPhase: reviewFull, HasBlockers: true, Complete: true},
			{Provider: job.Main.Provider, Role: roleMain, Round: 2, Complete: true},
			{Provider: job.Critic.Provider, Role: roleCritic, ReviewComplete: true, Round: 2, ReviewPhase: reviewVerification, NoBlockers: true},
			{Provider: job.Critic.Provider, Role: roleCritic, ReviewComplete: true, Round: 2, ReviewPhase: reviewFull, HasBlockers: true},
			{Provider: job.Main.Provider, Role: roleMain, Round: 3},
			{Provider: job.Critic.Provider, Role: roleCritic, ReviewComplete: true, Round: 3, ReviewPhase: reviewVerification, Approved: true, NoBlockers: true, Complete: true},
			{Provider: job.Critic.Provider, Role: roleCritic, ReviewComplete: true, Round: 3, ReviewPhase: reviewFull, Approved: true, NoBlockers: true},
		}}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"review_complete":true`, `"critic_stop_reason":"approved"`, `"approved":true`, `"no_blockers":true`, `"has_blockers":true`, `"complete":true`, `"complete":false`} {
		if !bytes.Contains(data, []byte(field)) {
			t.Fatalf("attestation %s missing from wire result: %s", field, data)
		}
	}
	var decoded Result
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(job); err != nil || !reflect.DeepEqual(decoded, result) {
		t.Fatalf("result roundtrip lost validated data: %+v,%v", decoded, err)
	}
}

func TestFixJobRejectsUnsupportedLayoutBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name    string
		files   []workspace.File
		omitted []string
		want    string
	}{
		{"directory spelling", []workspace.File{{Path: "Docs/a", Mode: workspace.Regular}, {Path: "docs/b", Mode: workspace.Regular}}, nil, "collision"},
		{"backslash", []workspace.File{{Path: `back\slash`, Mode: workspace.Regular}}, nil, "unsafe fix source path"},
		{"omitted folded ancestor", []workspace.File{{Path: "ſ/a", Mode: workspace.Regular}}, []string{"s"}, "collision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Fix)
			job.Source.Files, job.Source.Omitted = test.files, test.omitted
			if err := workspace.ValidateSnapshot(job.Source); err != nil {
				t.Fatalf("invalid exact source fixture: %v", err)
			}
			if err := job.Validate(); !errors.Is(err, gitwork.ErrUnsupportedSource) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported fix layout validation: %v", err)
			}
			runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
			if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, gitwork.ErrUnsupportedSource) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported fix source was not refused before execution: %v", err)
			}
			job.Action = command.Comment
			if err := job.Validate(); err != nil {
				t.Fatalf("read-only exact source must remain supported: %v", err)
			}
		})
	}
}

func TestResultCallUsageRequiresOrderedReviewPhases(t *testing.T) {
	job := testJob(command.Comment)
	job.Main.Model, job.Main.Effort = "main-model", "high"
	job.Critic = &RoleSettings{Provider: "claude", Model: "critic-model", Effort: "medium"}
	job.CriticMaxRounds = config.DefaultCriticMaxRounds
	main := func(round int) CallUsage {
		return CallUsage{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, Role: "main", Round: round, Complete: true}
	}
	critic := func(round int, phase string, verdict CallUsage) CallUsage {
		return CallUsage{Provider: job.Critic.Provider, Model: job.Critic.Model, Effort: job.Critic.Effort, Role: roleCritic, Round: round, ReviewPhase: phase, ReviewComplete: verdict.ReviewComplete, Approved: verdict.Approved, NoBlockers: verdict.NoBlockers, HasBlockers: verdict.HasBlockers, Complete: true}
	}
	approved := CallUsage{ReviewComplete: true, Approved: true, NoBlockers: true}
	blocking := CallUsage{ReviewComplete: true, HasBlockers: true}
	nits := CallUsage{ReviewComplete: true, NoBlockers: true}
	incomplete := CallUsage{HasBlockers: true}
	for _, test := range []struct {
		name              string
		calls             []CallUsage
		threshold         int
		reason            string
		complete, partial bool
	}{
		{"no review calls", nil, 0, "", false, true},
		{"first full review", []CallUsage{main(1), critic(1, "full", approved)}, 0, "approved", true, true},
		{"rejected full review", []CallUsage{main(1), critic(1, "full", blocking)}, 0, "", false, true},
		{"incomplete first full review", []CallUsage{main(1), critic(1, reviewFull, incomplete)}, 1, "", false, true},
		{"incomplete verification", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, incomplete)}, 2, "", false, true},
		{"incomplete full after verification", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, approved), critic(2, reviewFull, incomplete)}, 2, "", false, true},
		{"incomplete first full cannot revise", []CallUsage{main(1), critic(1, reviewFull, incomplete), main(2)}, 1, "", false, false},
		{"incomplete verification cannot revise", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, incomplete), main(3)}, 0, "", false, false},
		{"incomplete verification cannot advance full", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, incomplete), critic(2, reviewFull, approved)}, 0, "", false, false},
		{"incomplete full after verification cannot revise", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, approved), critic(2, reviewFull, incomplete), main(3)}, 0, "", false, false},
		{"incomplete full cannot claim approval", []CallUsage{main(1), critic(1, reviewFull, incomplete)}, 1, "approved", false, false},
		{"incomplete full cannot claim nonblocking acceptance", []CallUsage{main(1), critic(1, reviewFull, incomplete)}, 1, "nonblocking", false, false},
		{"coverage without parsed verdict", []CallUsage{main(1), critic(1, reviewFull, CallUsage{ReviewComplete: true})}, 0, "", false, false},
		{"incomplete approving verdict", []CallUsage{main(1), critic(1, reviewFull, CallUsage{Approved: true, NoBlockers: true})}, 1, "", false, false},
		{"incomplete no-blocker verdict", []CallUsage{main(1), critic(1, reviewFull, CallUsage{NoBlockers: true})}, 1, "", false, false},
		{"revised full review", []CallUsage{main(1), critic(1, "full", blocking), main(2), critic(2, "verification", approved), critic(2, "full", approved)}, 0, "approved", true, true},
		{"strict verification nits survive full approval", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, approved)}, 0, "", false, true},
		{"strict verification nits can revise after approved full", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, approved), main(3), critic(3, reviewVerification, approved), critic(3, reviewFull, approved)}, 0, "approved", true, true},
		{"verification nits remain before threshold", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, approved)}, 3, "", false, true},
		{"threshold waives verification nits", []CallUsage{main(1), critic(1, reviewFull, blocking), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, approved)}, 2, "nonblocking", true, true},
		{"rejected verification then revision", []CallUsage{main(1), critic(1, "full", blocking), main(2), critic(2, "verification", blocking), main(3), critic(3, "verification", approved), critic(3, "full", approved)}, 0, "approved", true, true},
		{"unapproved verification followed by full", []CallUsage{main(1), critic(1, "full", blocking), main(2), critic(2, "verification", blocking), critic(2, "full", approved)}, 0, "", false, false},
		{"approved verification skips full", []CallUsage{main(1), critic(1, "full", blocking), main(2), critic(2, "verification", approved), main(3)}, 0, "", false, false},
		{"approved full followed by main", []CallUsage{main(1), critic(1, "full", approved), main(2)}, 0, "", false, false},
		{"interrupted first main", []CallUsage{main(1)}, 0, "", false, true},
		{"interrupted revision", []CallUsage{main(1), critic(1, "full", blocking), main(2)}, 0, "", false, true},
		{"interrupted verification", []CallUsage{main(1), critic(1, "full", blocking), main(2), critic(2, "verification", blocking)}, 0, "", false, true},
		{"critic first", []CallUsage{critic(1, "full", approved)}, 0, "", false, false},
		{"missing first full", []CallUsage{main(1), critic(1, "verification", approved)}, 0, "", false, false},
		{"missing revision verification", []CallUsage{main(1), critic(1, "full", blocking), main(2), critic(2, "full", approved)}, 0, "", false, false},
		{"repeated full", []CallUsage{main(1), critic(1, "full", approved), critic(1, "full", approved)}, 0, "", false, false},
		{"main before critic", []CallUsage{main(1), main(2)}, 0, "", false, false},
		{"skipped round", []CallUsage{main(1), critic(1, "full", blocking), main(3)}, 0, "", false, false},
		{"critic round mismatch", []CallUsage{main(1), critic(2, "full", approved)}, 0, "", false, false},
		{"unknown phase", []CallUsage{main(1), critic(1, "focused", approved)}, 0, "", false, false},
		{"nits before threshold", []CallUsage{main(1), critic(1, reviewFull, nits)}, 2, "", false, true},
		{"nits at threshold", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, nits)}, 2, "nonblocking", true, true},
		{"verification cannot finish", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, nits)}, 2, "", false, true},
		{"fresh full still has blockers", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, blocking)}, 2, "", false, true},
		{"unparsed verdict cannot finish", []CallUsage{main(1), critic(1, reviewFull, CallUsage{})}, 2, "", false, true},
		{"unparsed verdict cannot revise", []CallUsage{main(1), critic(1, reviewFull, CallUsage{}), main(2), critic(2, reviewVerification, approved), critic(2, reviewFull, approved)}, 2, "", false, false},
		{"approval without no-blocker attestation", []CallUsage{main(1), critic(1, reviewFull, CallUsage{ReviewComplete: true, Approved: true})}, 2, "", false, false},
		{"contradictory blocker attestations", []CallUsage{main(1), critic(1, reviewFull, CallUsage{ReviewComplete: true, HasBlockers: true, NoBlockers: true})}, 2, "", false, false},
		{"accepted verification cannot skip full", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, nits), main(3)}, 2, "", false, false},
		{"accepted full cannot revise again", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, nits), critic(2, reviewFull, nits), main(3)}, 2, "", false, false},
		{"blocker verification cannot advance", []CallUsage{main(1), critic(1, reviewFull, nits), main(2), critic(2, reviewVerification, blocking), critic(2, reviewFull, approved)}, 2, "", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			job.CriticNonblockingRounds = test.threshold
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", CriticStopReason: test.reason, Calls: test.calls}
			if err := result.Validate(job); (err == nil) != test.complete || (len(test.calls) == 0 && !strings.Contains(err.Error(), "final full review")) {
				t.Fatalf("complete validation=%v, want valid=%v", err, test.complete)
			}
			if err := result.validate(job, false); (err == nil) != test.partial {
				t.Fatalf("interrupted validation=%v, want valid=%v", err, test.partial)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*CallUsage)
	}{
		{"main phase", func(call *CallUsage) { call.ReviewPhase = "full" }},
		{"main approval", func(call *CallUsage) { call.Approved = true }},
		{"main cannot attest no blockers", func(call *CallUsage) { call.NoBlockers = true }},
		{"main cannot attest blockers", func(call *CallUsage) { call.HasBlockers = true }},
		{"main cannot attest review coverage", func(call *CallUsage) { call.ReviewComplete = true }},
		{"provider attribution", func(call *CallUsage) { call.Provider = "claude" }},
		{"model attribution", func(call *CallUsage) { call.Model = "different-model" }},
		{"effort attribution", func(call *CallUsage) { call.Effort = "low" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := main(1)
			test.mutate(&call)
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: []CallUsage{call, critic(1, "full", approved)}}
			if err := result.validate(job, false); err == nil {
				t.Fatal("accepted malformed or incorrectly attributed call")
			}
		})
	}
}

func TestResultCallUsageRejectsCriticsAndRepeatedMainWithoutCritic(t *testing.T) {
	job := testJob(command.Comment)
	main := CallUsage{Provider: job.Main.Provider, Role: "main", Round: 1}
	for _, calls := range [][]CallUsage{
		{{Provider: "claude", Role: "critic", Round: 1, ReviewPhase: "full"}},
		{main, {Provider: "claude", Role: "critic", Round: 1, ReviewPhase: "full"}},
		{main, {Provider: job.Main.Provider, Role: "main", Round: 2}},
		{main, main},
	} {
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: calls}
		for _, complete := range []bool{false, true} {
			if err := result.validate(job, complete); err == nil || (!strings.Contains(err.Error(), "model call")) {
				t.Fatalf("accepted unexpected no-critic calls (complete=%t): %+v,%v", complete, calls, err)
			}
		}
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: []CallUsage{main}}
	if err := result.Validate(job); err != nil {
		t.Fatalf("refused one valid main call without critic: %v", err)
	}
	result.Calls = nil
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "requires one main call") {
		t.Fatalf("accepted complete result without an executed main call: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("refused an interrupted result before main dispatch: %v", err)
	}
}

func TestProtocolRejectsEarlierCriticContracts(t *testing.T) {
	job := testJob(command.Comment)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1}}}
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	if err := status.Validate(job); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{3, 4} {
		oldJob := job
		oldJob.Version = version
		result.Version, status.Version = version, version
		if err := oldJob.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported action protocol version") {
			t.Fatalf("version %d job accepted: %v", version, err)
		}
		if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "version, identity or source") {
			t.Fatalf("version %d result accepted: %v", version, err)
		}
		if err := status.Validate(job); err == nil || !strings.Contains(err.Error(), "version, identity or source") {
			t.Fatalf("version %d status accepted: %v", version, err)
		}
	}
}

func TestResultCallUsageHonorsConfiguredRoundLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
	}{
		{"explicit", 2}, {"configured default", config.DefaultCriticMaxRounds}, {"maximum", config.MaxCriticMaxRounds},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, test.limit
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", CriticStopReason: "approved"}
			for round := 1; round <= test.limit+1; round++ {
				result.Calls = nil
				for proposal := 1; proposal <= round; proposal++ {
					result.Calls = append(result.Calls, CallUsage{Provider: job.Main.Provider, Role: "main", Round: proposal})
					if proposal > 1 {
						result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: "critic", ReviewComplete: true, Round: proposal, ReviewPhase: "verification", Approved: true, NoBlockers: true})
					}
					result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: "critic", ReviewComplete: true, Round: proposal, ReviewPhase: "full", Approved: proposal == round, NoBlockers: proposal == round, HasBlockers: proposal != round})
				}
				if err := result.Validate(job); (err == nil) != (round <= test.limit) {
					t.Fatalf("round=%d limit=%d result validation=%v", round, test.limit, err)
				}
			}
		})
	}
}

func TestPortableContractsRejectMismatches(t *testing.T) {
	job := testJob(command.Comment)
	job.Title = strings.Repeat("😀", 256)
	if err := job.Validate(); err != nil {
		t.Fatalf("rejected title at UTF-8 boundary: %v", err)
	}
	valid := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "answer", Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
	if err := valid.Validate(job); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, want string
		change     func(*Result)
	}{
		{"version", "action result version, identity or source differs from job", func(r *Result) { r.Version++ }},
		{"source", "action result version, identity or source differs from job", func(r *Result) { r.SourceRef = strings.Repeat("b", 40) }},
		{"identity", "action result version, identity or source differs from job", func(r *Result) { r.Identity.AttemptID = "stale" }},
		{"oversized answer", "action answer is blank, invalid or exceeds limit", func(r *Result) { r.Answer = strings.Repeat("x", maxOutputBytes+1) }},
		{"invisible answer", "action answer is blank, invalid or exceeds limit", func(r *Result) { r.Answer = "\u200b\u2060" }},
		{"read-only changes", "read-only action returned source changes", func(r *Result) { r.Changes = []workspace.Change{{Path: "added", Mode: workspace.Regular}} }},
		{"usage", "invalid model call usage", func(r *Result) {
			r.Calls = []CallUsage{{Provider: "codex", Role: "main", Round: 1, Tokens: usage.Tokens{Input: -1}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.change(&result)
			if err := result.Validate(job); err == nil || err.Error() != test.want {
				t.Fatalf("result mismatch=%v want%q", err, test.want)
			}
		})
	}
}

func TestResultRejectsInvalidRawAnswer(t *testing.T) {
	job := testJob(command.Comment)
	valid := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "ok", Calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1}}}
	if err := valid.Validate(job); err != nil {
		t.Fatalf("invalid positive control: %v", err)
	}
	for _, test := range []struct {
		name, answer string
	}{
		{"NUL", "ok\x00"},
		{"oversized raw answer after trimming", "ok" + strings.Repeat(" ", maxOutputBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !modelOutputFits(test.answer) {
				t.Fatal("fixture must isolate raw text validation from rendered output limits")
			}
			result := valid
			result.Answer = test.answer
			for _, complete := range []bool{false, true} {
				if err := result.validate(job, complete); err == nil || !strings.Contains(err.Error(), "action answer") {
					t.Fatalf("raw answer complete=%t validation=%v", complete, err)
				}
			}
		})
	}
}

func TestResultRejectsChangedFileBounds(t *testing.T) {
	job := testJob(command.Fix)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "answer", Calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1}}}
	for i := 0; i < workspace.MaxChanges; i++ {
		result.Changes = append(result.Changes, workspace.Change{Path: fmt.Sprintf("file-%d", i), Mode: workspace.Regular})
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("invalid exact-limit fixture: %v", err)
	}
	result.Changes = append(result.Changes, workspace.Change{Path: "one-too-many", Mode: workspace.Regular})
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "changed file limit") {
		t.Fatalf("changed file bound validation = %v", err)
	}
}

func TestFixResultValidatesFinalLayoutAgainstOriginalSource(t *testing.T) {
	for _, test := range []struct {
		name    string
		files   []workspace.File
		omitted string
		changes []workspace.Change
		want    string
	}{
		{"same directory", nil, "dir/link", []workspace.Change{{Path: "dir/new", Mode: workspace.Regular}}, ""},
		{"directory spelling", nil, "dir/link", []workspace.Change{{Path: "Dir/new", Mode: workspace.Regular}}, "directory spelling collision"},
		{"unicode directory spelling", nil, "ſ/link", []workspace.Change{{Path: "s/new", Mode: workspace.Regular}}, "directory spelling collision"},
		{"replace omission", nil, "dir/link", []workspace.Change{{Path: "dir/link", Mode: workspace.Regular}}, "omitted source path"},
		{"delete omission", nil, "dir/link", []workspace.Change{{Path: "DIR/LINK", Delete: true}}, "omitted source path"},
		{"write descendant", nil, "dir/link", []workspace.Change{{Path: "DIR/link/new", Mode: workspace.Regular}}, "collision"},
		{"replace ancestor", nil, "dir/link", []workspace.Change{{Path: "DIR", Mode: workspace.Regular}}, "collision"},
		{"unchanged directory spelling", []workspace.File{{Path: "Foo/stay", Mode: workspace.Regular}}, "", []workspace.Change{{Path: "foo/new", Mode: workspace.Regular}}, "directory spelling collision"},
		{"directory rename", []workspace.File{{Path: "Foo/stay", Mode: workspace.Regular}}, "", []workspace.Change{{Path: "Foo/stay", Delete: true}, {Path: "foo/new", Mode: workspace.Regular}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Fix)
			if test.files != nil {
				job.Source.Files = test.files
			}
			if test.omitted != "" {
				job.Source.Omitted = []string{test.omitted}
			}
			if err := job.Validate(); err != nil {
				t.Fatalf("invalid test source layout: %v", err)
			}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
				Changes: test.changes, Calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1}}}
			if err := result.Validate(job); (err == nil) != (test.want == "") || (err != nil && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("final layout validation=%v want%q", err, test.want)
			}
		})
	}
}

func TestResultDiagnosticBounds(t *testing.T) {
	job := testJob(command.Comment)
	tooMany := make([]Diagnostic, maxDiagnostics+1)
	for i := range tooMany {
		tooMany[i] = Diagnostic{Code: "code", Message: "message"}
	}
	for _, test := range []struct {
		name        string
		diagnostics []Diagnostic
		want        string
	}{
		{"valid", []Diagnostic{{Code: "incomplete_usage", Message: "Provider omitted usage."}}, ""},
		{"maximum count", tooMany[:maxDiagnostics], ""},
		{"maximum message", []Diagnostic{{Code: "code", Message: strings.Repeat("x", maxDiagnosticMessageBytes)}}, ""},
		{"empty code", []Diagnostic{{Code: "", Message: "message"}}, "invalid action diagnostic"},
		{"unsafe code", []Diagnostic{{Code: "../private", Message: "message"}}, "invalid action diagnostic"},
		{"nul message", []Diagnostic{{Code: "code", Message: "nul\x00message"}}, "invalid action diagnostic"},
		{"invalid UTF8 message", []Diagnostic{{Code: "code", Message: string([]byte{0xff})}}, "invalid action diagnostic"},
		{"oversized message", []Diagnostic{{Code: "code", Message: strings.Repeat("x", maxDiagnosticMessageBytes+1)}}, "invalid action diagnostic"},
		{"too many", tooMany, "too many action diagnostics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.",
				Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}, Diagnostics: test.diagnostics}
			if err := result.Validate(job); (err == nil) != (test.want == "") || (err != nil && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("diagnostics validation=%v want%q", err, test.want)
			}
		})
	}
}

func TestJobSubjectMatchesComparisonForEveryAction(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.SecurityAudit, command.Review, command.Fix} {
		for _, subject := range []string{"Issue", "PullRequest"} {
			for _, comparison := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/comparison=%t", action, subject, comparison), func(t *testing.T) {
					job := testJob(action)
					job.Identity.SubjectType = subject
					job.Source.Base, job.Source.MergeBase = "", ""
					job.Source.BaseFiles, job.Source.Diff = nil, nil
					if comparison {
						job.Source.Base, job.Source.MergeBase = strings.Repeat("b", 40), strings.Repeat("c", 40)
					}
					if err := workspace.ValidateSnapshot(job.Source); err != nil {
						t.Fatalf("invalid source fixture: %v", err)
					}
					want := ""
					if (subject == "PullRequest") != comparison {
						want = "action subject and supplied comparison differ"
					} else if action == command.Review && subject != "PullRequest" {
						want = "review requires a supplied pull request comparison"
					} else if action == command.Fix && subject != "Issue" {
						want = "fix requires an issue"
					}
					err := job.Validate()
					if (err == nil) != (want == "") || (err != nil && !strings.Contains(err.Error(), want)) {
						t.Fatalf("subject/comparison validation=%v want%q", err, want)
					}
				})
			}
		}
	}
}

func TestJobJSONRoundTrip(t *testing.T) {
	job := testJob(command.Review)
	job.Critic = &RoleSettings{Provider: "claude", Model: "critic-model", Effort: "high"}
	job.CriticMaxRounds = config.DefaultCriticMaxRounds
	job.CriticNonblockingRounds, job.CriticConsecutiveNoBlockerRounds = 4, 2
	job.Source.Omitted = []string{"omitted-link"}
	job.Source.ChangedPaths = []string{`back\slash`, "case", "CASE"}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Job
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(job, decoded) {
		t.Fatalf("job changed in JSON round trip: %#v != %#v", job, decoded)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded job: %v", err)
	}
}

func TestResultEmptyAnswerContract(t *testing.T) {
	for _, test := range []struct {
		action  command.Action
		critic  bool
		changed bool
		valid   bool
	}{{command.Fix, false, true, true}, {command.Fix, false, false, false}, {command.Fix, true, false, false}, {command.Fix, true, true, true}, {command.Comment, false, false, false}, {command.Review, false, false, false}, {command.SecurityAudit, false, false, true}} {
		t.Run(fmt.Sprintf("%s/critic=%t/changed=%t", test.action, test.critic, test.changed), func(t *testing.T) {
			job := testJob(test.action)
			if test.critic {
				job = withClaudeCritic(job)
			}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
			if test.action == command.SecurityAudit {
				result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), "Audit report\n")
			}
			if test.critic {
				result.CriticStopReason = "approved"
				result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: roleCritic, ReviewComplete: true, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true})
			}
			if test.changed {
				result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("fixed source")}}
			}
			if test.critic && test.changed {
				result.Answer = "Fixed source."
				if err := result.Validate(job); err != nil {
					t.Fatalf("invalid changed-fix critic positive control: %v", err)
				}
				result.Answer = ""
			}
			if err := result.Validate(job); (err == nil) != test.valid || (!test.valid && !strings.Contains(err.Error(), "action answer")) {
				t.Fatalf("empty %s answer with critic=%t: %v", test.action, test.critic, err)
			}
		})
	}
}
