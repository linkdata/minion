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

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRequesterAllowsOneBotSuffix(t *testing.T) {
	for _, requester := range []string{"writer", "app[bot]", "[bot]", "app[bot][bot]", "app[Bot]", "app[bot]suffix"} {
		job := portableJob(command.Comment)
		job.Identity.Requester = requester
		valid := requester == "writer" || requester == "app[bot]"
		if err := job.Validate(); (err == nil) != valid {
			t.Fatalf("requester=%q validation=%v want valid=%t", requester, err, valid)
		}
	}
}

func auditArtifactsFixture(job Job, target, report string) []workspace.File {
	metadata := fmt.Sprintf(`{"run_id":"test-run","repo":%q,"target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, job.Identity.Repository, target, job.Source.Head, auditTestOwners)
	var files []workspace.File
	for _, name := range auditArtifacts {
		data := "Audit content\n"
		switch name {
		case "run-metadata.json":
			data = metadata
		case "REPORT.md":
			data = report
		case "findings.json":
			data = "[]"
		case "coverage-ledger.json":
			data = `[{"status":"covered"}]`
		case "FINDINGS-DETAIL.md", "NEEDS-VALIDATION.md":
			data = ""
		}
		files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: []byte(data)})
	}
	return files
}

func completeAuditResult(job Job) Result {
	const report = "Reviewed audit report."
	return Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: report, Calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1}}, Artifacts: auditArtifactsFixture(job, sourceIdentity(job), report)}
}

func TestResultArtifactActionAndCardinality(t *testing.T) {
	for _, scenario := range []string{"valid audit", "non-audit", "missing all", "missing optional"} {
		t.Run(scenario, func(t *testing.T) {
			job := portableJob(command.SecurityAudit)
			result := completeAuditResult(job)
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid positive control: %v", err)
			}
			want := ""
			switch scenario {
			case "non-audit":
				job.Action, job.Command = command.Comment, "comment"
				want = "unexpected action report artifacts"
			case "missing all":
				result.Artifacts = nil
				want = "missing report artifacts"
			case "missing optional":
				for i, file := range result.Artifacts {
					if file.Path == "FINDINGS-DETAIL.md" {
						result.Artifacts = append(result.Artifacts[:i], result.Artifacts[i+1:]...)
						break
					}
				}
				want = "incomplete audit artifacts"
			}
			err := result.Validate(job)
			if (err == nil) != (want == "") || (err != nil && !strings.Contains(err.Error(), want)) {
				t.Fatalf("artifact action/count=%v want%q", err, want)
			}
		})
	}
}

func TestAuditCompanionsStayInsideInstalledSkillAndPortableResult(t *testing.T) {
	job := portableJob(command.SecurityAudit)
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

func TestIdentityStringBounds(t *testing.T) {
	for _, field := range []struct {
		name string
		set  func(*Identity, string)
	}{
		{"job", func(id *Identity, v string) { id.JobID = v }}, {"attempt", func(id *Identity, v string) { id.AttemptID = v }},
		{"executor", func(id *Identity, v string) { id.ExecutorID = v }}, {"requester", func(id *Identity, v string) { id.Requester = v }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, value := range []string{strings.Repeat("a", 100), "a/b"} {
				job := portableJob(command.Comment)
				field.set(&job.Identity, value)
				err := job.Validate()
				if (err == nil) != (value == strings.Repeat("a", 100)) || (err != nil && !strings.Contains(err.Error(), "action identity is missing or invalid")) {
					t.Fatalf("identity %q: %v", value, err)
				}
			}
		})
	}
}

func TestJobRejectsInvalidRoleAndDispatchSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Job)
		want   string
	}{
		{"version", func(j *Job) { j.Version++ }, "unsupported action protocol version"},
		{"source", func(j *Job) { j.Source.Head = "untrusted" }, "snapshot has invalid source commits"},
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
		{"model bound", func(j *Job) { j.Main.Model = strings.Repeat("m", 129) }, "invalid action model or skills"},
		{"model NUL", func(j *Job) { j.Main.Model = "a\x00b" }, "invalid action model or skills"},
		{"model CR", func(j *Job) { j.Main.Model = "a\rb" }, "invalid action model or skills"},
		{"missing main skills", func(j *Job) { j.Main.Skills = nil }, "requires at least one skill"},
		{"skill length", func(j *Job) {
			j.Main.Skills = []string{"https://github.com/o/r/tree/main/skills/" + strings.Repeat("a", 4096)}
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
			job := portableJob(command.Comment)
			if err := job.Validate(); err != nil {
				t.Fatal(err)
			}
			test.mutate(&job)
			if err := job.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("settings validation=%v want%q", err, test.want)
			}
		})
	}
	job := portableJob(command.Comment)
	job.Main.Model = strings.Repeat("m", 128)
	job.Main.Effort = strings.Repeat("e", 128)
	job.RuntimeBudget = config.MaxJobTimeout
	if err := job.Validate(); err != nil {
		t.Fatalf("maximum valid settings refused: %v", err)
	}
}

func TestResultJSONRoundTripPreservesValidatedPayload(t *testing.T) {
	job := portableJob(command.Fix)
	job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 1
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.", Changes: []workspace.Change{{Path: "binary", Mode: workspace.Executable, Data: []byte{0, 0xff, '\n'}}}, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
	result.Calls = append(result.Calls, CallUsage{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, NoBlockers: true})
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"approved":true`)) {
		t.Fatalf("approval attestation missing from wire result: %s", data)
	}
	var decoded Result
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(job); err != nil || decoded.Answer != result.Answer || len(decoded.Changes) != 1 || !bytes.Equal(decoded.Changes[0].Data, result.Changes[0].Data) || decoded.Changes[0].Mode != workspace.Executable {
		t.Fatalf("result roundtrip lost validated data: %+v,%v", decoded, err)
	}
}

