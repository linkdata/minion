package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

const usageTokenNote = "Token counts and completion are executor-reported claims, not verified billing. Cache creation and cache read are disjoint and included in input; total tokens = input + output. Attempt totals can include both main and critic providers. With a service filter, JSON detail records retain the whole attempt and add selected_usage; table tokens show the selected service. Status counts describe each matching attempt's overall outcome. Unknown service means token totals lack call attribution; call counters may still have provider labels. Repository and requester ID filters exclude records without the corresponding numeric IDs. Call counts include pending pre-call checkpoints, which may precede acknowledgment and do not prove a CLI invocation. Billed credits are unknown. Incomplete usage may omit unknown token counts. Actions without usage files are not counted."

type usageAttempt struct {
	usage.Record
	TotalTokens   int64          `json:"total_tokens"`
	SelectedUsage *usage.Summary `json:"selected_usage,omitempty"`
}

type usageOutput struct {
	GroupBy        []string `json:"group_by"`
	ActionCoverage string   `json:"action_coverage"`
	Filters        struct {
		Repo         string `json:"repo,omitempty"`
		RepositoryID int64  `json:"repository_id,omitempty"`
		User         string `json:"user,omitempty"`
		Service      string `json:"service,omitempty"`
		RequesterID  int64  `json:"requester_id,omitempty"`
		From         string `json:"from,omitempty"`
		To           string `json:"to,omitempty"`
	} `json:"filters"`
	Totals    usage.Summary   `json:"totals"`
	Groups    []usage.Summary `json:"groups"`
	Attempts  []usageAttempt  `json:"attempts"`
	TokenNote string          `json:"token_note"`
}

func runUsage(args []string) int {
	return runUsageWithOutput(args, os.Stdout, os.Stderr)
}

func runUsageWithOutput(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("minion usage", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.local.json", "path to JSON configuration")
	dir := flags.String("dir", "", "read usage records directly, without loading configuration")
	repo := flags.String("repo", "", "filter by owner/repo (case insensitive)")
	repositoryID := flags.Int64("repository-id", 0, "filter by numeric repository ID")
	user := flags.String("user", "", "filter by invoking GitHub login (case insensitive)")
	service := flags.String("service", "", "filter by call provider: codex, claude, or unknown")
	requesterID := flags.Int64("requester-id", 0, "filter by numeric requester ID")
	from := flags.String("from", "", "inclusive attempt start: YYYY-MM-DD (UTC) or RFC3339")
	to := flags.String("to", "", "exclusive attempt start: YYYY-MM-DD (UTC) or RFC3339")
	groupBy := flags.String("group-by", "repo,user,action", "comma-separated repo,repository_id,user,requester_id,service,action,day,month; none lists attempts")
	jsonOutput := flags.Bool("json", false, "output JSON instead of a table")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(code int, err error) int {
		fmt.Fprintf(stderr, "minion usage: %v\n", err) //nolint:errcheck // -- The command already fails; stderr is the final diagnostic sink.
		return code
	}
	if flags.NArg() != 0 {
		return fail(2, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " ")))
	}
	var dirSet, configSet, repositorySet, requesterSet bool
	flags.Visit(func(f *flag.Flag) {
		dirSet = dirSet || f.Name == "dir"
		configSet = configSet || f.Name == "config"
		repositorySet = repositorySet || f.Name == "repository-id"
		requesterSet = requesterSet || f.Name == "requester-id"
	})
	if dirSet && (configSet || strings.TrimSpace(*dir) == "") {
		return fail(2, fmt.Errorf("-dir must be nonempty and cannot be combined with -config"))
	}
	if repositorySet && *repositoryID <= 0 {
		return fail(2, fmt.Errorf("-repository-id must be positive"))
	}
	if requesterSet && *requesterID <= 0 {
		return fail(2, fmt.Errorf("-requester-id must be positive"))
	}
	since, err := parseUsageTime(*from)
	if err != nil {
		return fail(2, fmt.Errorf("-from: %w", err))
	}
	until, err := parseUsageTime(*to)
	if err != nil {
		return fail(2, fmt.Errorf("-to: %w", err))
	}
	filter := usage.Filter{Repo: strings.TrimSpace(*repo), RepositoryID: *repositoryID, User: strings.TrimSpace(*user), Service: strings.TrimSpace(*service), RequesterID: *requesterID, Since: since, Until: until}
	groups := strings.Split(*groupBy, ",")
	for i := range groups {
		groups[i] = strings.TrimSpace(groups[i])
	}
	details := len(groups) == 1 && groups[0] == "none"
	if details {
		groups = nil
	}
	if _, err := usage.Summarize(nil, filter, groups); err != nil {
		return fail(2, err)
	}
	if !dirSet {
		statePath, err := config.LoadStatePath(*configPath)
		if err != nil {
			return fail(2, fmt.Errorf("config: %w", err))
		}
		*dir = filepath.Join(filepath.Dir(statePath), "usage")
	}
	query, err := (usage.Store{Dir: *dir}).Query(filter, groups, details)
	if err != nil {
		return fail(1, err)
	}
	output := usageReport(filter, groups, query)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(output)
	} else {
		err = writeUsageTable(stdout, output, details)
	}
	if err != nil {
		return fail(1, fmt.Errorf("write report: %w", err))
	}
	return 0
}

