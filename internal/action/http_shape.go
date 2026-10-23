package action

import (
	"errors"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

var errHTTPShape = errors.New("HTTP execution facts exceed field bounds")

// Wire bounds preserve raw failed receipts; policy and complete-result validation stay with the parent.
func httpBindingShape(binding httpBinding) error {
	for _, field := range []struct {
		value string
		limit int
	}{
		{binding.Identity.JobID, 100}, {binding.Identity.AttemptID, 100}, {binding.Identity.ExecutorID, 100},
		{binding.Identity.Repository, 201}, {binding.Identity.Requester, 105}, {binding.Identity.SubjectType, 11},
		{binding.SourceRef, 40},
	} {
		if !boundedText(field.value, field.limit) {
			return errHTTPShape
		}
	}
	return nil
}

func httpCallShape(call CallUsage) error {
	for _, value := range []string{call.Provider, call.Role, call.ReviewPhase, call.Model, call.Effort} {
		if !boundedText(value, config.MaxModelSettingBytes) {
			return errHTTPShape
		}
	}
	return nil
}

func httpReviewShape(review ReviewCheckpoint) error {
	if err := httpBindingShape(httpBinding{review.Version, review.Identity, review.SourceRef}); err != nil {
		return err
	}
	if len(review.Calls) > 3*config.MaxCriticMaxRounds-1 || len(review.Refutations) > config.MaxCriticMaxRounds-1 || !boundedText(review.CriticStopReason, 32) {
		return errHTTPShape
	}
	for _, call := range review.Calls {
		if err := httpCallShape(call); err != nil {
			return err
		}
	}
	for _, refutation := range review.Refutations {
		if len(refutation.Text) > maxOutputBytes || !utf8.ValidString(refutation.Text) {
			return errHTTPShape
		}
	}
	return nil
}

func httpResultShape(result Result) error {
	if err := httpReviewShape(ReviewCheckpoint{Version: result.Version, Identity: result.Identity, SourceRef: result.SourceRef, Calls: result.Calls, Refutations: result.Refutations, CriticStopReason: result.CriticStopReason}); err != nil {
		return err
	}
	if !boundedText(result.Answer, maxOutputBytes) || len(result.Changes) > workspace.MaxChanges || len(result.Artifacts) > len(auditArtifacts) || len(result.Diagnostics) > maxDiagnostics {
		return errHTTPShape
	}
	var changes int64
	for _, change := range result.Changes {
		if !boundedText(change.Path, workspace.MaxPathBytes) || len(change.Data) > workspace.MaxChangeBytes {
			return errHTTPShape
		}
		changes += int64(len(change.Data))
	}
	if changes > workspace.MaxChangeBytes {
		return errHTTPShape
	}
	for _, artifact := range result.Artifacts {
		if !boundedText(artifact.Path, workspace.MaxPathBytes) || len(artifact.Data) > maxAuditArtifactBytes {
			return errHTTPShape
		}
	}
	for _, diagnostic := range result.Diagnostics {
		if !boundedText(diagnostic.Code, 100) || !boundedText(diagnostic.Message, maxDiagnosticMessageBytes) {
			return errHTTPShape
		}
	}
	return nil
}
