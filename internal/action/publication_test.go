package action

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

func TestCriticCapNotice(t *testing.T) {
	for _, test := range []struct {
		name string
		cap  *state.CriticCap
		want string
	}{
		{name: "normal"},
		{name: "one round nits", cap: &state.CriticCap{MaxRounds: 1}, want: "Critic round limit reached (1). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic did not approve."},
		{name: "verification blockers with full approval", cap: &state.CriticCap{MaxRounds: 3, HasBlockers: true, FullApproved: true}, want: "Critic round limit reached (3). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic reported approval, but required verification findings were not cleared. A critic review phase of the final candidate reported blockers."},
		{name: "full blockers", cap: &state.CriticCap{MaxRounds: config.MaxCriticMaxRounds, HasBlockers: true}, want: "Critic round limit reached (20). Normal critic acceptance was not reached; unresolved findings may remain. The fresh full critic did not approve. A critic review phase of the final candidate reported blockers."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := criticCapNotice(test.cap)
			if err != nil || got != test.want {
				t.Fatalf("notice=%q error=%v; want %q", got, err, test.want)
			}
		})
	}
	for _, maximum := range []int{-1, 0, config.MaxCriticMaxRounds + 1} {
		if got, err := criticCapNotice(&state.CriticCap{MaxRounds: maximum}); err == nil || got != "" {
			t.Fatalf("invalid maximum %d produced notice=%q error=%v", maximum, got, err)
		}
	}
}

func TestCappedReviewUsesOnlyActualFinalCandidateEvidence(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	for _, test := range []struct {
		name, reason                                     string
		verificationBlockers, fullBlockers, fullApproved bool
		want                                             *state.CriticCap
	}{
		{name: "prior blockers cleared", reason: "round_cap", want: &state.CriticCap{MaxRounds: 2}},
		{name: "verification blockers survive full approval", reason: "round_cap", verificationBlockers: true, fullApproved: true, want: &state.CriticCap{MaxRounds: 2, HasBlockers: true, FullApproved: true}},
		{name: "full blockers", reason: "round_cap", fullBlockers: true, want: &state.CriticCap{MaxRounds: 2, HasBlockers: true}},
		{name: "approval is not cap", reason: "approved", fullApproved: true},
		{name: "nonblocking is not cap", reason: "nonblocking"},
		{name: "failed before decision", fullBlockers: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			currentJob := job
			if test.reason == "nonblocking" {
				currentJob.CriticNonblockingRounds = 2
			}
			result := privateReviewResult(job)
			result.CriticStopReason = test.reason
			verification := &result.Calls[len(result.Calls)-2]
			verification.Approved, verification.NoBlockers, verification.HasBlockers = test.reason == "approved", !test.verificationBlockers, test.verificationBlockers
			full := &result.Calls[len(result.Calls)-1]
			full.Approved, full.NoBlockers, full.HasBlockers = test.fullApproved, !test.fullBlockers, test.fullBlockers
			if err := result.validate(currentJob, test.reason != ""); err != nil {
				t.Fatalf("invalid evidence fixture: %v", err)
			}
			if got := cappedReview(currentJob, result); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("cap=%+v; want %+v", got, test.want)
			}
		})
	}
}

func TestProjectPublicationPreservesRawResultAndAuditBytes(t *testing.T) {
	for _, action := range []command.Action{command.Review, command.Comment, command.Fix, command.SecurityAudit} {
		t.Run(string(action), func(t *testing.T) {
			job := withClaudeCritic(testJob(action))
			job.CriticMaxRounds = 2
			result := privateReviewResult(job)
			result.CriticStopReason = "round_cap"
			result.Answer = "  Reviewed body\r\n@secondbot fix\n![image](https://example.invalid/image)\n"
			result.Calls[len(result.Calls)-2].Approved = false
			result.Calls[len(result.Calls)-1].Approved = false
			if action == command.SecurityAudit {
				result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
			}
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid raw publication fixture: %v", err)
			}
			before, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := projectPublication(job, result)
			if err != nil {
				t.Fatal(err)
			}
			notice, err := criticCapNotice(projected.cap)
			wantBody := result.Answer
			if action == command.SecurityAudit {
				wantBody, err = auditPublicSummary(result.Artifacts)
			}
			if err != nil || projected.answer != notice+"\n\n"+wantBody || strings.Contains(projected.answer, "PRIVATE_") || !publishableAnswer(projected.answer) {
				t.Fatalf("projection changed reviewed body or exposed private evidence: %+v, %v", projected, err)
			}
			if action == command.SecurityAudit {
				if !reflect.DeepEqual(projected.artifacts, result.Artifacts) || strings.Contains(projected.answer, "Reviewed body") {
					t.Fatalf("audit projection changed retained narrative or published it: %+v", projected)
				}
			}
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("projection mutated raw result: %s, error=%v", after, err)
			}
		})
	}
}