func TestStatusUsesConfiguredRoundBudget(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	for _, limit := range []int{config.DefaultCriticMaxRounds, config.MaxCriticMaxRounds} {
		job.CriticMaxRounds = limit
		status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseCritic, Round: limit, ReviewPhase: "full"}
		if err := status.Validate(job); err != nil {
			t.Fatalf("valid round %d: %v", limit, err)
		}
		status.Round++
		if err := status.Validate(job); err == nil {
			t.Fatal("accepted status beyond configured rounds")
		}
	}
}

func TestStatusDistinguishesReviewPhases(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Comment))
	for _, tt := range []struct {
		name        string
		phase       StatusPhase
		round       int
		reviewPhase string
		valid       bool
	}{
		{"initial full", PhaseCritic, 1, "full", true},
		{"revised full", PhaseCritic, 2, "full", true},
		{"finding verification", PhaseCritic, 2, "verification", true},
		{"main", PhaseMain, 1, "", true},
		{"missing critic phase", PhaseCritic, 1, "", false},
		{"unknown critic phase", PhaseCritic, 1, "focused", false},
		{"initial verification", PhaseCritic, 1, "verification", false},
		{"unstarted review", PhaseCritic, 0, "full", false},
		{"review phase on main", PhaseMain, 1, "full", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: tt.phase, Round: tt.round, ReviewPhase: tt.reviewPhase}
			if err := status.Validate(job); (err == nil) != tt.valid {
				t.Fatalf("status validation=%v; want valid=%v", err, tt.valid)
			}
		})
	}
	job.Critic = nil
	job.CriticMaxRounds = 0
	status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseCritic, Round: 1, ReviewPhase: "full"}
	if err := status.Validate(job); err == nil || !strings.Contains(err.Error(), "review phase") {
		t.Fatalf("allowed critic status without a critic: %v", err)
	}
}