// usageReport keeps CLI and admin exports on the same accounting contract.
func usageReport(filter usage.Filter, groups []string, query usage.QueryResult) usageOutput {
	output := usageOutput{GroupBy: []string{}, ActionCoverage: query.ActionCoverage, Attempts: []usageAttempt{}, TokenNote: usageTokenNote}
	output.Filters.Repo, output.Filters.RepositoryID, output.Filters.User = filter.Repo, filter.RepositoryID, filter.User
	output.Filters.Service, output.Filters.RequesterID = filter.Service, filter.RequesterID
	if !filter.Since.IsZero() {
		output.Filters.From = filter.Since.Format(time.RFC3339Nano)
	}
	if !filter.Until.IsZero() {
		output.Filters.To = filter.Until.Format(time.RFC3339Nano)
	}
	output.Totals = query.Totals
	if len(groups) != 0 {
		output.GroupBy = groups
	}
	output.Groups = query.Groups
	for _, detail := range query.Attempts {
		record := detail.Record
		record.UsageComplete = record.UsageComplete && record.Status != "running"
		attempt := usageAttempt{Record: record, TotalTokens: record.Tokens.Total()}
		if filter.Service != "" {
			selected := detail.Usage
			attempt.SelectedUsage = &selected
		}
		output.Attempts = append(output.Attempts, attempt)
	}
	return output
}

func parseUsageTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			if parsed.UTC().IsZero() {
				break
			}
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q must be a nonzero YYYY-MM-DD or RFC3339 time", value)
}

