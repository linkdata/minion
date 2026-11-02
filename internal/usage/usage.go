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
	Input              int64 `json:"input_tokens"`
	CacheCreationInput int64 `json:"cache_creation_input_tokens,omitempty"`
	CachedInput        int64 `json:"cached_input_tokens"`
	Output             int64 `json:"output_tokens"`
}

// Valid checks nonnegative counters, cache subsets and total overflow.
func (t Tokens) Valid() bool {
	return t.Input >= 0 && t.CachedInput >= 0 && t.CachedInput <= t.Input &&
		t.CacheCreationInput >= 0 && t.CacheCreationInput <= t.Input-t.CachedInput &&
		t.Output >= 0 && t.Output <= math.MaxInt64-t.Input
}

// Cached and cache-creation input are disjoint subsets of input tokens.
func (t Tokens) Total() int64 { return t.Input + t.Output }

// AddChecked sums normalized counters, retaining the receiver on overflow.
func (t Tokens) AddChecked(other Tokens) (Tokens, bool) {
	if !t.Valid() || !other.Valid() || other.Total() > math.MaxInt64-t.Total() {
		return t, false
	}
	t.Input += other.Input
	t.CacheCreationInput += other.CacheCreationInput
	t.CachedInput += other.CachedInput
	t.Output += other.Output
	return t, true
}

// Sub returns raw component differences; callers validate the resulting delta.
func (t Tokens) Sub(other Tokens) Tokens {
	return Tokens{Input: t.Input - other.Input, CacheCreationInput: t.CacheCreationInput - other.CacheCreationInput,
		CachedInput: t.CachedInput - other.CachedInput, Output: t.Output - other.Output}
}

// Max returns component maxima as a conservative floor. Its result may violate
// the disjoint cache-subset invariant even when both operands are valid; callers
// must validate it before using it as a token count.
func (t Tokens) Max(other Tokens) Tokens {
	return Tokens{Input: max(t.Input, other.Input), CacheCreationInput: max(t.CacheCreationInput, other.CacheCreationInput),
		CachedInput: max(t.CachedInput, other.CachedInput), Output: max(t.Output, other.Output)}
}

// Coverage describes the scope and gaps of an executor-reported token claim.
// PartialReport means the observed counters may be a subset of the invocation;
// an unusable present final report is MalformedFinalReport, not MissingFinalReport.
// An empty value is permitted for older attempt records and zero-token calls.
// Billed credits are not inferred from these counters.
type Coverage struct {
	Scope                  string `json:"scope,omitempty"`
	MissingFinalReport     bool   `json:"missing_final_report,omitempty"`
	MalformedFinalReport   bool   `json:"malformed_final_report,omitempty"`
	UnverifiedHelpers      bool   `json:"unverified_helpers,omitempty"`
	PartialReport          bool   `json:"partial_report,omitempty"`
	CumulativeUnattributed bool   `json:"cumulative_unattributed,omitempty"`
}

