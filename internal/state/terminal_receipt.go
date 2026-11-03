package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

// ReadSavedDurable confirms the visible state entry before a late receipt ACK.
func (s *Store) ReadSavedDurable() (Data, error) {
	return s.readSavedDurable(workspace.SyncDirectories)
}

func (s *Store) readSavedDurable(syncDirectories func(...string) error) (Data, error) {
	data, err := s.ReadSaved()
	if err != nil {
		return Data{}, err
	}
	// A rename can be visible before Save's first directory sync completes.
	if err := syncDirectories(filepath.Dir(s.path)); err != nil {
		return Data{}, fmt.Errorf("sync saved state directory: %w", err)
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
