package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

type publication struct {
	answer    string
	artifacts []workspace.File
	cap       *state.CriticCap
}

// projectPublication requires a raw Result that already passed validation.
func projectPublication(job Job, result Result) (publication, error) {
	answer := result.Answer
	if job.Action == command.SecurityAudit {
		var err error
		answer, err = auditPublicSummary(result.Artifacts)
		if err != nil {
			return publication{}, err
		}
	}
	projected := publication{answer: answer, artifacts: result.Artifacts, cap: cappedReview(job, result)}
	notice, err := criticCapNotice(projected.cap)
	if err != nil {
		return publication{}, err
	}
	if job.Action == command.Fix && len(result.Changes) != 0 {
		return publication{cap: projected.cap}, nil
	}
	if notice != "" {
		projected.answer = notice + "\n\n" + answer
	}
	if !publishableAnswer(projected.answer) {
		return publication{}, errors.New("publication answer is invalid or exceeds limit")
	}
	return projected, nil
}

// auditPublicSummary publishes only fields from the validated structured record.
// The executor's Markdown report remains private narrative and may contradict it.
func auditPublicSummary(artifacts []workspace.File) (string, error) {
	var findingsBytes, ledgerBytes []byte
	for _, file := range artifacts {
		switch file.Path {
		case "findings.json":
			findingsBytes = file.Data
		case "coverage-ledger.json":
			ledgerBytes = file.Data
		}
	}
	var findings []struct {
		Verdict, Fingerprint, Title string
		Severity                    struct {
			Overall string `json:"overall_severity"`
		}
		Trace []struct {
			File string
			Line float64
		}
	}
	var units []struct{ Status string }
	if err := json.Unmarshal(findingsBytes, &findings); err != nil {
		return "", fmt.Errorf("decode audit findings for publication: %w", err)
	}
	if err := json.Unmarshal(ledgerBytes, &units); err != nil {
		return "", fmt.Errorf("decode audit coverage for publication: %w", err)
	}
	counts := make(map[string]int)
	for _, unit := range units {
		counts[unit.Status]++
	}
	for _, finding := range findings {
		counts[finding.Verdict]++
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "Executor-reported security audit summary. Record structure and cited source locations were checked; reviewer activity and behavior remain executor claims.\n\nCoverage units: %d covered, %d candidate, %d blocked, %d out of scope, %d not applicable.\nFindings: %d confirmed, %d need validation, %d rejected.\n", counts["covered"], counts["candidate"], counts["blocked"], counts["out_of_scope"], counts["not_applicable"], counts["confirmed"], counts["needs_validation"], counts["rejected"])
	for _, finding := range findings {
		if finding.Verdict == "rejected" {
			continue
		}
		if len(finding.Trace) == 0 {
			return "", errors.New("audit finding has no source trace for publication")
		}
		if finding.Verdict == "confirmed" {
			fmt.Fprintf(&summary, "\nConfirmed (%s) fingerprint=%q title=%q source=%q:%.0f", finding.Severity.Overall, finding.Fingerprint, finding.Title, finding.Trace[0].File, finding.Trace[0].Line)
		} else {
			fmt.Fprintf(&summary, "\nNeeds validation fingerprint=%q title=%q source=%q:%.0f", finding.Fingerprint, finding.Title, finding.Trace[0].File, finding.Trace[0].Line)
		}
	}
	return summary.String(), nil
}

