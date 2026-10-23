package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

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

// Refutations are private even when an executor supplies a matching reviewed-content digest.
func rejectPublishedRefutations(job Job, result Result, projected publication) error {
	if len(result.Refutations) == 0 {
		return nil
	}
	matcher := publicationSegments{prefixes: make(map[string][]string), budget: 1 << 30}
	for _, refutation := range result.Refutations {
		text := modelLineBreaks.Replace(refutation.Text)
		matcher.add(foldPublicationSpace(text))
		for _, sentence := range strings.FieldsFunc(text, func(r rune) bool {
			return r == '\n' || r == '.' || r == ';' || r == ':' || r == '!' || r == '?'
		}) {
			if segment := foldPublicationSpace(sentence); len(segment) >= 24 {
				matcher.add(segment)
			}
		}
		// shortcut: Short plain words stay unindexed to avoid unrelated-reply rejects; use structured privacy markers if they must be protected.
		for _, token := range strings.FieldsFunc(text, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
		}) {
			if len(token) >= 6 && strings.IndexFunc(token, unicode.IsDigit) >= 0 {
				matcher.add(foldPublicationSpace(token))
				if trimmed := strings.Trim(token, "_-"); trimmed != token && len(trimmed) >= 6 {
					matcher.add(foldPublicationSpace(trimmed))
				}
			}
		}
	}
	public := publicationTextVariants(projected.answer)
	if projected.answer != "" {
		public = append(public, publicationTextVariants(inertModelOutput(projected.answer))...)
	}
	for _, file := range projected.artifacts {
		public = append(public, publicationTextVariants(string(file.Data))...)
	}
	if job.Action == command.Fix {
		for _, change := range result.Changes {
			public = append(public, change.Path)
			public = append(public, publicationTextVariants(string(change.Data))...)
		}
	}
	for _, text := range public {
		if matcher.contains(foldPublicationSpace(text)) {
			return matcher.rejection()
		}
		if strings.ContainsRune(text, '\\') && (matcher.containsJSONStrings(text, 0) || matcher.containsUnicodeEscapes(text)) {
			return matcher.rejection()
		}
	}
	return nil
}

type publicationSegments struct {
	all      []string
	prefixes map[string][]string
	budget   int64
	limited  bool
}

func (m *publicationSegments) add(text string) {
	m.all = append(m.all, text)
	if len(text) >= 16 {
		m.prefixes[text[:16]] = append(m.prefixes[text[:16]], text)
	}
}

func (m *publicationSegments) spend(bytes int) bool {
	if int64(bytes) > m.budget {
		m.limited = true
		return false
	}
	m.budget -= int64(bytes)
	return true
}

func (m publicationSegments) rejection() error {
	if m.limited {
		return errors.New("publication privacy scan limit reached")
	}
	return errors.New("publication contains private review evidence")
}

func (m *publicationSegments) contains(text string) bool {
	// An exhausted budget rejects publication, including hostile prefix collisions.
	if !m.spend(len(text)) {
		return true
	}
	if len(m.all) <= 8 {
		for _, segment := range m.all {
			if !m.spend(len(text)) {
				return true
			}
			if strings.Contains(text, segment) {
				return true
			}
		}
		return false
	}
	for _, segment := range m.all {
		if len(segment) < 16 {
			if !m.spend(len(text)) || strings.Contains(text, segment) {
				return true
			}
		}
	}
	for i := 0; i+16 <= len(text); i++ {
		for _, segment := range m.prefixes[text[i:i+16]] {
			if !m.spend(len(segment)) {
				return true
			}
			if strings.HasPrefix(text[i:], segment) {
				return true
			}
		}
	}
	return false
}

func (m *publicationSegments) containsJSONStrings(text string, depth int) bool {
	if !m.spend(len(text)) {
		return true
	}
	for offset := 0; offset < len(text); {
		start := strings.IndexByte(text[offset:], '"')
		if start < 0 {
			return false
		}
		start += offset
		offset = start + 1
		end := offset
		for end < len(text) && text[end] != '"' {
			if !m.spend(1) {
				return true
			}
			if text[end] == '\\' {
				end++
				if !m.spend(1) {
					return true
				}
			}
			end++
		}
		if end >= len(text) || !strings.ContainsRune(text[offset:end], '\\') {
			continue
		}
		if !m.spend(end - start + 1) {
			return true
		}
		var value string
		if json.Unmarshal([]byte(text[start:end+1]), &value) == nil {
			if m.contains(foldPublicationSpace(value)) {
				return true
			}
			if strings.ContainsRune(value, '\\') {
				if depth == 4 {
					m.limited = true
					return true
				}
				if m.containsJSONStrings(value, depth+1) {
					return true
				}
			}
		}
	}
	return false
}

func (m *publicationSegments) containsUnicodeEscapes(text string) bool {
	if !strings.Contains(text, `\u`) {
		return false
	}
	if !m.spend(len(text)) {
		return true
	}
	var decoded strings.Builder
	decoded.Grow(len(text))
	for i := 0; i < len(text); {
		if i+6 <= len(text) && text[i:i+2] == `\u` {
			if !m.spend(64) {
				return true
			}
			end := i + 6
			if end+6 <= len(text) && text[end:end+2] == `\u` {
				end += 6
			}
			var value string
			if json.Unmarshal([]byte(`"`+text[i:end]+`"`), &value) == nil {
				decoded.WriteString(value)
				i = end
				continue
			}
		}
		decoded.WriteByte(text[i])
		i++
	}
	return m.contains(foldPublicationSpace(decoded.String()))
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
	return result.String()
}

func publicationTextVariants(text string) []string {
	text = modelLineBreaks.Replace(text)
	variants := []string{text}
	if !strings.Contains(text, ">") {
		return variants
	}
	lines := strings.Split(text, "\n")
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