func writeUsageTable(stdout io.Writer, output usageOutput, details bool) error {
	var buffer bytes.Buffer
	w := tabwriter.NewWriter(&buffer, 0, 4, 2, ' ', 0)
	if details {
		fmt.Fprintln(w, "STARTED (UTC)\tREPO\tREPO ID\tUSER\tREQUESTER ID\tACTION\tSTATUS\tMAIN\tCRITIC\tCODEX CALLS\tCLAUDE CALLS\tINPUT\tCACHE CREATE\tCACHE READ\tOUTPUT\tTOTAL\tUSAGE\tUSAGE GAPS\tATTEMPT") //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		for _, attempt := range output.Attempts {
			critic := attempt.CriticProvider
			if critic == "" {
				critic = "-"
			}
			tokens, total := attempt.Tokens, attempt.TotalTokens
			codexCalls, claudeCalls := attempt.CodexCalls, attempt.ClaudeCalls
			complete := attempt.UsageComplete && attempt.Status != "running"
			gaps := usageGaps(attempt.UsageCoverage)
			if selected := attempt.SelectedUsage; selected != nil {
				tokens, total = selected.Tokens, selected.TotalTokens
				codexCalls, claudeCalls = selected.CodexCalls, selected.ClaudeCalls
				complete = selected.IncompleteUsage == 0
				coverage := attempt.UsageCoverage
				if output.Filters.Service != "unknown" {
					coverage = usage.Coverage{}
					for _, call := range attempt.Calls {
						if call.Provider == output.Filters.Service {
							coverage = coverage.AddGaps(call.UsageCoverage)
						}
					}
				}
				gaps = usageGaps(coverage)
				if !complete && gaps == "-" {
					gaps = "selected_service_incomplete"
				}
			}
			coverage := "complete"
			if !complete {
				coverage = "unknown"
				if total != 0 {
					coverage = "partial"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", attempt.StartedAt.UTC().Format(time.RFC3339), attempt.Repo, usageIdentity(attempt.RepositoryID), attempt.User, usageIdentity(attempt.RequesterID), attempt.Action, attempt.Status, attempt.Provider, critic, codexCalls, claudeCalls, tokens.Input, tokens.CacheCreationInput, tokens.CachedInput, tokens.Output, total, coverage, gaps, attempt.ID) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		}
	} else {
		for _, group := range output.GroupBy {
			fmt.Fprintf(w, "%s\t", strings.ToUpper(group)) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		}
		fmt.Fprintln(w, "ATTEMPTS\tACTIONS RECORDED\tSUCCEEDED\tFAILED\tRUNNING\tCODEX CALLS\tCLAUDE CALLS\tINPUT\tCACHE CREATE\tCACHE READ\tOUTPUT\tTOTAL\tUNALLOCATED\tINCOMPLETE") //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		for _, summary := range output.Groups {
			values := map[string]string{"repo": summary.Repo, "repository_id": usageIdentity(summary.RepositoryID), "user": summary.User, "requester_id": usageIdentity(summary.RequesterID), "service": summary.Service, "action": summary.Action, "day": summary.Day, "month": summary.Month}
			for _, group := range output.GroupBy {
				fmt.Fprintf(w, "%s\t", values[group]) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
			}
			fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", summary.Attempts, summary.UsageRecordedActions, summary.Succeeded, summary.Failed, summary.Running, summary.CodexCalls, summary.ClaudeCalls, summary.Tokens.Input, summary.Tokens.CacheCreationInput, summary.Tokens.CachedInput, summary.Tokens.Output, summary.TotalTokens, summary.UnallocatedTokens.Total(), summary.IncompleteUsage) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	t := output.Totals
	fmt.Fprintf(&buffer, "\nTotals: %d usage-recorded actions, %d attempts; %d succeeded, %d failed, %d running; %d Codex calls, %d Claude calls.\n", t.UsageRecordedActions, t.Attempts, t.Succeeded, t.Failed, t.Running, t.CodexCalls, t.ClaudeCalls)
	fmt.Fprintf(&buffer, "Tokens: %d input, %d cache creation, %d cache read, %d output, %d total, %d unallocated; %d attempts with incomplete usage.\n", t.Tokens.Input, t.Tokens.CacheCreationInput, t.Tokens.CachedInput, t.Tokens.Output, t.TotalTokens, t.UnallocatedTokens.Total(), t.IncompleteUsage)
	fmt.Fprintf(&buffer, "Accepted-action coverage: %s.\n", output.ActionCoverage)
	fmt.Fprintln(&buffer, usageTokenNote)
	if t.Running != 0 {
		fmt.Fprintln(&buffer, "Running attempts may be stale after an interruption.")
	}
	_, err := buffer.WriteTo(stdout)
	return err
}

func usageIdentity(id int64) string {
	if id == 0 {
		return "unknown"
	}
	return strconv.FormatInt(id, 10)
}

func usageGaps(c usage.Coverage) string {
	var gaps []string
	if c.MissingFinalReport {
		gaps = append(gaps, "missing_final_report")
	}
	if c.MalformedFinalReport {
		gaps = append(gaps, "malformed_final_report")
	}
	if c.UnverifiedHelpers {
		gaps = append(gaps, "unverified_helpers")
	}
	if c.PartialReport {
		gaps = append(gaps, "partial_report")
	}
	if c.CumulativeUnattributed {
		gaps = append(gaps, "cumulative_unattributed")
	}
	if len(gaps) == 0 {
		return "-"
	}
	return strings.Join(gaps, ",")
}
