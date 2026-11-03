package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/linkdata/minion/internal/workspace"
)

// ReadSaved reads one committed state file without touching concurrently mutable Data.
func (s *Store) ReadSaved() (Data, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return Data{}, err
	}
	defer f.Close()
	var data Data
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return Data{}, fmt.Errorf("decode saved state: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return Data{}, errors.New("decode saved state: trailing data")
	}
	if err := validateData(data); err != nil {
		return Data{}, fmt.Errorf("validate saved state: %w", err)
	}
	return data, nil
}

func validateData(data Data) error {
	if data.Cursor.IsZero() {
		return errors.New("state cursor is missing")
	}
	if data.PublicationScanCursor != "" && !workspace.ValidIdentitySegment(data.PublicationScanCursor) {
		return errors.New("invalid publication scan cursor")
	}
	if data.Accepted != nil {
		if err := data.Accepted.Validate(); err != nil {
			return fmt.Errorf("invalid accepted action: %w", err)
		}
	}
	for jobID, failure := range data.JobFailures {
		if err := failure.Validate(); err != nil {
			return fmt.Errorf("invalid job failure %q: %w", jobID, err)
		}
	}
	for jobID, decision := range data.TerminalDecisions {
		if !validTerminalDecision(jobID, decision) {
			return fmt.Errorf("invalid terminal decision %q", jobID)
		}
	}
	for jobID, write := range data.PublicationWrites {
		if !validPublicationWrite(jobID, write) {
			return fmt.Errorf("invalid publication write %q", jobID)
		}
	}
	return nil
}
