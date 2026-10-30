package action

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/workspace"
)

const (
	maxAttemptRecordBytes     int64 = 64 << 20
	maxAttemptRecordTextBytes       = 16 << 10
	maxAttemptMarkerTextBytes       = 256
	maxAttemptRecordLineBytes       = maxAttemptRecordTextBytes*6 + 2048
)

const (
	attemptRecordInput           = "input"
	attemptRecordDiscussion      = "discussion"
	attemptRecordDiagnostic      = "diagnostic"
	attemptRecordReport          = "report"
	attemptRecordReviewReference = "review_reference"
	attemptRecordWorkerReference = "worker_reference"
	attemptRecordStop            = "stop"
	attemptRecordCoverage        = "coverage"
)

var errAttemptRecordsFull = errors.New("attempt record store full")
var errAttemptRecordUncertain = errors.New("attempt record write uncertain; reopen to reconcile")
var errAttemptRecordConflict = errors.New("attempt record conflicts with saved attempt")

// Reference kinds point to the existing per-attempt review and worker files.
type attemptRecord struct {
	Identity  Identity `json:"identity"`
	SourceRef string   `json:"source_ref"`
	Sequence  uint64   `json:"sequence"`
	Kind      string   `json:"kind"`
	Text      string   `json:"text,omitempty"`
}

type attemptRecordLine struct {
	offset int64
	length int
}

type attemptRecordStore struct {
	file       *os.File
	identity   Identity
	sourceRef  string
	limit      int64
	size       int64
	lines      []attemptRecordLine
	stop       bool
	coverage   bool
	stopSpace  int64
	coverSpace int64
	uncertain  bool
	writeAt    func([]byte, int64) (int, error)
	syncFile   func() error
}

func openAttemptRecordStore(workRoot string, identity Identity, sourceRef string) (*attemptRecordStore, error) {
	return openAttemptRecordStoreWithLimit(workRoot, identity, sourceRef, maxAttemptRecordBytes)
}

// The limit argument only reduces the fixed cap for focused tests.
func openAttemptRecordStoreWithLimit(workRoot string, identity Identity, sourceRef string, limit int64) (_ *attemptRecordStore, resultErr error) {
	if err := identity.validate(); err != nil {
		return nil, err
	}
	if !workspace.ValidSHA(sourceRef) || limit <= 0 || limit > maxAttemptRecordBytes {
		return nil, errors.New("invalid attempt record source or limit")
	}
	dir, err := privateRecordsDir(workRoot, "attempt-records")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, identity.AttemptID+".jsonl")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		if _, readErr := workspace.ReadRegular(path, limit); readErr != nil {
			if retryableHostIO(readErr) || errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
			return nil, fmt.Errorf("%w: %w", errAttemptRecordConflict, readErr)
		}
		file, err = os.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%w: attempt record file is not regular or exceeds cap", errAttemptRecordConflict)
	}
	store := &attemptRecordStore{file: file, identity: identity, sourceRef: sourceRef, limit: limit}
	store.writeAt, store.syncFile = file.WriteAt, file.Sync
	store.stopSpace = store.markerSpace(attemptRecordStop)
	store.coverSpace = store.markerSpace(attemptRecordCoverage)
	if store.stopSpace+store.coverSpace > limit {
		return nil, errors.New("attempt record limit cannot hold both markers")
	}
	if err := store.scan(); err != nil {
		return nil, err
	}
	reserve := int64(0)
	if !store.stop {
		reserve += store.stopSpace
	}
	if !store.coverage {
		reserve += store.coverSpace
	}
	if store.size+reserve > limit {
		return nil, fmt.Errorf("%w: marker reserve is missing", errAttemptRecordConflict)
	}
	// This also reconciles a previous uncertain Sync or directory creation.
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := syncPrivateRecordDirectories(dir, filepath.Dir(dir)); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *attemptRecordStore) markerSpace(kind string) int64 {
	marker := attemptRecord{Identity: s.identity, SourceRef: s.sourceRef, Sequence: math.MaxUint64, Kind: kind, Text: strings.Repeat("\x01", maxAttemptMarkerTextBytes)}
	data, _ := json.Marshal(marker)
	return int64(len(data) + 1)
}

func (r attemptRecord) validateText() error {
	limit := maxAttemptRecordTextBytes
	switch r.Kind {
	case attemptRecordInput, attemptRecordDiscussion, attemptRecordDiagnostic, attemptRecordReport:
	case attemptRecordStop, attemptRecordCoverage:
		limit = maxAttemptMarkerTextBytes
	case attemptRecordReviewReference, attemptRecordWorkerReference:
		limit = 0
	default:
		return errors.New("unknown attempt record kind")
	}
	if len(r.Text) > limit || !utf8.ValidString(r.Text) || strings.ContainsRune(r.Text, 0) {
		return errors.New("attempt record text is invalid or exceeds limit")
	}
	return nil
}

