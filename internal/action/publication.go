package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
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
	fmt.Fprintf(&summary, "Executor-reported security audit summary. Record structure and cited source locations were checked; reviewer activity and behavior remain executor claims.\n\nCoverage units: %d covered, %d candidate, %d blocked, %d not applicable.\nFindings: %d confirmed, %d need validation, %d rejected.\n", counts["covered"], counts["candidate"], counts["blocked"], counts["not_applicable"], counts["confirmed"], counts["needs_validation"], counts["rejected"])
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
		if len(finding.Trace) > 1 {
			sink := finding.Trace[len(finding.Trace)-1]
			fmt.Fprintf(&summary, " sink=%q:%.0f", sink.File, sink.Line)
		}
	}
	return summary.String(), nil
}

// This catches exact whole-refutation or complete-sentence copies, not rewrites or hostile disclosures.
// Exact short matches are limited to whole replies and audit titles; fingerprints and Fix files can contain generic short values.
func rejectPublishedRefutations(job Job, result Result, projected publication) error {
	if len(result.Refutations) == 0 {
		return nil
	}
	const minCopyRunes = 24
	var whole []string
	exact := make(map[string]bool)
	sentences := make(map[string]bool)
	for _, refutation := range result.Refutations {
		privateTexts := []string{refutation.Text}
		if withoutNUL := strings.ReplaceAll(refutation.Text, "\x00", ""); withoutNUL != refutation.Text {
			privateTexts = append(privateTexts, withoutNUL)
		}
		for _, privateText := range privateTexts {
			for _, variant := range publicationTextVariants(privateText) {
				if text := foldPublicationSpace(variant); text != "" {
					exact[text] = true
					if utf8.RuneCountInString(text) >= minCopyRunes {
						whole = append(whole, text)
					}
				}
				for _, sentence := range strings.FieldsFunc(variant, publicationSentenceBreak) {
					if text := foldPublicationSentence(sentence); utf8.RuneCountInString(text) >= minCopyRunes {
						sentences[text] = true
					}
				}
			}
		}
	}
	if len(exact) == 0 {
		return nil
	}
	type sentenceSuffix struct {
		first, next uint32
		char        byte
		terminal    bool
	}
	// One node per distinct byte keeps accepted private text from multiplying into per-byte maps.
	suffixes := []sentenceSuffix{{}}
	child := func(parent uint32, char byte) uint32 {
		for i := suffixes[parent].first; i != 0; i = suffixes[i].next {
			if suffixes[i].char == char {
				return i
			}
		}
		return 0
	}
	for text := range sentences {
		var node uint32
		for i := len(text) - 1; i >= 0; i-- {
			next := child(node, text[i])
			if next == 0 {
				next = uint32(len(suffixes))
				suffixes = append(suffixes, sentenceSuffix{char: text[i], next: suffixes[node].first})
				suffixes[node].first = next
			}
			node = next
		}
		suffixes[node].terminal = true
	}
	sentenceCopy := func(text string, reject func(string) bool) bool {
		text = foldPublicationSentence(text)
		var node uint32
		for i := len(text); i > 0; {
			i--
			node = child(node, text[i])
			if node == 0 {
				break
			}
			if !suffixes[node].terminal {
				continue
			}
			if i > 0 {
				before, _ := utf8.DecodeLastRuneInString(text[:i])
				if unicode.IsLetter(before) || unicode.IsNumber(before) || unicode.IsMark(before) || before == '_' {
					continue
				}
			}
			if reject(text[i:]) {
				return true
			}
		}
		return false
	}
	containsCopy := func(text string, reject func(string) bool) bool {
		word := func(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' }
		for _, variant := range publicationTextVariants(text) {
			folded := foldPublicationSpace(variant)
			for _, private := range whole {
				first, _ := utf8.DecodeRuneInString(private)
				last, _ := utf8.DecodeLastRuneInString(private)
				for offset := 0; offset < len(folded); {
					index := strings.Index(folded[offset:], private)
					if index < 0 {
						break
					}
					start, end := offset+index, offset+index+len(private)
					left, right := rune(0), rune(0)
					if start > 0 {
						left, _ = utf8.DecodeLastRuneInString(folded[:start])
					}
					if end < len(folded) {
						right, _ = utf8.DecodeRuneInString(folded[end:])
					}
					if !(word(first) && word(left)) && !(word(last) && word(right)) && reject(private) {
						return true
					}
					offset = start + 1
				}
			}
			for _, sentence := range strings.FieldsFunc(variant, publicationSentenceBreak) {
				if sentenceCopy(sentence, reject) {
					return true
				}
			}
		}
		return false
	}
	reject := func(string) bool { return true }
	wholeReply := func(text string, reject func(string) bool) bool {
		for _, variant := range publicationTextVariants(text) {
			folded := foldPublicationSpace(variant)
			if exact[folded] && reject(folded) {
				return true
			}
		}
		return false
	}
	// A Fix transfers full replacement files; source quotations can also appear in public prose.
	// shortcut: Source searches stop after 1 GiB; use multi-pattern matching if fuller coverage matters at that workload.
	const maxSourceSearchBytes int64 = 1 << 30
	remainingSourceSearchBytes := maxSourceSearchBytes
	inSource := make(map[string]bool)
	scanExhausted := false
	var sourceText string
	cacheChecked, sourceCached := false, false
	// These parent-prepared GitHub fields were shown to the model with the source files.
	threadText := [...]string{job.Title, job.Description, job.Notes}
	rejectNew := func(private string) bool {
		if public, known := inSource[private]; known {
			return !public
		}
		if scanExhausted {
			return false
		}
		cacheable := strings.IndexByte(private, 0) < 0
		if !cacheChecked && cacheable {
			cacheChecked = true
			const maxCacheSourceBytes, maxCacheNormalizedBytes = 4 << 20, 32 << 20
			rawBytes := 0
			for _, text := range threadText {
				rawBytes += len(text)
			}
			for _, files := range [][]workspace.File{job.Source.Files, job.Source.BaseFiles} {
				for _, file := range files {
					rawBytes += len(file.Path) + len(file.Data)
				}
			}
			if rawBytes <= maxCacheSourceBytes {
				var source strings.Builder
				sourceCached = true
			threadCache:
				for _, text := range threadText {
					for _, variant := range publicationTextVariants(text) {
						folded := foldPublicationSpace(variant)
						if len(folded)+1 > maxCacheNormalizedBytes-source.Len() {
							sourceCached = false
							break threadCache
						}
						source.WriteString(folded)
						source.WriteByte(0)
					}
				}
				if sourceCached {
				buildCache:
					for _, files := range [][]workspace.File{job.Source.Files, job.Source.BaseFiles} {
						for _, file := range files {
							for _, text := range []string{file.Path, string(file.Data)} {
								for _, variant := range publicationTextVariants(text) {
									folded := foldPublicationSpace(variant)
									if len(folded)+1 > maxCacheNormalizedBytes-source.Len() {
										sourceCached = false
										break buildCache
									}
									source.WriteString(folded)
									source.WriteByte(0) // Matches cannot span files or variants.
								}
							}
						}
					}
				}
				if sourceCached {
					sourceText = source.String()
				}
			}
		}
		// NUL may occur in private evidence; the cache also uses it between source elements.
		if sourceCached && cacheable {
			if int64(len(sourceText)) > remainingSourceSearchBytes {
				scanExhausted = true
				return false
			}
			remainingSourceSearchBytes -= int64(len(sourceText))
			inSource[private] = strings.Contains(sourceText, private)
			return !inSource[private]
		}
		for _, text := range threadText {
			if int64(len(text)) > remainingSourceSearchBytes {
				scanExhausted = true
				return false
			}
			for _, variant := range publicationTextVariants(text) {
				folded := foldPublicationSpace(variant)
				if int64(len(folded)) > remainingSourceSearchBytes {
					scanExhausted = true
					return false
				}
				remainingSourceSearchBytes -= int64(len(folded))
				if strings.Contains(folded, private) {
					inSource[private] = true
					return false
				}
			}
		}
		for _, files := range [][]workspace.File{job.Source.Files, job.Source.BaseFiles} {
			for _, file := range files {
				for _, text := range []string{file.Path, string(file.Data)} {
					if int64(len(text)) > remainingSourceSearchBytes {
						scanExhausted = true
						return false
					}
					for _, variant := range publicationTextVariants(text) {
						folded := foldPublicationSpace(variant)
						if int64(len(folded)) > remainingSourceSearchBytes {
							scanExhausted = true
							return false
						}
						remainingSourceSearchBytes -= int64(len(folded))
						if strings.Contains(folded, private) {
							inSource[private] = true
							return false
						}
					}
				}
			}
		}
		inSource[private] = false
		return true
	}
	rejectUnproved := func(private string) bool {
		return rejectNew(private) || !inSource[private]
	}
	// Check executor text, not the host's cap notice; audit fields are checked below.
	if projected.answer != "" && job.Action != command.SecurityAudit &&
		(wholeReply(result.Answer, rejectUnproved) || containsCopy(result.Answer, rejectNew)) {
		return errors.New("publication contains private review evidence")
	}
	if job.Action == command.SecurityAudit {
		for _, artifact := range result.Artifacts {
			if artifact.Path != "findings.json" {
				continue
			}
			var findings []struct{ Verdict, Fingerprint, Title string }
			if err := json.Unmarshal(artifact.Data, &findings); err != nil {
				return fmt.Errorf("decode audit findings before publication: %w", err)
			}
			for _, finding := range findings {
				if finding.Verdict != "rejected" && (containsCopy(finding.Fingerprint, reject) || wholeReply(finding.Title, rejectUnproved) || containsCopy(finding.Title, rejectNew)) {
					return errors.New("publication contains private review evidence")
				}
			}
			break
		}
	}
	if job.Action == command.Fix {
		for _, change := range result.Changes {
			if containsCopy(change.Path, rejectNew) || containsCopy(string(change.Data), rejectNew) {
				return errors.New("publication contains private review evidence")
			}
		}
	}
	return nil
}

func publicationSentenceBreak(r rune) bool {
	return r == '.' || r == ';' || r == ':' || r == '!' || r == '?' || r == '"' || r == '。'
}

func foldPublicationSentence(text string) string {
	return foldPublicationSpace(strings.Trim(strings.TrimSpace(text), "*_`"))
}

func foldPublicationSpace(text string) string {
	var result strings.Builder
	result.Grow(len(text))
	space := false
	for _, r := range text {
		if r == 0 {
			result.WriteByte(0) // Source quotes cannot cross a binary boundary.
			space = false
			continue
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && result.Len() != 0 {
			result.WriteByte(' ')
		}
		result.WriteRune(r)
		space = false
	}
	return norm.NFC.String(cases.Fold().String(result.String()))
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