// Call is one admitted direct provider call; the provider process may not start.
// StartedAt is the BeforeCall checkpoint time. The enclosing Record's job and
// attempt IDs with Ordinal identify the call across checkpoint replays. A nil
// FinishedAt means no usage receipt was saved.
type Call struct {
	Ordinal       int        `json:"ordinal"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Effort        string     `json:"reasoning_effort,omitempty"`
	Role          string     `json:"role"`
	Round         int        `json:"round"`
	ReviewPhase   string     `json:"review_phase,omitempty"`
	Tokens        Tokens     `json:"tokens"`
	UsageCoverage Coverage   `json:"usage_coverage,omitzero"`
	Complete      bool       `json:"usage_complete"`
}

func (c Coverage) Valid() bool {
	switch c.Scope {
	case "", "invocation", "root_only", "known_subset", "unknown", "attempt":
		return true
	default:
		return false
	}
}

// ValidForCall and ValidForAttempt check claim consistency. A missing or bad
// final CLI report can coexist with complete rollout accounting, and a resumed
// cumulative report can be ignored when a current-turn delta is proven.
func (c Coverage) ValidForCall(complete bool) bool {
	return c.Valid() && c.Scope != "attempt" && !(c.MissingFinalReport && c.MalformedFinalReport) &&
		(!complete || (c.Scope == "" || c.Scope == "invocation") && !c.UnverifiedHelpers && !c.PartialReport)
}

func (c Coverage) ValidForAttempt(complete bool) bool {
	return c.Valid() && (c.Scope == "" || c.Scope == "attempt") &&
		(!complete || !c.UnverifiedHelpers && !c.PartialReport)
}

// AddGaps retains reasons reported by each call in an attempt.
func (c Coverage) AddGaps(other Coverage) Coverage {
	c.MissingFinalReport = c.MissingFinalReport || other.MissingFinalReport
	c.MalformedFinalReport = c.MalformedFinalReport || other.MalformedFinalReport
	c.UnverifiedHelpers = c.UnverifiedHelpers || other.UnverifiedHelpers
	c.PartialReport = c.PartialReport || other.PartialReport
	c.CumulativeUnattributed = c.CumulativeUnattributed || other.CumulativeUnattributed
	return c
}

// Record contains the usage reported by all model calls in one action attempt.
//
// Model and Effort describe the main model. The model name "default" denotes
// the provider CLI's selected default.
type Record struct {
	ID             string     `json:"id"`
	JobID          string     `json:"job_id"`
	Repo           string     `json:"repo"`
	RepositoryID   int64      `json:"repository_id,omitempty"`
	User           string     `json:"user"`
	RequesterID    int64      `json:"requester_id,omitempty"`
	Action         string     `json:"action"`
	Number         int        `json:"number"`
	SourceURL      string     `json:"source_url,omitempty"`
	ExecutorID     string     `json:"executor_id,omitempty"`
	SourceRef      string     `json:"source_ref,omitempty"`
	UsageSource    string     `json:"usage_source,omitempty"`
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
	UsageCoverage  Coverage   `json:"usage_coverage,omitzero"`
	UsageComplete  bool       `json:"usage_complete"`
	Calls          []Call     `json:"calls,omitempty"`
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
	if r.RepositoryID < 0 || r.RequesterID < 0 {
		return fmt.Errorf("usage record %s has invalid numeric identity", r.ID)
	}
	if (r.ExecutorID == "") != (r.SourceRef == "") ||
		(r.ExecutorID != "" && !workspace.ValidIdentitySegment(r.ExecutorID)) ||
		(r.SourceRef != "" && !workspace.ValidSHA(r.SourceRef)) ||
		(r.UsageSource != "" && r.UsageSource != "executor_reported") {
		return fmt.Errorf("usage record %s has invalid provenance", r.ID)
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
	if r.CodexCalls < 0 || r.ClaudeCalls < 0 || !r.Tokens.Valid() || !r.UsageCoverage.ValidForAttempt(r.UsageComplete) {
		return fmt.Errorf("usage record %s has invalid counts", r.ID)
	}
	if len(r.Calls) != 0 {
		if r.RepositoryID <= 0 || r.RequesterID <= 0 {
			return fmt.Errorf("usage record %s has invalid call identity", r.ID)
		}
		codex, claude, tokens, coverage, complete, source, err := r.callTotals()
		if err != nil {
			return fmt.Errorf("usage record %s has invalid calls: %w", r.ID, err)
		}
		if r.CodexCalls != codex || r.ClaudeCalls != claude || r.Tokens != tokens ||
			r.UsageCoverage != coverage || r.UsageComplete != complete || r.UsageSource != source {
			return fmt.Errorf("usage record %s has inconsistent call totals", r.ID)
		}
	}
	counted := r.CodexCalls > 0 || r.ClaudeCalls > 0
	hasTokens := r.Tokens != (Tokens{})
	// A pending call has a counter before its returned usage claim exists.
	if (!counted && (hasTokens || r.UsageSource != "")) ||
		(counted && r.ExecutorID == "") ||
		(counted && (hasTokens || r.UsageComplete || r.Status == "succeeded") && r.UsageSource != "executor_reported") {
		return fmt.Errorf("usage record %s has unattributed usage", r.ID)
	}
	return nil
}

func (r Record) callTotals() (codex, claude int, tokens Tokens, coverage Coverage, complete bool, source string, err error) {
	coverage, complete = Coverage{Scope: "attempt"}, true
	for i, call := range r.Calls {
		if call.Ordinal != i+1 || call.StartedAt.IsZero() || call.Model == "" || call.Round < 1 ||
			(call.Role != "main" && call.Role != "critic") ||
			(call.Role == "main" && call.ReviewPhase != "") ||
			(call.Role == "critic" && call.ReviewPhase != "full" && call.ReviewPhase != "verification") ||
			(call.Role == "main" && call.Provider != r.Provider) ||
			(call.Role == "critic" && call.Provider != r.CriticProvider) ||
			!call.Tokens.Valid() || !call.UsageCoverage.ValidForCall(call.Complete) ||
			(call.Tokens != (Tokens{}) && call.UsageCoverage.Scope == "") {
			err = fmt.Errorf("invalid call ordinal %d", i+1)
			return
		}
		switch call.Provider {
		case "codex":
			codex++
		case "claude":
			claude++
		default:
			err = fmt.Errorf("invalid call provider %q", call.Provider)
			return
		}
		if call.FinishedAt == nil {
			if call.Complete || call.Tokens != (Tokens{}) || call.UsageCoverage != (Coverage{}) || i != len(r.Calls)-1 {
				err = fmt.Errorf("invalid pending call ordinal %d", call.Ordinal)
				return
			}
			complete, coverage.PartialReport = false, true
			continue
		}
		if call.FinishedAt.Before(call.StartedAt) {
			err = fmt.Errorf("invalid call finish time at ordinal %d", call.Ordinal)
			return
		}
		source = "executor_reported"
		coverage = coverage.AddGaps(call.UsageCoverage)
		var valid bool
		if tokens, valid = tokens.AddChecked(call.Tokens); !valid {
			complete, coverage.PartialReport = false, true
		}
		if !call.Complete {
			complete, coverage.PartialReport = false, true
		}
	}
	return
}

func sameCallMetadata(a, b Call) bool {
	return a.Ordinal == b.Ordinal && a.Provider == b.Provider && a.Model == b.Model &&
		a.Effort == b.Effort && a.Role == b.Role && a.Round == b.Round && a.ReviewPhase == b.ReviewPhase
}

// BeginCall saves an ordinal's pending metadata once. Replays with identical
// metadata do not change its original start time or reported usage.
func (r *Record) BeginCall(call Call) (bool, error) {
	if call.StartedAt.IsZero() || call.FinishedAt != nil || call.Tokens != (Tokens{}) ||
		call.UsageCoverage != (Coverage{}) || call.Complete || r.RepositoryID <= 0 || r.RequesterID <= 0 ||
		(len(r.Calls) == 0 && (r.CodexCalls != 0 || r.ClaudeCalls != 0)) {
		return false, fmt.Errorf("invalid usage call start ordinal %d", call.Ordinal)
	}
	if call.Ordinal > 0 && call.Ordinal <= len(r.Calls) {
		if sameCallMetadata(r.Calls[call.Ordinal-1], call) {
			return false, nil
		}
		return false, fmt.Errorf("conflicting usage call ordinal %d", call.Ordinal)
	}
	if call.Ordinal != len(r.Calls)+1 ||
		(len(r.Calls) != 0 && r.Calls[len(r.Calls)-1].FinishedAt == nil) {
		return false, fmt.Errorf("invalid usage call start ordinal %d", call.Ordinal)
	}
	next := *r
	next.Calls = append(slices.Clone(r.Calls), call)
	if err := next.reconcileCalls(); err != nil {
		return false, err
	}
	*r = next
	return true, nil
}

// DiscardPendingCall removes an unacknowledged start that did not reach the
// provider. A returned receipt or an earlier ordinal cannot be discarded.
func (r *Record) DiscardPendingCall(ordinal int) error {
	if ordinal == 0 || ordinal != len(r.Calls) || r.Calls[ordinal-1].FinishedAt != nil {
		return fmt.Errorf("invalid pending usage call ordinal %d", ordinal)
	}
	next := *r
	next.Calls = slices.Clone(r.Calls[:ordinal-1])
	if err := next.reconcileCalls(); err != nil {
		return err
	}
	*r = next
	return nil
}

// FinishCall records one returned usage claim. Identical replayed receipts are
// ignored, so a save error after atomic replacement cannot add tokens twice.
func (r *Record) FinishCall(call Call) (bool, error) {
	if call.Ordinal <= 0 || call.Ordinal > len(r.Calls) || call.FinishedAt == nil {
		return false, fmt.Errorf("invalid usage call receipt ordinal %d", call.Ordinal)
	}
	prior := r.Calls[call.Ordinal-1]
	if !sameCallMetadata(prior, call) {
		return false, fmt.Errorf("conflicting usage call ordinal %d", call.Ordinal)
	}
	if prior.FinishedAt != nil {
		if prior.Tokens == call.Tokens && prior.UsageCoverage == call.UsageCoverage && prior.Complete == call.Complete {
			return false, nil
		}
		return false, fmt.Errorf("conflicting usage call receipt ordinal %d", call.Ordinal)
	}
	if call.FinishedAt.Before(prior.StartedAt) {
		call.FinishedAt = &prior.StartedAt
	}
	next := *r
	next.Calls = slices.Clone(r.Calls)
	entry := &next.Calls[call.Ordinal-1]
	entry.FinishedAt, entry.Tokens, entry.UsageCoverage, entry.Complete = call.FinishedAt, call.Tokens, call.UsageCoverage, call.Complete
	if err := next.reconcileCalls(); err != nil {
		return false, err
	}
	*r = next
	return true, nil
}

func (r *Record) reconcileCalls() error {
	codex, claude, tokens, coverage, complete, source, err := r.callTotals()
	if err != nil {
		return err
	}
	r.CodexCalls, r.ClaudeCalls, r.Tokens = codex, claude, tokens
	r.UsageCoverage, r.UsageComplete, r.UsageSource = coverage, complete, source
	return nil
}

type Store struct{ Dir string }

// Load reads one attempt, returning false when its record does not exist.
func (s Store) Load(id string) (Record, bool, error) {
	if !validID.MatchString(id) {
		return Record{}, false, fmt.Errorf("invalid usage record ID %q", id)
	}
	root, err := os.OpenRoot(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	defer root.Close()
	name := id + ".json"
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if !info.Mode().IsRegular() {
		return Record{}, false, fmt.Errorf("invalid usage file %q", name)
	}
	record, err := readRecord(root, name)
	return record, err == nil, err
}

// Save atomically replaces one attempt and syncs its directory and parent on Unix.
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
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() || !validID.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			return nil, fmt.Errorf("invalid usage file %q", entry.Name())
		}
		record, err := readRecord(root, entry.Name())
		if err != nil {
			return nil, err
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

func readRecord(root *os.Root, name string) (Record, error) {
	// Root.Open shares delete access on Windows, allowing Save to replace open records.
	f, err := root.Open(name)
	if err != nil {
		return Record{}, err
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
	if err == nil && name != record.ID+".json" {
		err = fmt.Errorf("record ID does not match filename")
	}
	if err != nil {
		return Record{}, fmt.Errorf("read usage file %q: %w", name, err)
	}
	return record, nil
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