func (s *attemptRecordStore) scan() error {
	reader := bufio.NewReaderSize(s.file, maxAttemptRecordLineBytes)
	var offset int64
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
			if err == nil {
				return fmt.Errorf("%w: oversized complete line", errAttemptRecordConflict)
			}
			if !errors.Is(err, io.EOF) {
				return err
			}
			break // An oversized incomplete tail is discarded below.
		}
		if errors.Is(err, io.EOF) {
			break // A final line without a newline was not acknowledged.
		}
		if err != nil {
			return err
		}
		var record attemptRecord
		if err := json.Unmarshal(line[:len(line)-1], &record); err != nil {
			return fmt.Errorf("%w: invalid complete line: %w", errAttemptRecordConflict, err)
		}
		canonical, err := json.Marshal(record)
		if err != nil || !bytes.Equal(canonical, line[:len(line)-1]) ||
			record.Identity != s.identity || record.SourceRef != s.sourceRef || record.Sequence != uint64(len(s.lines)+1) ||
			record.validateText() != nil || record.Kind == attemptRecordStop && s.stop || record.Kind == attemptRecordCoverage && s.coverage {
			return fmt.Errorf("%w: invalid complete line at sequence %d", errAttemptRecordConflict, len(s.lines)+1)
		}
		if record.Kind == attemptRecordStop {
			s.stop = true
		}
		if record.Kind == attemptRecordCoverage {
			s.coverage = true
		}
		s.lines = append(s.lines, attemptRecordLine{offset: offset, length: len(line)})
		offset += int64(len(line))
	}
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != offset {
		if err := s.file.Truncate(offset); err != nil {
			return err
		}
	}
	s.size = offset
	return nil
}

// Append syncs new entries before returning the highest contiguous sequence.
// Replay compares the persisted redacted record under the same redaction
// inputs; raw text that sanitizes to the same stored text is equivalent.
// After an uncertain write or Sync, callers must close and reopen this store.
// The caller owns one live store per attempt and serializes Append calls.
func (s *attemptRecordStore) Append(record attemptRecord, secrets ...string) (uint64, error) {
	if s.file == nil {
		return 0, os.ErrClosed
	}
	if s.uncertain {
		return 0, errAttemptRecordUncertain
	}
	if record.Identity != s.identity || record.SourceRef != s.sourceRef || record.Sequence == 0 {
		return 0, errAttemptRecordConflict
	}
	if err := record.validateText(); err != nil {
		return 0, err
	}
	rawText := record.Text
	record.Text = redactActionText(rawText, secrets...)
	unsafeText := rawText != "" && record.Text == ""
	if record.Text != "" {
		encodedText, err := json.Marshal(record.Text)
		if err != nil {
			return 0, err
		}
		for _, secret := range secrets {
			if secret != "" && containsConfiguredKey(encodedText, secret) {
				unsafeText = true
				break
			}
		}
	}
	marker := record.Kind == attemptRecordStop || record.Kind == attemptRecordCoverage
	if marker && (unsafeText || len(record.Text) > maxAttemptMarkerTextBytes) {
		// The marker remains durable even when its details cannot be retained safely.
		record.Text = ""
	} else if unsafeText {
		return 0, errors.New("attempt record text retains a configured credential")
	}
	if err := record.validateText(); err != nil {
		return 0, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	data = append(data, '\n')
	if len(data) > maxAttemptRecordLineBytes {
		return 0, errors.New("attempt record line exceeds limit")
	}
	highest := uint64(len(s.lines))
	if record.Sequence <= highest {
		previous := s.lines[record.Sequence-1]
		saved := make([]byte, previous.length)
		if _, err := s.file.ReadAt(saved, previous.offset); err != nil {
			return 0, err
		}
		if !bytes.Equal(saved, data) {
			return 0, errAttemptRecordConflict
		}
		return highest, nil
	}
	if record.Sequence != highest+1 || record.Kind == attemptRecordStop && s.stop || record.Kind == attemptRecordCoverage && s.coverage {
		return 0, errAttemptRecordConflict
	}
	reserve := int64(0)
	if !s.stop && record.Kind != attemptRecordStop {
		reserve += s.stopSpace
	}
	if !s.coverage && record.Kind != attemptRecordCoverage {
		reserve += s.coverSpace
	}
	if s.size+int64(len(data))+reserve > s.limit {
		return 0, errAttemptRecordsFull
	}
	n, err := s.writeAt(data, s.size)
	if err != nil {
		s.uncertain = true
		return 0, err
	}
	if n != len(data) {
		s.uncertain = true
		return 0, io.ErrShortWrite
	}
	if err := s.syncFile(); err != nil {
		s.uncertain = true
		return 0, err
	}
	s.lines = append(s.lines, attemptRecordLine{offset: s.size, length: len(data)})
	s.size += int64(len(data))
	if record.Kind == attemptRecordStop {
		s.stop = true
	}
	if record.Kind == attemptRecordCoverage {
		s.coverage = true
	}
	return uint64(len(s.lines)), nil
}

func (s *attemptRecordStore) Close() error {
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	return file.Close()
}
