package action

import (
	"errors"
	"fmt"
	"html"
	"slices"
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
	projected := publication{answer: result.Answer, artifacts: result.Artifacts, cap: cappedReview(job, result)}
	notice, err := criticCapNotice(projected.cap)
	if err != nil {
		return publication{}, err
	}
	if job.Action == command.Fix && len(result.Changes) != 0 {
		return publication{cap: projected.cap}, nil
	}
	if notice == "" {
		return projected, nil
	}
	projected.answer = notice + "\n\n" + result.Answer
	if !publishableAnswer(projected.answer) {
		return publication{}, errors.New("capped publication answer is invalid or exceeds limit")
	}
	if job.Action == command.SecurityAudit {
		projected.artifacts = slices.Clone(result.Artifacts)
		for i := range projected.artifacts {
			if projected.artifacts[i].Path == "REPORT.md" {
				projected.artifacts[i].Data = []byte(projected.answer)
			}
		}
	}
	return projected, nil
}

// This catches accidental copies of substantial reported refutation text, not leaks from a hostile executor.
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
	public := []string{projected.answer}
	for _, file := range projected.artifacts {
		public = append(public, string(file.Data))
	}
	if job.Action == command.Fix {
		for _, change := range result.Changes {
			public = append(public, change.Path, string(change.Data))
		}
	}
	for _, text := range public {
		for _, variant := range publicationTextVariants(text) {
			folded := foldPublicationSpace(variant)
			for _, private := range whole {
				if strings.Contains(folded, private) {
					return errors.New("publication contains private review evidence")
				}
			}
			for _, sentence := range strings.FieldsFunc(variant, publicationSentenceBreak) {
				if sentences[foldPublicationSpace(sentence)] {
					return errors.New("publication contains private review evidence")
				}
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