func TestFixJobRejectsUnsupportedLayoutBeforeExecution(t *testing.T) {
	for _, paths := range [][]string{{"Docs/a", "docs/b"}, {`back\slash`}} {
		job := portableJob(command.Fix)
		for _, name := range paths {
			job.Source.Files = append(job.Source.Files, workspace.File{Path: name, Mode: workspace.Regular})
		}
		if err := job.Validate(); err == nil {
			t.Fatalf("accepted unsupported writable source paths %q", paths)
		}
		job.Action = command.Comment
		if err := job.Validate(); err != nil {
			t.Fatalf("read-only source paths %q should remain supported: %v", paths, err)
		}
	}
}

func TestFixJobIncludesOmittedPathsInUpfrontFoldedLayout(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "ſ/a", Mode: workspace.Regular}}
	job.Source.Omitted = []string{"s"}
	if err := workspace.ValidateSnapshot(job.Source); err != nil {
		t.Fatalf("test does not describe a valid exact source: %v", err)
	}
	runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t)}
	if _, err := runner.Run(context.Background(), job); !errors.Is(err, monitor.ErrTerminalJob) || (!strings.Contains(err.Error(), "collision") && !strings.Contains(err.Error(), "duplicate source path")) {
		t.Fatalf("unsupported fix source was not refused before execution: %v", err)
	}
	job.Action = command.Comment
	if err := job.Validate(); err != nil {
		t.Fatalf("read-only exact source must remain supported: %v", err)
	}
}

func TestResultValidatesChangedPathsAgainstOriginalSource(t *testing.T) {
	job := portableJob(command.Fix)
	job.Source.Files = []workspace.File{{Path: "Foo/stay", Mode: workspace.Regular}}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
		Changes: []workspace.Change{{Path: "foo/new", Mode: workspace.Regular}}, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
	if err := result.Validate(job); err == nil {
		t.Fatal("accepted directory case collision against unchanged source")
	}
	result.Changes = append([]workspace.Change{{Path: "Foo/stay", Delete: true}}, result.Changes...)
	if err := result.Validate(job); err != nil {
		t.Fatalf("rejected directory case rename: %v", err)
	}
}

func TestResultCallUsageRequiresOrderedReviewPhases(t *testing.T) {
	job := portableJob(command.Comment)
	job.Main.Model, job.Main.Effort = "main-model", "high"
	job.Critic = &RoleSettings{Provider: "claude", Model: "critic-model", Effort: "medium"}
	job.CriticMaxRounds = config.DefaultCriticMaxRounds
	main := func(round int) CallUsage {
		return CallUsage{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, Role: "main", Round: round, Complete: true}
	}
	critic := func(round int, phase string, approved bool) CallUsage {
		return CallUsage{Provider: job.Critic.Provider, Model: job.Critic.Model, Effort: job.Critic.Effort, Role: "critic", Round: round, ReviewPhase: phase, Approved: approved, NoBlockers: approved, HasBlockers: !approved, Complete: true}
	}
	for _, test := range []struct {
		name              string
		calls             []CallUsage
		complete, partial bool
	}{
		{"no review calls", nil, false, true},
		{"first full review", []CallUsage{main(1), critic(1, "full", true)}, true, true},
		{"rejected full review", []CallUsage{main(1), critic(1, "full", false)}, false, true},
		{"revised full review", []CallUsage{main(1), critic(1, "full", false), main(2), critic(2, "verification", true), critic(2, "full", true)}, true, true},
		{"rejected verification then revision", []CallUsage{main(1), critic(1, "full", false), main(2), critic(2, "verification", false), main(3), critic(3, "verification", true), critic(3, "full", true)}, true, true},
		{"unapproved verification followed by full", []CallUsage{main(1), critic(1, "full", false), main(2), critic(2, "verification", false), critic(2, "full", true)}, false, false},
		{"approved verification skips full", []CallUsage{main(1), critic(1, "full", false), main(2), critic(2, "verification", true), main(3)}, false, false},
		{"approved full followed by main", []CallUsage{main(1), critic(1, "full", true), main(2)}, false, false},
		{"interrupted first main", []CallUsage{main(1)}, false, true},
		{"interrupted revision", []CallUsage{main(1), critic(1, "full", false), main(2)}, false, true},
		{"interrupted verification", []CallUsage{main(1), critic(1, "full", false), main(2), critic(2, "verification", false)}, false, true},
		{"critic first", []CallUsage{critic(1, "full", true)}, false, false},
		{"missing first full", []CallUsage{main(1), critic(1, "verification", true)}, false, false},
		{"missing revision verification", []CallUsage{main(1), critic(1, "full", false), main(2), critic(2, "full", true)}, false, false},
		{"repeated full", []CallUsage{main(1), critic(1, "full", true), critic(1, "full", true)}, false, false},
		{"main before critic", []CallUsage{main(1), main(2)}, false, false},
		{"skipped round", []CallUsage{main(1), critic(1, "full", false), main(3)}, false, false},
		{"critic round mismatch", []CallUsage{main(1), critic(2, "full", true)}, false, false},
		{"unknown phase", []CallUsage{main(1), critic(1, "focused", true)}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: test.calls}
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
		{"provider attribution", func(call *CallUsage) { call.Provider = "claude" }},
		{"model attribution", func(call *CallUsage) { call.Model = "different-model" }},
		{"effort attribution", func(call *CallUsage) { call.Effort = "low" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := main(1)
			test.mutate(&call)
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.", Calls: []CallUsage{call, critic(1, "full", true)}}
			if err := result.validate(job, false); err == nil {
				t.Fatal("accepted malformed or incorrectly attributed call")
			}
		})
	}
}

