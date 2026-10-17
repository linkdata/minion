package action

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// Refutation preserves private main-model counterevidence, not acceptance of changed code.
// Revisions and scoped verification receive it; fresh full reviews remain independent.
type Refutation struct {
	Round int    `json:"round"`
	Text  string `json:"text"`
}

// validateRefutations requires a validated job and ordered call receipts.
func validateRefutations(job Job, calls []CallUsage, refutations []Refutation) error {
	if job.Critic == nil && len(refutations) != 0 {
		return errors.New("unexpected refutations without a critic")
	}
	if len(refutations) > job.rounds() {
		return errors.New("refutations exceed main round count")
	}
	previous := 0
	for _, entry := range refutations {
		if entry.Round <= previous || !slices.ContainsFunc(calls, func(call CallUsage) bool { return call.Role == roleMain && call.Round == entry.Round }) {
			return errors.New("refutation does not match an ordered main call")
		}
		if strings.TrimSpace(entry.Text) == "" || len(entry.Text) > maxOutputBytes || !utf8.ValidString(entry.Text) {
			return errors.New("refutation text is blank, invalid or exceeds limit")
		}
		previous = entry.Round
	}
	return nil
}