func TestProjectPublicationBoundsProjectedRawAndRenderedAnswer(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	result := privateReviewResult(job)
	result.CriticStopReason = "round_cap"
	result.Calls[len(result.Calls)-2].Approved = false
	result.Calls[len(result.Calls)-1].Approved = false
	notice, err := criticCapNotice(cappedReview(job, result))
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(inertModelOutput(notice+"\n\nx")) - 1
	result.Answer = strings.Repeat("x", maxOutputBytes-overhead)
	projected, err := projectPublication(job, result)
	if err != nil || len(inertModelOutput(projected.answer)) != maxOutputBytes {
		t.Fatalf("exact projected boundary refused: %d, %v", len(inertModelOutput(projected.answer)), err)
	}
	result.Answer += "x"
	if len(notice)+2+len(result.Answer) > maxOutputBytes || !publishableAnswer(result.Answer) {
		t.Fatal("rendered-overflow fixture does not isolate rendering expansion")
	}
	if _, err := projectPublication(job, result); err == nil {
		t.Fatal("accepted one-byte projected rendered overflow")
	}
	result.Answer = "x" + strings.Repeat(" ", maxOutputBytes-1)
	if !publishableAnswer(result.Answer) || len(inertModelOutput(notice+"\n\n"+result.Answer)) > maxOutputBytes {
		t.Fatal("raw-overflow fixture does not isolate raw byte expansion")
	}
	if _, err := projectPublication(job, result); err == nil || len(result.Answer) != maxOutputBytes {
		t.Fatalf("raw overflow accepted or reviewed text truncated: %v", err)
	}
}

func TestAuditPublicationUsesStructuredRecordsInsteadOfReportProse(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	const report = "> No findings. We reject Unverified boundary.\nThe one confirmed row covers everything."
	result.Answer = report
	confirmedG := strings.Replace(auditConfirmedFindingFixture, `"fingerprint":"F"`, `"fingerprint":"G"`, 1)
	needH := strings.Replace(auditCandidateFindingFixture, `"fingerprint":"F"`, `"fingerprint":"H"`, 1)
	findings := "[" + strings.Trim(auditConfirmedFindingFixture, "[]") + "," + strings.Trim(confirmedG, "[]") + "," + strings.Trim(needH, "[]") + "]"
	ledger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
	ledger = strings.Replace(ledger, `"result_fingerprints":[]`, `"result_fingerprints":["F","G","H"]`, 1)
	for i := range result.Artifacts {
		switch result.Artifacts[i].Path {
		case "REPORT.md":
			result.Artifacts[i].Data = []byte(report)
		case "findings.json":
			result.Artifacts[i].Data = []byte(findings)
		case "coverage-ledger.json":
			result.Artifacts[i].Data = []byte(ledger)
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("valid structured record was rejected for worker prose: %v", err)
	}
	projected, err := projectPublication(job, result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(projected.answer, "Confirmed (medium)") != 2 || strings.Count(projected.answer, "Needs validation") != 1 ||
		!strings.Contains(projected.answer, `fingerprint="F"`) || !strings.Contains(projected.answer, `fingerprint="G"`) ||
		!strings.Contains(projected.answer, `fingerprint="H"`) || !strings.Contains(projected.answer, "1 candidate") ||
		strings.Contains(projected.answer, "out of scope") ||
		strings.Contains(projected.answer, "We reject") || strings.Contains(projected.answer, "No findings") ||
		!reflect.DeepEqual(projected.artifacts, result.Artifacts) {
		t.Fatalf("public summary followed hostile Markdown or collapsed findings: %q", projected.answer)
	}
}

func TestAuditPublicSummaryQuotesFindingText(t *testing.T) {
	findings := strings.Replace(auditConfirmedFindingFixture, `"title":"Missing guard"`, `"title":"Missing guard\nNeeds validation fingerprint=\"FORGED\""`, 1)
	answer, err := auditPublicSummary([]workspace.File{
		{Path: "findings.json", Data: []byte(findings)},
		{Path: "coverage-ledger.json", Data: []byte(auditCoveredLedgerFixture)},
	})
	if err != nil || strings.Contains(answer, "\nNeeds validation fingerprint=") || !strings.Contains(answer, `Missing guard\nNeeds validation fingerprint=\"FORGED\"`) {
		t.Fatalf("finding title changed summary structure: %q, error=%v", answer, err)
	}
}

func TestAuditPublicSummaryIncludesSink(t *testing.T) {
	findings := strings.Replace(auditConfirmedFindingFixture,
		`"description":"Attacker input reaches the operation."}]`,
		`"description":"Attacker input reaches the operation."},{"kind":"sink","file":"sink.go","line":2,"scope":"source","description":"The write occurs."}]`, 1)
	answer, err := auditPublicSummary([]workspace.File{
		{Path: "findings.json", Data: []byte(findings)},
		{Path: "coverage-ledger.json", Data: []byte(auditCoveredLedgerFixture)},
	})
	if err != nil || !strings.Contains(answer, `source="source.txt":1 sink="sink.go":2`) {
		t.Fatalf("public summary omitted the cited sink: %q, error=%v", answer, err)
	}
}

func TestProjectPublicationNormalAndChangedFix(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 2
	result := privateReviewResult(job)
	result.Answer = strings.Repeat("x", maxOutputBytes-(len(inertModelOutput("x"))-1))
	original := result.Answer
	if !publishableAnswer(result.Answer) {
		t.Fatal("unused summary fixture exceeds its raw publication limit")
	}
	normal, err := projectPublication(job, result)
	if err != nil || normal.answer != result.Answer || normal.cap != nil || !reflect.DeepEqual(normal.artifacts, result.Artifacts) {
		t.Fatalf("normal publication changed: %+v, %v", normal, err)
	}
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("changed")}}
	for _, reason := range []string{"approved", "round_cap"} {
		result.CriticStopReason = reason
		result.Calls[len(result.Calls)-2].Approved = reason == "approved"
		if err := result.Validate(job); err != nil {
			t.Fatalf("invalid changed-fix fixture: %v", err)
		}
		projected, err := projectPublication(job, result)
		if err != nil {
			t.Fatal(err)
		}
		_, err = criticCapNotice(projected.cap)
		if err != nil || projected.answer != "" || result.Answer != original {
			t.Fatalf("changed fix projected unused summary: %+v, error=%v", projected, err)
		}
	}
}