// This catches exact whole-refutation or complete-sentence copies, not rewrites or hostile disclosures.
func rejectPublishedRefutations(job Job, result Result, projected publication) error {
	if len(result.Refutations) == 0 {
		return nil
	}
	const minCopyRunes = 24
	var whole []string
	sentences := make(map[string]bool)
	for _, refutation := range result.Refutations {
		for _, variant := range publicationTextVariants(refutation.Text) {
			if text := foldPublicationSpace(variant); utf8.RuneCountInString(text) >= minCopyRunes {
				whole = append(whole, text)
			}
			for _, sentence := range strings.FieldsFunc(variant, publicationSentenceBreak) {
				if text := foldPublicationSpace(sentence); utf8.RuneCountInString(text) >= minCopyRunes {
					sentences[text] = true
				}
			}
		}
	}
	if len(whole) == 0 && len(sentences) == 0 {
		return nil
	}
	containsCopy := func(text string, reject func(string) bool) bool {
		for _, variant := range publicationTextVariants(text) {
			folded := foldPublicationSpace(variant)
			for _, private := range whole {
				if strings.Contains(folded, private) && reject(private) {
					return true
				}
			}
			for _, sentence := range strings.FieldsFunc(variant, publicationSentenceBreak) {
				if text := foldPublicationSpace(sentence); sentences[text] && reject(text) {
					return true
				}
			}
			for line := range strings.SplitSeq(variant, "\n") {
				line = strings.TrimLeft(line, " \t")
				if len(line) < minCopyRunes+2 || !strings.ContainsAny(line[:1], "-*+") || line[1] != ' ' && line[1] != '\t' {
					continue
				}
				for _, sentence := range strings.FieldsFunc(line[2:], publicationSentenceBreak) {
					if text := foldPublicationSpace(sentence); sentences[text] && reject(text) {
						return true
					}
				}
			}
		}
		return false
	}
	reject := func(string) bool { return true }
	if containsCopy(projected.answer, reject) {
		return errors.New("publication contains private review evidence")
	}
	if job.Action == command.Fix {
		// A Fix transfers full replacement files. Source text quoted in a refutation was already public.
		inSource := make(map[string]bool)
		rejectNew := func(private string) bool {
			if public, known := inSource[private]; known {
				return !public
			}
			for _, file := range job.Source.Files {
				for _, text := range []string{file.Path, string(file.Data)} {
					for _, variant := range publicationTextVariants(text) {
						if strings.Contains(foldPublicationSpace(variant), private) {
							inSource[private] = true
							return false
						}
					}
				}
			}
			inSource[private] = false
			return true
		}
		for _, change := range result.Changes {
			if containsCopy(change.Path, rejectNew) || containsCopy(string(change.Data), rejectNew) {
				return errors.New("publication contains private review evidence")
			}
		}
	}
	return nil
}

func publicationSentenceBreak(r rune) bool {
	return r == '.' || r == ';' || r == ':' || r == '!' || r == '?'
}

func foldPublicationSpace(text string) string {
	var result strings.Builder
	result.Grow(len(text))
	space := false
	for _, r := range text {
		if r == 0 {
			continue
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && result.Len() != 0 {
			result.WriteByte(' ')
		}
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		result.WriteRune(folded)
		space = false
	}
	return norm.NFC.String(result.String())
}

func publicationTextVariants(text string) []string {
	text = modelLineBreaks.Replace(text)
	variants := []string{text}
	if decoded := html.UnescapeString(text); decoded != text {
		variants = append(variants, decoded)
	}
	for _, candidate := range variants {
		if !strings.Contains(candidate, ">") {
			continue
		}
		lines := strings.Split(candidate, "\n")
		changed := false
		for i, line := range lines {
			for {
				trimmed := strings.TrimLeft(line, " \t")
				if !strings.HasPrefix(trimmed, ">") {
					break
				}
				line, changed = strings.TrimLeft(trimmed[1:], " \t"), true
			}
			lines[i] = line
		}
		if changed {
			variants = append(variants, strings.Join(lines, "\n"))
		}
	}
	return variants
}

// cappedReview derives public facts only from an actual capped decision.
func cappedReview(job Job, result Result) *state.CriticCap {
	if result.CriticStopReason != "round_cap" {
		return nil
	}
	cap := &state.CriticCap{MaxRounds: job.rounds()}
	for _, call := range result.Calls {
		if call.Role == roleCritic && call.Round == cap.MaxRounds {
			cap.HasBlockers = cap.HasBlockers || call.HasBlockers
			if call.ReviewPhase == reviewFull {
				cap.FullApproved = call.Approved
			}
		}
	}
	return cap
}

func criticCapNotice(cap *state.CriticCap) (string, error) {
	if cap == nil {
		return "", nil
	}
	if cap.MaxRounds < 1 || cap.MaxRounds > config.MaxCriticMaxRounds {
		return "", errors.New("invalid capped critic round maximum")
	}
	notice := fmt.Sprintf("Critic round limit reached (%d). Normal critic acceptance was not reached; unresolved findings may remain.", cap.MaxRounds)
	if cap.FullApproved {
		notice += " The fresh full critic reported approval, but required verification findings were not cleared."
	} else {
		notice += " The fresh full critic did not approve."
	}
	if cap.HasBlockers {
		notice += " A critic review phase of the final candidate reported blockers."
	}
	return notice, nil
}