func TestResultCallUsageRejectsCriticsAndRepeatedMainWithoutCritic(t *testing.T) {
	job := portableJob(command.Comment)
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

func TestResultCallUsageHonorsConfiguredRoundLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		limit  int
		rounds int
	}{
		{"explicit", 2, 2}, {"configured default", config.DefaultCriticMaxRounds, config.DefaultCriticMaxRounds}, {"maximum", config.MaxCriticMaxRounds, config.MaxCriticMaxRounds},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, test.limit
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer."}
			for round := 1; round <= test.rounds+1; round++ {
				result.Calls = nil
				for proposal := 1; proposal <= round; proposal++ {
					result.Calls = append(result.Calls, CallUsage{Provider: job.Main.Provider, Role: "main", Round: proposal})
					if proposal > 1 {
						result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: "critic", Round: proposal, ReviewPhase: "verification", Approved: true, NoBlockers: true})
					}
					result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Role: "critic", Round: proposal, ReviewPhase: "full", Approved: proposal == round, NoBlockers: proposal == round, HasBlockers: proposal != round})
				}
				if err := result.Validate(job); (err == nil) != (round <= test.rounds) {
					t.Fatalf("round=%d limit=%d result validation=%v", round, test.limit, err)
				}
			}
		})
	}
}

func TestCompleteAuditRequiresNonemptyCoreArtifacts(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	for _, empty := range auditArtifacts {
		if auditArtifactMayBeEmpty(empty) {
			continue
		}
		var files []workspace.File
		for _, name := range auditArtifacts {
			data := []byte("placeholder")
			if name == empty {
				data = nil
			}
			files = append(files, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
		}
		if err := validateResultArtifacts(job, files, true); err == nil || !strings.Contains(err.Error(), "empty action report artifact") {
			t.Fatalf("empty %s=%v", empty, err)
		}
		if err := validateResultArtifacts(job, files, false); err != nil {
			t.Fatalf("partial %s=%v", empty, err)
		}
	}
}

func TestPortableContractsRejectMismatches(t *testing.T) {
	job := portableJob(command.Comment)
	job.Title = strings.Repeat("😀", 256)
	if err := job.Validate(); err != nil {
		t.Fatalf("rejected title at UTF-8 boundary: %v", err)
	}
	valid := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "answer", Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
	if err := valid.Validate(job); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Result)
	}{
		{"version", func(r *Result) { r.Version++ }},
		{"source", func(r *Result) { r.SourceRef = strings.Repeat("b", 40) }},
		{"identity", func(r *Result) { r.Identity.AttemptID = "stale" }},
		{"oversized answer", func(r *Result) { r.Answer = strings.Repeat("x", maxOutputBytes+1) }},
		{"read-only changes", func(r *Result) { r.Changes = []workspace.Change{{Path: "added", Mode: workspace.Regular}} }},
		{"usage", func(r *Result) {
			r.Calls = []CallUsage{{Provider: "codex", Role: "main", Round: 1, Tokens: usage.Tokens{Input: -1}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.change(&result)
			if result.Validate(job) == nil {
				t.Fatal("accepted invalid result")
			}
		})
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"api_key", "read_only_dirs", "/home/", "RunCodex"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("portable job contains local setting %q", private)
		}
	}
	status := Status{Version: ProtocolVersion, Identity: job.Identity, State: "running", Phase: "main", Round: 1, ProgressSequence: 2, LogSequence: 3}
	if err := status.Validate(job); err != nil {
		t.Fatal(err)
	}
	status.ProgressAge = -time.Nanosecond
	if status.Validate(job) == nil {
		t.Fatal("accepted negative progress age")
	}
	status.ProgressAge = 0
	status.Version++
	if status.Validate(job) == nil {
		t.Fatal("accepted incompatible status version")
	}
	status.Version = ProtocolVersion
	status.Identity.AttemptID = "stale"
	if status.Validate(job) == nil {
		t.Fatal("accepted stale status identity")
	}
	status.Identity = job.Identity
	status.State = "unknown"
	if status.Validate(job) == nil {
		t.Fatal("accepted unknown status state")
	}
	status.State, status.Phase = StatusRunning, "unknown"
	if status.Validate(job) == nil {
		t.Fatal("accepted unknown status phase")
	}
}