func TestPublishedRefutationsCheckOnlyActualPublication(t *testing.T) {
	const private = "Private objection\nand evidence."
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations[0].Text = private
	result.Answer = "Public reply\r\nPrivate objection\r\nand evidence."
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("line-ending-normalized private reply was accepted: %v", err)
	}
	result.Answer = "Public reply\n> Private objection\n> and evidence."
	projected, err = projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("quoted private reply was accepted: %v", err)
	}
	result.Answer = "Public reply\n> > Private objection\n> > and evidence."
	projected, err = projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("nested quoted private reply was accepted: %v", err)
	}

	job.Action = command.Fix
	result.Answer = private
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("safe change\n")}}
	projected, err = projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
		t.Fatalf("unused Fix summary blocked a safe change: %v", err)
	}
	result.Changes[0].Data = []byte("Private objection\r\nand evidence.")
	if rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatal("private refutation in changed source was accepted")
	}

	job.Action = command.SecurityAudit
	result.Changes = nil
	result.Answer = "Public report."
	result.Artifacts = []workspace.File{{Path: "architecture.md", Mode: workspace.Regular, Data: []byte("Private objection\r\nand evidence.")}}
	projected = publication{answer: "Public summary", artifacts: result.Artifacts}
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("private audit narrative triggered a public-output guard: %v", err)
	}
}

func TestPublishedRefutationsRejectShortWholeReply(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	result := privateReviewResult(job)
	result.Refutations[0].Text = "Secret abc123"
	result.Answer = result.Refutations[0].Text
	for _, capped := range []bool{false, true} {
		if capped {
			result.CriticStopReason = "round_cap"
			result.Calls[len(result.Calls)-1].Approved = false
		}
		if err := result.Validate(job); err != nil {
			t.Fatal(err)
		}
		projected, err := projectPublication(job, result)
		if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
			t.Fatalf("short whole refutation reached public reply (capped=%t): %v", capped, err)
		}
	}
}

func TestPublishedRefutationsComparePrivateTextWithoutNUL(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations[0].Text = "The private review\x00 sentence stays internal."
	result.Answer = "Public reply: The private review sentence stays internal."
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("NUL-stripped private sentence reached public reply: %v", err)
	}
	job.Source.Files[0].Data = []byte("The private review sentence stays internal.\n")
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("preexisting source quote was blocked: %v", err)
	}
}

