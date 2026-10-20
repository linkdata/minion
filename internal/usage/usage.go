package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

type Tokens struct {
	Input       int64 `json:"input_tokens"`
	CachedInput int64 `json:"cached_input_tokens"`
	Output      int64 `json:"output_tokens"`
}

// Valid checks nonnegative counters, cached-input inclusion and total overflow.
func (t Tokens) Valid() bool {
	return t.Input >= 0 && t.CachedInput >= 0 && t.CachedInput <= t.Input && t.Output >= 0 && t.Output <= math.MaxInt64-t.Input
}

// Cached input is a subset of input tokens.
func (t Tokens) Total() int64 { return t.Input + t.Output }

// AddChecked sums normalized counters, retaining the receiver on overflow.
func (t Tokens) AddChecked(other Tokens) (Tokens, bool) {
	if !t.Valid() || !other.Valid() || other.Total() > math.MaxInt64-t.Total() {
		return t, false
	}
	t.Input += other.Input
	t.CachedInput += other.CachedInput
	t.Output += other.Output
	return t, true
}

// Sub returns raw component differences; callers validate the resulting delta.
func (t Tokens) Sub(other Tokens) Tokens {
	return Tokens{Input: t.Input - other.Input, CachedInput: t.CachedInput - other.CachedInput, Output: t.Output - other.Output}
}

// Max returns component maxima; callers validate that the combined total fits.
func (t Tokens) Max(other Tokens) Tokens {
	return Tokens{Input: max(t.Input, other.Input), CachedInput: max(t.CachedInput, other.CachedInput), Output: max(t.Output, other.Output)}
}

// Record contains the usage reported by all model calls in one action attempt.
//
// Model and Effort describe the main model. The model name "default" denotes
// the provider CLI's selected default.
type Record struct {
	ID             string     `json:"id"`
	JobID          string     `json:"job_id"`
	Repo           string     `json:"repo"`
	User           string     `json:"user"`
	Action         string     `json:"action"`
	Number         int        `json:"number"`
	SourceURL      string     `json:"source_url,omitempty"`
	Provider       string     `json:"provider"`
	Model          string     `json:"model"`
	Effort         string     `json:"reasoning_effort,omitempty"`
	CriticProvider string     `json:"critic_provider,omitempty"`
	CriticModel    string     `json:"critic_model,omitempty"`
	CriticEffort   string     `json:"critic_effort,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Status         string     `json:"status"`
	CodexCalls     int        `json:"codex_calls"`
	ClaudeCalls    int        `json:"claude_calls,omitempty"`
	Tokens         Tokens     `json:"tokens"`
	UsageComplete  bool       `json:"usage_complete"`
}

var validID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (r Record) validate() error {
	if !validID.MatchString(r.ID) {
		return fmt.Errorf("invalid usage record ID %q", r.ID)
	}
	if r.JobID == "" || r.Repo == "" || r.User == "" || r.Action == "" || r.Model == "" || r.StartedAt.IsZero() {
		return fmt.Errorf("usage record %s is missing required metadata", r.ID)
	}
	if r.Provider != "codex" && r.Provider != "claude" {
		return fmt.Errorf("usage record %s has invalid provider %q", r.ID, r.Provider)
	}
	if r.CriticProvider != "" && r.CriticProvider != "codex" && r.CriticProvider != "claude" {
		return fmt.Errorf("usage record %s has invalid critic provider %q", r.ID, r.CriticProvider)
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
	if r.CodexCalls < 0 || r.ClaudeCalls < 0 || !r.Tokens.Valid() {
		return fmt.Errorf("usage record %s has invalid counts", r.ID)
	}
	return nil
}

type Store struct{ Dir string }

// Save atomically replaces one attempt and syncs its directory and parent.
//
// The caller serializes updates to an ID. An error after replacement can leave
// the new attempt in place.
func (s Store) Save(record Record) error {
	if err := record.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	return workspace.WriteAtomic(filepath.Join(s.Dir, record.ID+".json"), append(data, '\n'), filepath.Dir(s.Dir))
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

// Summary aggregates action attempts and token usage across both providers.
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
	ClaudeCalls     int    `json:"claude_calls,omitempty"`
	IncompleteUsage int    `json:"incomplete_usage"`
	Tokens          Tokens `json:"tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}

// Summarize aggregates matching records and rejects invalid matches, filters, groups
// and unrepresentable aggregate counts.
func Summarize(records []Record, filter Filter, groupBy []string) ([]Summary, error) {
	if !filter.Since.IsZero() && !filter.Until.IsZero() && !filter.Since.Before(filter.Until) {
		return nil, fmt.Errorf("since must precede until")
	}
	indices := make([]int, 0, len(groupBy))
	for _, group := range groupBy {
		index := slices.Index([]string{"repo", "user", "action", "day", "month"}, group)
		if index < 0 || slices.Contains(indices, index) {
			return nil, fmt.Errorf("invalid or duplicate usage group %q", group)
		}
		indices = append(indices, index)
	}
	groups := map[[5]string]*Summary{}
	for _, record := range records {
		if !filter.Match(record) {
			continue
		}
		if err := record.validate(); err != nil {
			return nil, err
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
		}
		if record.CodexCalls > math.MaxInt-summary.CodexCalls || record.ClaudeCalls > math.MaxInt-summary.ClaudeCalls {
			return nil, errors.New("usage call counts exceed integer limit")
		}
		summary.CodexCalls += record.CodexCalls
		summary.ClaudeCalls += record.ClaudeCalls
		if !record.UsageComplete || record.Status == "running" {
			summary.IncompleteUsage++
		}
		tokens, valid := summary.Tokens.AddChecked(record.Tokens)
		if !valid {
			return nil, errors.New("usage token counts are invalid or exceed integer limit")
		}
		summary.Tokens = tokens
		summary.TotalTokens = tokens.Total()
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
