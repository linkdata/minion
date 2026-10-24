package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	httpJobBytes     = 256 << 20
	httpOutcomeBytes = 64 << 20
	httpReviewBytes  = 8 << 20
	httpControlBytes = 128 << 10
)

var errHTTPBodyLimit = errors.New("HTTP body exceeds encoded limit")
var errHTTPComplexity = errors.New("HTTP body exceeds JSON complexity limit")

// httpEncode bounds emitted bytes; callers must bound the value before the encoder allocates it.
func httpEncode(value any, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, errHTTPBodyLimit
	}
	buffer := bounded.Buffer{Limit: limit}
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		return nil, err
	}
	if buffer.Exceeded {
		return nil, errHTTPBodyLimit
	}
	return buffer.Bytes(), nil
}

func httpDecode(reader io.Reader, limit int, value any) error {
	if limit < 0 || int64(limit) == math.MaxInt64 {
		return errHTTPBodyLimit
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if int64(len(data)) > int64(limit) {
		return errHTTPBodyLimit
	}
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return errors.New("HTTP body is not valid UTF-8")
	}
	// Bound container expansion before allocating typed slices from untrusted JSON.
	// Canonical maximum Job arrays require fewer than 181,000 tokens.
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	depth := 0
	for count := 0; ; count++ {
		token, err := scan.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if count >= 200_000 {
			return errHTTPComplexity
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			if depth > 64 {
				return errHTTPComplexity
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("HTTP body contains trailing JSON")
	}
	return nil
}

// httpJobBound requires a validated Job. Reject a bound above httpJobBytes before encoding.
// It reserves 4 KiB for fixed Job keys/scalars and a control-sized assignment envelope.
func httpJobBound(job Job) int64 {
	size := int64(httpControlBytes + 4096)
	add := func(n, multiplier int64) {
		if n > (math.MaxInt64-size)/multiplier {
			size = math.MaxInt64
			return
		}
		size += n * multiplier
	}
	text := func(value string) {
		add(3, 1) // Quotes and a container separator.
		add(int64(len(value)), 6)
	}
	data := func(value []byte) {
		add(4, 1) // null, or base64 quotes and separators.
		add(int64(len(value))/3, 4)
		if len(value)%3 != 0 {
			add(4, 1)
		}
	}
	for _, value := range []string{
		job.Identity.JobID, job.Identity.AttemptID, job.Identity.ExecutorID,
		job.Identity.Repository, job.Identity.Requester, job.Identity.SubjectType,
		job.Command, string(job.Action), job.Title, job.Description, job.Notes,
		job.Source.Head, job.Source.Base, job.Source.MergeBase,
	} {
		text(value)
	}
	role := func(settings RoleSettings) {
		text(settings.Provider)
		text(settings.Model)
		text(settings.Effort)
		for _, sources := range [][]string{settings.Skills, settings.AlwaysOnSkills} {
			for _, source := range sources {
				text(source)
			}
		}
	}
	role(job.Main)
	if job.Critic != nil {
		role(*job.Critic)
	}
	for _, files := range [][]workspace.File{job.Source.Files, job.Source.BaseFiles} {
		for _, file := range files {
			add(64, 1) // File object keys, mode, braces and separator.
			text(file.Path)
			data(file.Data)
		}
	}
	data(job.Source.Repository)
	data(job.Source.Diff)
	for _, paths := range [][]string{job.Source.Omitted, job.Source.HeadOmitted, job.Source.ChangedPaths} {
		for _, path := range paths {
			text(path)
		}
	}
	return size
}