func TestPublishedRefutationsWholeTextNeedsWordBoundaries(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations[0].Text = "The endpoint supports auth"
	for _, answer := range []string{
		"The endpoint supports authorization is useful.",
		"PrefixThe endpoint supports auth is useful.",
	} {
		result.Answer = answer
		projected, err := projectPublication(job, result)
		if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
			t.Fatalf("longer public word was treated as a copy: %q, %v", answer, err)
		}
	}
	for _, test := range []struct{ private, answer string }{
		{"The endpoint supports café", "The endpoint supports cafés is useful."},
		{"The endpoint supports Ελληνικά", "The endpoint supports Ελληνικάς is useful."},
		{"The endpoint supports система", "The endpoint supports системами is useful."},
	} {
		result.Refutations[0].Text, result.Answer = test.private, test.answer
		projected, err := projectPublication(job, result)
		if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
			t.Fatalf("longer Unicode word was treated as a copy: %q, %v", test.answer, err)
		}
	}
	result.Refutations[0].Text = "The endpoint supports auth"
	result.Answer = "Public reply: The endpoint supports auth is useful."
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("whole private text with word boundaries was accepted: %v", err)
	}
}

func TestPublishedRefutationsWholeTextInUnspacedScript(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations[0].Text = strings.Repeat("私人審查結論", 5)
	result.Answer = "公開說明" + result.Refutations[0].Text + "可供參考"
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("private text embedded in unspaced public prose was accepted: %v", err)
	}
}

func TestPublishedRefutationsSentenceInUnspacedScript(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	second := strings.Repeat("第二段私人結論不可公開", 3)
	result.Refutations[0].Text = "第一段私人審查內容。" + second
	result.Answer = "公開說明前綴" + second
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("private sentence embedded in unspaced public prose was accepted: %v", err)
	}
}

func TestPublishedRefutationsIgnoreGeneratedCapNotice(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	result := privateReviewResult(job)
	result.CriticStopReason = "round_cap"
	result.Calls[len(result.Calls)-1].Approved = false
	result.Refutations[0].Text = "Critic round limit reached"
	result.Answer = "Public response unrelated to review process."
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	projected, err := projectPublication(job, result)
	if err != nil || !strings.Contains(projected.answer, result.Refutations[0].Text) {
		t.Fatalf("capped publication omitted generated notice: %+v, %v", projected, err)
	}
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("generated notice triggered private-copy guard: %v", err)
	}
}

func TestPublishedRefutationsFoldExpandedUnicodeCase(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations[0].Text = "The private Straße example should not be copied into a public reply."
	result.Answer = "Public reply. THE PRIVATE STRASSE EXAMPLE SHOULD NOT BE COPIED INTO A PUBLIC REPLY."
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("expanded Unicode case fold missed a private sentence: %v", err)
	}
}

func TestPublishedRefutationsRejectMarkedSentenceCopies(t *testing.T) {
	const private = "The first private review sentence stays internal. The second private review sentence stays internal."
	const copied = "The second private review sentence stays internal."
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	result.Refutations[0].Text = private
	result.Answer = "Public reply:\n# " + copied
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("Markdown heading copied private sentence: %v", err)
	}

	job.Action = command.Fix
	result.Answer = "Done"
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("// " + copied + "\n")}}
	projected, err = projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("source comment copied private sentence: %v", err)
	}
	job.Source.Files[0].Data = []byte("// " + copied + "\n")
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("existing source comment blocked an unchanged quote: %v", err)
	}
}

func TestPublishedRefutationsRejectPrefixedSentenceCopies(t *testing.T) {
	const private = "The first private review sentence stays internal. The second private review sentence stays internal."
	const copied = "The second private review sentence stays internal."
	for _, action := range []command.Action{command.Comment, command.Fix} {
		t.Run(string(action), func(t *testing.T) {
			job := withClaudeCritic(testJob(action))
			result := privateReviewResult(job)
			result.Refutations[0].Text = private
			setPublic := func(text string) {
				if action == command.Fix {
					result.Answer = "Done"
					result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("// " + text + "\n")}}
				} else {
					result.Answer = text
				}
			}
			setPublic("Public reply " + copied)
			projected, err := projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
				t.Fatalf("prefixed private sentence passed publication guard: %v", err)
			}
			job.Source.Files[0].Data = []byte(copied + "\n")
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("existing source quote was blocked: %v", err)
			}
			job.Source.Files[0].Data = []byte("unrelated source\n")
			setPublic("Public reply Prefix" + copied)
			projected, err = projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
				t.Fatalf("private text within a larger word was treated as a copy: %v", err)
			}
		})
	}
}