func TestResultRejectsChangedFileAndArtifactBounds(t *testing.T) {
	job := portableJob(command.Fix)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "answer"}
	for i := 0; i <= workspace.MaxChanges; i++ {
		result.Changes = append(result.Changes, workspace.Change{Path: fmt.Sprintf("file-%d", i), Mode: workspace.Regular})
	}
	if err := result.Validate(job); err == nil {
		t.Fatal("accepted too many changed files")
	}
	job = portableJob(command.SecurityAudit)
	result.Identity, result.SourceRef, result.Changes = job.Identity, job.Source.Head, nil
	for _, name := range auditArtifacts {
		result.Artifacts = append(result.Artifacts, workspace.File{Path: name, Mode: workspace.Regular})
	}
	result.Artifacts[0].Data = make([]byte, maxAuditArtifactBytes+1)
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("oversized artifact validation = %v", err)
	}
}

func TestFixResultIncludesOmittedDirectorySpellingsInFinalLayout(t *testing.T) {
	for _, test := range []struct {
		name, omitted string
		change        workspace.Change
		want          string
	}{
		{"same directory", "dir/link", workspace.Change{Path: "dir/new", Mode: workspace.Regular}, ""},
		{"directory spelling", "dir/link", workspace.Change{Path: "Dir/new", Mode: workspace.Regular}, "directory spelling collision"},
		{"unicode directory spelling", "ſ/link", workspace.Change{Path: "s/new", Mode: workspace.Regular}, "directory spelling collision"},
		{"replace omission", "dir/link", workspace.Change{Path: "dir/link", Mode: workspace.Regular}, "omitted source path"},
		{"delete omission", "dir/link", workspace.Change{Path: "DIR/LINK", Delete: true}, "omitted source path"},
		{"write descendant", "dir/link", workspace.Change{Path: "DIR/link/new", Mode: workspace.Regular}, "collision"},
		{"replace ancestor", "dir/link", workspace.Change{Path: "DIR", Mode: workspace.Regular}, "collision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := portableJob(command.Fix)
			job.Source.Omitted = []string{test.omitted}
			if err := job.Validate(); err != nil {
				t.Fatalf("invalid test source layout: %v", err)
			}
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Done.",
				Changes: []workspace.Change{test.change}, Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
			err := result.Validate(job)
			if test.want == "" {
				if err != nil {
					t.Fatalf("valid sibling change refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("omission layout validation=%v want%q", err, test.want)
			}
		})
	}
}

func TestAuditResultAnswerMustMatchTransferredReport(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	result := completeAuditResult(job)
	report := result.Answer
	if err := result.Validate(job); err != nil {
		t.Fatalf("invalid complete audit fixture: %v", err)
	}
	for _, answer := range []string{"Different unreviewed text.", report + "\n"} {
		result.Answer = answer
		if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "answer differs from REPORT.md") {
			t.Fatalf("mismatched audit answer validation=%v", err)
		}
		if err := result.validate(job, false); err != nil {
			t.Fatalf("incomplete audit diagnostics cannot be retained: %v", err)
		}
	}
}

