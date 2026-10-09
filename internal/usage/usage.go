package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Tokens struct {
	Input       int64 `json:"input_tokens"`
	CachedInput int64 `json:"cached_input_tokens"`
	Output      int64 `json:"output_tokens"`
}

// Cached input is a subset of input tokens.
func (t Tokens) Total() int64 { return t.Input + t.Output }

func (t *Tokens) Add(other Tokens) {
	t.Input += other.Input
	t.CachedInput += other.CachedInput
	t.Output += other.Output
}

type Record struct {
	ID            string     `json:"id"`
	JobID         string     `json:"job_id"`
	Repo          string     `json:"repo"`
	User          string     `json:"user"`
	Action        string     `json:"action"`
	Number        int        `json:"number"`
	SourceURL     string     `json:"source_url,omitempty"`
	Model         string     `json:"model"`
	Effort        string     `json:"reasoning_effort,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Status        string     `json:"status"`
	CodexCalls    int        `json:"codex_calls"`
	Tokens        Tokens     `json:"tokens"`
	UsageComplete bool       `json:"usage_complete"`
}

var validID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (r Record) validate() error {
	if !validID.MatchString(r.ID) {
		return fmt.Errorf("invalid usage record ID %q", r.ID)
	}
	if r.JobID == "" || r.Repo == "" || r.User == "" || r.Action == "" || r.Model == "" || r.StartedAt.IsZero() {
		return fmt.Errorf("usage record %s is missing required metadata", r.ID)
	}
	switch r.Status {
	case "running":
		if r.FinishedAt != nil {
			return fmt.Errorf("running usage record %s has a finish time", r.ID)
		}
	case "succeeded", "failed":
		if r.FinishedAt == nil || r.FinishedAt.Before(r.StartedAt) {
			return fmt.Errorf("finished usage record %s has an invalid finish time", r.ID)
		}
	default:
		return fmt.Errorf("usage record %s has invalid status %q", r.ID, r.Status)
	}
	if r.CodexCalls < 0 || r.Tokens.Input < 0 || r.Tokens.CachedInput < 0 || r.Tokens.Output < 0 || r.Tokens.CachedInput > r.Tokens.Input {
		return fmt.Errorf("usage record %s has invalid counts", r.ID)
	}
	return nil
}

type Store struct{ Dir string }

// Save atomically replaces one attempt and syncs its directory.
//
// The caller serializes updates to an ID. An error after replacement can leave
// the new attempt in place.
func (s Store) Save(record Record) (err error) {
	if err := record.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".usage-*")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(f.Name()); !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	err = json.NewEncoder(f).Encode(record)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return
	}
	if err := os.Rename(f.Name(), filepath.Join(s.Dir, record.ID+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	return
}

// ReadAll reads complete usage records from dir.
//
// It sees a complete version of each file, including while [Store.Save] replaces it.
func ReadAll(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() || !validID.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			return nil, fmt.Errorf("invalid usage file %q", entry.Name())
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record Record
		decoder := json.NewDecoder(f)
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&record)
		if err == nil {
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				err = fmt.Errorf("trailing data")
			}
		}
		err = errors.Join(err, f.Close())
		if err == nil {
			err = record.validate()
		}
		if err == nil && entry.Name() != record.ID+".json" {
			err = fmt.Errorf("record ID does not match filename")
		}
		if err != nil {
			return nil, fmt.Errorf("read usage file %q: %w", entry.Name(), err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}

type Filter struct {
	Repo, User   string
	Since, Until time.Time
}

func (f Filter) Match(record Record) bool {
	return (f.Repo == "" || strings.EqualFold(f.Repo, record.Repo)) &&
		(f.User == "" || strings.EqualFold(f.User, record.User)) &&
		(f.Since.IsZero() || !record.StartedAt.Before(f.Since)) &&
		(f.Until.IsZero() || record.StartedAt.Before(f.Until))
}

type Summary struct {
	Repo            string `json:"repo,omitempty"`
	User            string `json:"user,omitempty"`
	Action          string `json:"action,omitempty"`
	Day             string `json:"day,omitempty"`
	Month           string `json:"month,omitempty"`
	Attempts        int    `json:"attempts"`
	Succeeded       int    `json:"succeeded"`
	Failed          int    `json:"failed"`
	Running         int    `json:"running"`
	CodexCalls      int    `json:"codex_calls"`
	IncompleteUsage int    `json:"incomplete_usage"`
	Tokens          Tokens `json:"tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}

func Summarize(records []Record, filter Filter, groupBy []string) ([]Summary, error) {
	if !filter.Since.IsZero() && !filter.Until.IsZero() && !filter.Since.Before(filter.Until) {
		return nil, fmt.Errorf("since must precede until")
	}
	indices := make([]int, 0, len(groupBy))
	used := map[string]bool{}
	for _, group := range groupBy {
		index := -1
		for i, name := range []string{"repo", "user", "action", "day", "month"} {
			if group == name {
				index = i
			}
		}
		if index < 0 || used[group] {
			return nil, fmt.Errorf("invalid or duplicate usage group %q", group)
		}
		used[group] = true
		indices = append(indices, index)
	}
	groups := map[[5]string]*Summary{}
	for _, record := range records {
		if !filter.Match(record) {
			continue
		}
		values := [5]string{strings.ToLower(record.Repo), strings.ToLower(record.User), record.Action, record.StartedAt.UTC().Format("2006-01-02"), record.StartedAt.UTC().Format("2006-01")}
		var key [5]string
		for _, i := range indices {
			key[i] = values[i]
		}
		summary := groups[key]
		if summary == nil {
			summary = &Summary{Repo: key[0], User: key[1], Action: key[2], Day: key[3], Month: key[4]}
			groups[key] = summary
		}
		summary.Attempts++
		switch record.Status {
		case "succeeded":
			summary.Succeeded++
		case "failed":
			summary.Failed++
		case "running":
			summary.Running++
		default:
			return nil, fmt.Errorf("usage record %s has invalid status %q", record.ID, record.Status)
		}
		summary.CodexCalls += record.CodexCalls
		if !record.UsageComplete || record.Status == "running" {
			summary.IncompleteUsage++
		}
		summary.Tokens.Add(record.Tokens)
		summary.TotalTokens = summary.Tokens.Total()
	}
	keys := make([][5]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		for _, column := range indices {
			if keys[i][column] != keys[j][column] {
				return keys[i][column] < keys[j][column]
			}
		}
		return false
	})
	result := make([]Summary, 0, len(keys))
	for _, key := range keys {
		result = append(result, *groups[key])
	}
	return result, nil
}