func TestPublishedRefutationsRejectInteriorSentenceCopies(t *testing.T) {
	const copied = "The second private review sentence stays internal"
	for _, action := range []command.Action{command.Comment, command.Fix} {
		t.Run(string(action), func(t *testing.T) {
			job := withClaudeCritic(testJob(action))
			result := privateReviewResult(job)
			result.Refutations[0].Text = "The first private review sentence stays internal. " + copied + "."
			setPublic := func(text string) {
				if action == command.Fix {
					result.Answer = "Done"
					result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte(text)}}
				} else {
					result.Answer = text
				}
			}
			setPublic("Public lead: " + copied + " with more public context.")
			projected, err := projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
				t.Fatalf("interior private sentence passed publication guard: %v", err)
			}
			job.Source.Files[0].Data = []byte(copied + "\n")
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("existing source quote was blocked: %v", err)
			}
			job.Source.Files[0].Data = []byte("ordinary source\n")
			setPublic("Public lead: " + copied + "ary with more public context.")
			projected, err = projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
				t.Fatalf("longer public word was treated as a sentence copy: %v", err)
			}
		})
	}
}

func TestPublishedRefutationsRejectInlineMarkdownSentenceCopies(t *testing.T) {
	const private = "The first private review sentence stays internal. The second private review sentence stays internal."
	const copied = "The second private review sentence stays internal."
	for _, marker := range []string{"**", "_", "`"} {
		t.Run(marker, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			result := privateReviewResult(job)
			result.Refutations[0].Text = private
			result.Answer = "Public reply: " + marker + copied + marker
			projected, err := projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
				t.Fatalf("inline Markdown copied a private sentence: %v", err)
			}
			job.Source.Files[0].Data = []byte(copied + "\n")
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("existing public source quote was blocked: %v", err)
			}
		})
	}
}

func TestPublishedRefutationsRejectQuotedAndIdeographicSentenceCopies(t *testing.T) {
	for _, test := range []struct {
		name, private, answer, sourceQuote string
	}{
		{
			name:        "quoted sentence",
			private:     "The first private review sentence stays internal. The second private review sentence stays internal.",
			answer:      `Public reply: "The second private review sentence stays internal."`,
			sourceQuote: "The second private review sentence stays internal.",
		},
		{
			name:        "ideographic full stop",
			private:     "The first private review sentence stays internal。The second private review sentence stays internal。",
			answer:      "Public reply: The second private review sentence stays internal。",
			sourceQuote: "The second private review sentence stays internal。",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			result := privateReviewResult(job)
			result.Refutations[0].Text = test.private
			result.Answer = test.answer
			if err := result.Validate(job); err != nil {
				t.Fatal(err)
			}
			projected, err := projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
				t.Fatalf("copied private sentence reached publication: %q, %v", projected.answer, err)
			}
			job.Source.Files[0].Data = []byte(test.sourceQuote)
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("existing source quote was blocked: %v", err)
			}
		})
	}
}

func TestPublishedRefutationsRejectAuditTitleCopies(t *testing.T) {
	job := withClaudeCritic(testJob(command.SecurityAudit))
	result := privateReviewResult(job)
	result.Refutations[0].Text = "Private objection\nand evidence."
	result.Answer = "Audit report\n"
	result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), result.Answer)
	title, err := json.Marshal(result.Refutations[0].Text)
	if err != nil {
		t.Fatal(err)
	}
	findings := strings.Replace(auditCandidateFindingFixture, `"title":"Unverified boundary"`, `"title":`+string(title), 1)
	ledger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
	ledger = strings.Replace(ledger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
	for i := range result.Artifacts {
		switch result.Artifacts[i].Path {
		case "findings.json":
			result.Artifacts[i].Data = []byte(findings)
		case "coverage-ledger.json":
			result.Artifacts[i].Data = []byte(ledger)
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	projected, err := projectPublication(job, result)
	if err != nil || !strings.Contains(projected.answer, `Private objection\nand evidence.`) || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("quoted audit title reached public reply: %q, %v", projected.answer, err)
	}
	result.Refutations[0].Text = "The first private audit finding stays internal。The second private audit finding stays internal。"
	title, err = json.Marshal("The second private audit finding stays internal。")
	if err != nil {
		t.Fatal(err)
	}
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "findings.json" {
			result.Artifacts[i].Data = []byte(strings.Replace(auditCandidateFindingFixture, `"title":"Unverified boundary"`, `"title":`+string(title), 1))
		}
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	projected, err = projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("ideographic private sentence in audit title reached public reply: %q, %v", projected.answer, err)
	}
	result.Refutations[0].Text = "F"
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("short generic fingerprint matched an unrelated refutation: %v", err)
	}
	result.Refutations[0].Text = "Secret abc123"
	shortTitle, err := json.Marshal(result.Refutations[0].Text)
	if err != nil {
		t.Fatal(err)
	}
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "findings.json" {
			result.Artifacts[i].Data = []byte(strings.Replace(auditCandidateFindingFixture, `"title":"Unverified boundary"`, `"title":`+string(shortTitle), 1))
		}
	}
	projected, err = projectPublication(job, result)
	if err != nil || !strings.Contains(projected.answer, "Secret abc123") || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("short private audit title reached public reply: %q, %v", projected.answer, err)
	}
}