func TestStatusRequiresMeaningfulStatePhasePairs(t *testing.T) {
	job := portableJob(command.Comment)
	for _, test := range []struct {
		state StatusState
		phase StatusPhase
		round int
		valid bool
	}{
		{StatusReady, PhasePreparing, 0, true},
		{StatusRunning, PhasePreparing, 0, true},
		{StatusRunning, PhaseMain, 1, true},
		{StatusRunning, PhaseCollecting, 1, true},
		{StatusRunning, PhaseUploading, 1, true},
		{StatusStopping, PhaseCleanup, 0, true},
		{StatusCompleted, PhaseFinished, 1, true},
		{StatusFailed, PhaseFinished, 0, true},
		{StatusUnavailable, PhaseFinished, 0, true},
		{StatusReady, PhaseMain, 1, false},
		{StatusReady, PhasePreparing, 1, false},
		{StatusRunning, PhaseFinished, 1, false},
		{StatusStopping, PhaseFinished, 1, false},
		{StatusCompleted, PhaseMain, 1, false},
		{StatusFailed, PhaseMain, 1, false},
		{StatusUnavailable, PhaseCleanup, 1, false},
		{StatusCompleted, PhaseFinished, 0, false},
		{StatusRunning, PhasePreparing, 1, false},
		{StatusRunning, PhaseMain, 0, false},
		{StatusRunning, PhaseCollecting, 0, false},
		{StatusRunning, PhaseUploading, 0, false},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", test.state, test.phase, test.round), func(t *testing.T) {
			status := Status{Version: ProtocolVersion, Identity: job.Identity, State: test.state, Phase: test.phase, Round: test.round}
			err := status.Validate(job)
			if (err == nil) != test.valid || (err != nil && !strings.Contains(err.Error(), "state and phase")) {
				t.Fatalf("status pair validation=%v want valid=%t", err, test.valid)
			}
		})
	}
}

