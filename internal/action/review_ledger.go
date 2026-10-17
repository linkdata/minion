package action

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ReviewEntry preserves private parsed model evidence in call order. Main notes
// are assertions. Critic approval applies to that phase and candidate, not every
// earlier main refutation; fresh full reviews receive no prior review history.
type ReviewEntry struct {
	Role        string `json:"role"`
	Round       int    `json:"round"`
	ReviewPhase string `json:"review_phase,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Approved    bool   `json:"approved,omitempty"`
	HasBlockers bool   `json:"has_blockers,omitempty"`
	Feedback    string `json:"feedback,omitempty"`
}

// validateReviewLedger requires a validated job and call receipts. An incomplete
// result may end before a failed or unparsed call, but cannot skip a call.
func validateReviewLedger(job Job, calls []CallUsage, ledger []ReviewEntry, complete bool) error {
	if job.Critic == nil {
		if len(ledger) != 0 {
			return errors.New("unexpected review ledger without a critic")
		}
		return nil
	}
	if len(ledger) > 3*job.rounds()-1 || len(ledger) > len(calls) || (complete && len(ledger) != len(calls)) {
		return errors.New("review ledger does not match model call count")
	}
	for i, entry := range ledger {
		call := calls[i]
		if entry.Role != call.Role || entry.Round != call.Round || entry.ReviewPhase != call.ReviewPhase {
			return errors.New("review ledger differs from model call order")
		}
		// These private strings are JSON-escaped evidence, not identity or path fields.
		if len(entry.Notes) > maxOutputBytes || !utf8.ValidString(entry.Notes) || len(entry.Feedback) > maxCriticOutputBytes || !utf8.ValidString(entry.Feedback) {
			return errors.New("review ledger text is invalid or exceeds limit")
		}
		switch entry.Role {
		case roleMain:
			if entry.Approved || entry.HasBlockers || entry.Feedback != "" {
				return errors.New("main review ledger entry contains a critic verdict")
			}
		case roleCritic:
			if entry.Notes != "" || entry.Approved != call.Approved || entry.HasBlockers != call.HasBlockers || call.NoBlockers != !entry.HasBlockers {
				return errors.New("review ledger differs from parsed critic verdict")
			}
			if entry.Approved != (entry.Feedback == "") || (entry.Approved && entry.HasBlockers) || (!entry.Approved && strings.TrimSpace(entry.Feedback) == "") {
				return errors.New("invalid critic verdict in review ledger")
			}
		default:
			return errors.New("invalid review ledger role")
		}
	}
	return nil
}