func TestPublishedRefutationsAllowAuditTitleQuotedFromSource(t *testing.T) {
	const quote = "This audit title quotes existing public source text."
	job := withClaudeCritic(testJob(command.SecurityAudit))
	job.Source.Files[0].Data = []byte(quote + "\n")
	result := privateReviewResult(job)
	result.Refutations[0].Text = quote
	result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), "Audit report\n")
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "findings.json" {
			result.Artifacts[i].Data = []byte(strings.Replace(auditCandidateFindingFixture, `"title":"Unverified boundary"`, `"title":"`+quote+`"`, 1))
		}
	}
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
		t.Fatalf("public source quote blocked in audit title: %v", err)
	}
}

func TestPublishedRefutationsAuditTitleRejectsUnprovedCopyAfterScanBudget(t *testing.T) {
	job := withClaudeCritic(testJob(command.SecurityAudit))
	job.CriticMaxRounds = 3
	var quotes []string
	for i := range 982 {
		quotes = append(quotes, fmt.Sprintf("Existing source quote %03d is already public.", i))
	}
	const private = "This private audit title must remain unpublished."
	job.Source.Files[0].Data = []byte(strings.Join(quotes, "\n") + "\n" + strings.Repeat("f", 1<<20) + "\n" + private)
	result := privateReviewResult(job)
	result.Refutations = []Refutation{{Round: 2, Text: strings.Join(quotes, "\n")}, {Round: 3, Text: private}}
	result.Calls[4].Approved, result.Calls[4].NoBlockers, result.Calls[4].HasBlockers = false, false, true
	result.Calls = append(result.Calls,
		CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 3},
		CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: 3, ReviewPhase: reviewVerification, ReviewComplete: true, Approved: true, NoBlockers: true},
		CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: 3, ReviewPhase: reviewFull, ReviewComplete: true, Approved: true, NoBlockers: true},
	)
	type finding struct{ Verdict, Fingerprint, Title string }
	findings := make([]finding, 0, len(quotes)+1)
	for i, quote := range quotes {
		findings = append(findings, finding{Verdict: "confirmed", Fingerprint: fmt.Sprint(i), Title: quote})
	}
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	result.Artifacts = []workspace.File{{Path: "findings.json", Mode: workspace.Regular, Data: data}}
	// This isolates the publication guard; the audit artifact set is intentionally incomplete.
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("partial result and refutations are not structurally valid: %v", err)
	}
	if err := rejectPublishedRefutations(job, result, publication{answer: "Audit summary"}); err != nil {
		t.Fatalf("public audit titles exhausted the source scan budget: %v", err)
	}
	findings = append(findings, finding{Verdict: "confirmed", Fingerprint: "partial", Title: "Background: " + private})
	data, err = json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	result.Artifacts[0].Data = data
	if err := rejectPublishedRefutations(job, result, publication{answer: "Audit summary"}); err == nil {
		t.Fatal("partial private audit title passed without source proof after scan budget")
	}
	findings[len(quotes)] = finding{Verdict: "confirmed", Fingerprint: "private", Title: private}
	data, err = json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	result.Artifacts[0].Data = data
	if err := result.validate(job, false); err != nil {
		t.Fatalf("final partial result and refutations are not structurally valid: %v", err)
	}
	if err := rejectPublishedRefutations(job, result, publication{answer: "Audit summary"}); err == nil {
		t.Fatal("exact private audit title passed without source proof after scan budget")
	}
}

func TestPublishedRefutationsFixAllowsQuotedSourceButRejectsNewCopy(t *testing.T) {
	const quoted = "Existing source quote remains unchanged."
	const private = "This private reviewer conclusion must remain unpublished."
	job := withClaudeCritic(testJob(command.Fix))
	job.Source.Files[0].Data = []byte(quoted + "\nold detail.\n")
	result := privateReviewResult(job)
	result.Refutations[0].Text = quoted + " " + private
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte(quoted + "\nnew detail.\n")}}
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
		t.Fatalf("unrelated edit to quoted source blocked: %v", err)
	}

	result.Changes = []workspace.Change{
		{Path: "source.txt", Delete: true},
		{Path: "renamed.txt", Mode: workspace.Regular, Data: []byte(quoted + "\nold detail.\n")},
	}
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("renamed quoted source blocked: %v", err)
	}
	result.Changes[1].Data = append(result.Changes[1].Data, private...)
	if err := rejectPublishedRefutations(job, result, projected); err == nil {
		t.Fatal("new private sentence in renamed source was accepted")
	}
}