func TestResultAndStatusShareDiagnosticBounds(t *testing.T) {
	job := portableJob(command.Comment)
	tooMany := make([]Diagnostic, 101)
	for i := range tooMany {
		tooMany[i] = Diagnostic{Code: "code", Message: "message"}
	}
	for _, test := range []struct {
		name        string
		diagnostics []Diagnostic
		want        string
	}{
		{"valid", []Diagnostic{{Code: "incomplete_usage", Message: "Provider omitted usage."}}, ""},
		{"maximum count", tooMany[:100], ""},
		{"maximum message", []Diagnostic{{Code: "code", Message: strings.Repeat("x", 8<<10)}}, ""},
		{"empty code", []Diagnostic{{Code: "", Message: "message"}}, "invalid action diagnostic"},
		{"unsafe code", []Diagnostic{{Code: "../private", Message: "message"}}, "invalid action diagnostic"},
		{"nul message", []Diagnostic{{Code: "code", Message: "nul\x00message"}}, "invalid action diagnostic"},
		{"invalid UTF8 message", []Diagnostic{{Code: "code", Message: string([]byte{0xff})}}, "invalid action diagnostic"},
		{"oversized message", []Diagnostic{{Code: "code", Message: strings.Repeat("x", (8<<10)+1)}}, "invalid action diagnostic"},
		{"too many", tooMany, "too many action diagnostics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Answer.",
				Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}, Diagnostics: test.diagnostics}
			status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseMain, Round: 1, Diagnostics: test.diagnostics}
			for name, err := range map[string]error{"result": result.Validate(job), "status": status.Validate(job)} {
				if (err == nil) != (test.want == "") || (err != nil && !strings.Contains(err.Error(), test.want)) {
					t.Fatalf("%s diagnostics validation=%v want%q", name, err, test.want)
				}
			}
		})
	}
}

func TestJobSubjectMatchesComparisonForEveryAction(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.SecurityAudit, command.Review, command.Fix} {
		for _, subject := range []string{"Issue", "PullRequest"} {
			for _, comparison := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/comparison=%t", action, subject, comparison), func(t *testing.T) {
					job := portableJob(action)
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
	job := portableJob(command.Review)
	job.Critic = &RoleSettings{Provider: "claude", Model: "critic-model", Effort: "high"}
	job.CriticMaxRounds = config.DefaultCriticMaxRounds
	job.Source.HistoryRequested = true
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

func TestAuditResultRejectsRenderedOverflowButKeepsPartialReport(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	result := completeAuditResult(job)
	result.Answer = strings.Repeat("a\u0085", 19000)
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "REPORT.md" {
			result.Artifacts[i].Data = []byte(result.Answer)
		}
	}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "answer") {
		t.Fatalf("unpublishable report imported as complete: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("bounded partial report cannot be retained: %v", err)
	}
}

func TestPartialAuditReportPreservesByteLimit(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	for _, size := range []int{maxOutputBytes, maxOutputBytes + 1} {
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
			Artifacts: []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: bytes.Repeat([]byte("x"), size)}},
		}
		if err := result.validate(job, false); (err == nil) != (size == maxOutputBytes) || err != nil && !strings.Contains(err.Error(), "report artifacts exceed limits") {
			t.Fatalf("partial report size %d accepted or misclassified: %v", size, err)
		}
	}
}

func TestStatusBoundsRemainingRuntimeWithoutRenewingBudget(t *testing.T) {
	job := portableJob(command.Comment)
	job.RuntimeBudget = time.Minute
	for _, remaining := range []time.Duration{-1, 0, time.Nanosecond, time.Minute, time.Minute + 1} {
		status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseMain, Round: 1, RuntimeRemaining: remaining}
		valid := remaining >= 0 && remaining <= job.RuntimeBudget
		if err := status.Validate(job); (err == nil) != valid {
			t.Fatalf("remaining=%s validation=%v want valid=%t", remaining, err, valid)
		}
		if !valid {
			continue
		}
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Status
		if err := json.Unmarshal(data, &decoded); err != nil || decoded.RuntimeRemaining != remaining || decoded.Validate(job) != nil || !strings.Contains(string(data), `"runtime_remaining_ns":`) {
			t.Fatalf("remaining runtime lost in transport: %s,%v", data, err)
		}
	}
	job.RuntimeBudget = time.Nanosecond
	status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusReady, Phase: PhasePreparing, RuntimeRemaining: 2 * time.Nanosecond}
	if status.Validate(job) == nil {
		t.Fatal("status renewed the supplied runtime budget")
	}
}