func TestPublishedRefutationsAllowSourceQuoteInPublicReply(t *testing.T) {
	const quoted = "This quoted source sentence was already public."
	const novel = "This novel private sentence must not be published."
	for _, action := range []command.Action{command.Comment, command.Review} {
		t.Run(string(action), func(t *testing.T) {
			job := withClaudeCritic(testJob(action))
			if action == command.Review {
				job.Source.BaseFiles[0].Data = []byte(quoted + "\n")
			} else {
				job.Source.Files[0].Data = []byte(quoted + "\n")
			}
			result := privateReviewResult(job)
			result.Refutations[0].Text = quoted
			result.Answer = "Public reply: " + quoted
			projected, err := projectPublication(job, result)
			if err != nil {
				t.Fatal(err)
			}
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("public source quote blocked: %v", err)
			}
			result.Answer = quoted
			projected, err = projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
				t.Fatalf("whole reply quoted from public source was blocked: %v", err)
			}
			result.Refutations[0].Text = novel
			result.Answer = "Public reply: " + novel
			projected, err = projectPublication(job, result)
			if err != nil {
				t.Fatal(err)
			}
			if err := rejectPublishedRefutations(job, result, projected); err == nil {
				t.Fatal("novel private copy accepted")
			}
		})
	}
}

func TestPublishedRefutationsAllowPreparedThreadQuote(t *testing.T) {
	const quoted = "This sentence was already in the selected GitHub thread."
	largeSource := []byte(strings.Repeat("x", 4<<20+1))
	for _, test := range []struct {
		name  string
		set   func(*Job, string)
		large bool
	}{
		{"title", func(j *Job, text string) { j.Title = text }, false},
		{"description", func(j *Job, text string) { j.Description = text }, true},
		{"notes", func(j *Job, text string) { j.Notes = text }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			if test.large {
				job.Source.Files[0].Data = largeSource // Use the uncached source search path.
			}
			test.set(&job, quoted)
			result := privateReviewResult(job)
			result.Refutations[0].Text = quoted
			result.Answer = "Public reply: " + quoted
			projected, err := projectPublication(job, result)
			if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
				t.Fatalf("preexisting GitHub quote blocked: %v", err)
			}
			test.set(&job, "")
			if err := rejectPublishedRefutations(job, result, projected); err == nil {
				t.Fatal("new private quote accepted without thread proof")
			}
		})
	}
}

func TestPublishedRefutationsDoNotTreatBinarySourceAsPublicQuote(t *testing.T) {
	const quote = "This private review sentence must stay unpublished."
	for _, padding := range []int{0, 4 << 20} {
		t.Run(fmt.Sprint(padding), func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			result := privateReviewResult(job)
			result.Refutations[0].Text = quote
			result.Answer = "Public reply: " + quote
			projected, err := projectPublication(job, result)
			if err != nil {
				t.Fatal(err)
			}
			prefix := strings.Repeat("x", padding)
			job.Source.Files[0].Data = []byte(prefix + strings.Join(strings.Split(quote, ""), "\x00"))
			if err := rejectPublishedRefutations(job, result, projected); err == nil {
				t.Fatal("binary source falsely proved the private quote was public")
			}
			job.Source.Files[0].Data = []byte(prefix + "\x00" + quote)
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("source quote after NUL was blocked: %v", err)
			}
			job.Source.Files[0].Data = []byte(prefix + quote)
			if err := rejectPublishedRefutations(job, result, projected); err != nil {
				t.Fatalf("text source quote was blocked: %v", err)
			}
		})
	}
}

func TestPublishedRefutationsSourceSearchDoesNotJoinFiles(t *testing.T) {
	const private = "A private sentence must not be formed by adjoining source files."
	job := withClaudeCritic(testJob(command.Comment))
	job.Source.Files = []workspace.File{
		{Path: "first.txt", Mode: workspace.Regular, Data: []byte("A private sentence must not be formed by ")},
		{Path: "second.txt", Mode: workspace.Regular, Data: []byte("adjoining source files.")},
	}
	result := privateReviewResult(job)
	result.Refutations[0].Text = private
	result.Answer = "Public reply: " + private
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("private sentence crossing source files passed: %v", err)
	}
}

func TestPublishedRefutationsFixNULSourceSeparator(t *testing.T) {
	const private = "foo\x00bar is a private review conclusion that must not enter a Fix."
	job := withClaudeCritic(testJob(command.Fix))
	job.Source.Files = []workspace.File{{Path: "foo", Mode: workspace.Regular, Data: []byte("bar is a private review conclusion that must not enter a Fix.\n")}}
	result := privateReviewResult(job)
	result.Refutations[0].Text = private
	result.Changes = []workspace.Change{{Path: "edit.bin", Mode: workspace.Regular, Data: []byte(private)}}
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("cross-element NUL quote passed the Fix guard: %v", err)
	}
	job.Source.Files[0].Data = []byte(private)
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("same-element binary quote was blocked: %v", err)
	}
}

func TestPublishedRefutationsFixManySourceQuotes(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	var quotes []string
	for i := range 300 {
		quotes = append(quotes, fmt.Sprintf("Existing documented review sentence number %03d remains public.", i))
	}
	quoted := strings.Join(quotes, "\n")
	job.Source.Files[0].Data = []byte(strings.Repeat("f", 256<<10) + "\n" + quoted + "\n")
	result := privateReviewResult(job)
	result.Refutations[0].Text = quoted
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte(quoted + "\nupdated.\n")}}
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) != nil {
		t.Fatalf("many pre-existing quotes blocked an unrelated edit: %v", err)
	}
	const novel = "This newly introduced private reviewer sentence must stay out of changed source."
	result.Refutations[0].Text += "\n" + novel
	result.Changes[0].Data = append(result.Changes[0].Data, novel...)
	if err := rejectPublishedRefutations(job, result, projected); err == nil {
		t.Fatal("new private quote after many pre-existing quotes was accepted")
	}
}

func TestPublishedRefutationsFixRejectsUnprovedCopyAtSourceScanBudget(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	var quotes []string
	for i := range 1040 {
		quotes = append(quotes, fmt.Sprintf("Private quote %04d was already in source.", i))
	}
	quoted := strings.Join(quotes, "\n")
	const novel = "This newly introduced private review sentence must not enter a Fix."
	job.Source.Files[0].Data = []byte(strings.Repeat("f", 1<<20) + "\n" + quoted)
	result := privateReviewResult(job)
	result.Refutations[0].Text = quoted + "\n" + novel
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte(strings.Join(quotes, "\n--;\n") + "\n--;\n" + novel)}}
	if err := result.Validate(job); err != nil {
		t.Fatalf("bounded fixture cannot reach publication: %v", err)
	}
	projected, err := projectPublication(job, result)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectPublishedRefutations(job, result, projected); err == nil {
		t.Fatal("unproved private sentence passed after the source scan budget")
	}
}

func TestPublishedRefutationsCopySearchBudgetFailsClosed(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	result := privateReviewResult(job)
	var sentences []string
	for i := range 1050 {
		sentences = append(sentences, fmt.Sprintf("Private quote %04d was already in source.", i))
	}
	result.Refutations[0].Text = strings.Join(sentences, "\n")
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte(strings.Repeat("z", workspace.MaxChangeBytes))}}
	if err := result.Validate(job); err != nil {
		t.Fatalf("bounded fixture cannot reach publication: %v", err)
	}
	projected, err := projectPublication(job, result)
	if err != nil || rejectPublishedRefutations(job, result, projected) == nil {
		t.Fatalf("unproved sentence search accepted after its work budget: %v", err)
	}
}

func TestPublishedRefutationsAllowCommonShortWords(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	result := privateReviewResult(job)
	for _, test := range []struct{ private, answer string }{
		{"a", "A public reply about the task."},
		{"Affected path: src/app; helper: safe_identifier", "Public reply about src/app and safe_identifier."},
	} {
		result.Refutations[0].Text, result.Answer = test.private, test.answer
		projected, err := projectPublication(job, result)
		if err != nil {
			t.Fatal(err)
		}
		if err := rejectPublishedRefutations(job, result, projected); err != nil {
			t.Fatalf("unrelated public reply blocked by %q: %v", test.private, err)
		}
	}
}

func TestPublishedRefutationsAllowLargeUnrelatedArtifact(t *testing.T) {
	job := withClaudeCritic(testJob(command.SecurityAudit))
	result := privateReviewResult(job)
	var private strings.Builder
	for n := 100000; n < 100205; n++ {
		fmt.Fprintf(&private, "%d ", n)
	}
	result.Refutations[0].Text = private.String()
	result.Artifacts = []workspace.File{{Path: "architecture.md", Mode: workspace.Regular, Data: []byte(strings.Repeat("ordinary public report text ", (5<<20)/len("ordinary public report text ")))}}
	projected := publication{answer: "Public summary", artifacts: result.Artifacts}
	if err := rejectPublishedRefutations(job, result, projected); err != nil {
		t.Fatalf("large unrelated artifact blocked: %v", err)
	}
}
