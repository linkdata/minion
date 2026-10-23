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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

const usageTokenNote = "Returned-call token counts and completion flags are executor-reported claims, not verified billing. Cached input tokens are included in input tokens; total tokens = input + output. Incomplete usage may omit unknown token counts."

type usageAttempt struct {
	usage.Record
	TotalTokens int64 `json:"total_tokens"`
}

type usageOutput struct {
	GroupBy []string `json:"group_by"`
	Filters struct {
		Repo string `json:"repo,omitempty"`
		User string `json:"user,omitempty"`
		From string `json:"from,omitempty"`
		To   string `json:"to,omitempty"`
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
	user := flags.String("user", "", "filter by invoking GitHub login (case insensitive)")
	from := flags.String("from", "", "inclusive attempt start: YYYY-MM-DD (UTC) or RFC3339")
	to := flags.String("to", "", "exclusive attempt start: YYYY-MM-DD (UTC) or RFC3339")
	groupBy := flags.String("group-by", "repo,user,action", "comma-separated repo,user,action,day,month; none lists attempts")
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
	var dirSet, configSet bool
	flags.Visit(func(f *flag.Flag) {
		dirSet = dirSet || f.Name == "dir"
		configSet = configSet || f.Name == "config"
	})
	if dirSet && (configSet || strings.TrimSpace(*dir) == "") {
		return fail(2, fmt.Errorf("-dir must be nonempty and cannot be combined with -config"))
	}
	since, err := parseUsageTime(*from)
	if err != nil {
		return fail(2, fmt.Errorf("-from: %w", err))
	}
	until, err := parseUsageTime(*to)
	if err != nil {
		return fail(2, fmt.Errorf("-to: %w", err))
	}
	filter := usage.Filter{Repo: strings.TrimSpace(*repo), User: strings.TrimSpace(*user), Since: since, Until: until}
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
		cfg, err := config.Load(*configPath)
		if err != nil {
			return fail(2, fmt.Errorf("config: %w", err))
		}
		*dir = filepath.Join(filepath.Dir(cfg.StatePath), "usage")
	}
	records, err := usage.ReadAll(*dir)
	if err != nil {
		return fail(1, err)
	}
	output := usageOutput{GroupBy: []string{}, Groups: []usage.Summary{}, Attempts: []usageAttempt{}, TokenNote: usageTokenNote}
	output.Filters.Repo, output.Filters.User = filter.Repo, filter.User
	if !since.IsZero() {
		output.Filters.From = since.Format(time.RFC3339Nano)
	}
	if !until.IsZero() {
		output.Filters.To = until.Format(time.RFC3339Nano)
	}
	totals, err := usage.Summarize(records, filter, nil)
	if err != nil {
		return fail(1, err)
	}
	if len(totals) != 0 {
		output.Totals = totals[0]
	}
	if details {
		for _, record := range records {
			if filter.Match(record) {
				record.UsageComplete = record.UsageComplete && record.Status != "running"
				output.Attempts = append(output.Attempts, usageAttempt{Record: record, TotalTokens: record.Tokens.Total()})
			}
		}
	} else {
		output.GroupBy = groups
		output.Groups, err = usage.Summarize(records, filter, groups)
		if err != nil {
			return fail(1, err)
		}
	}
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

func parseUsageTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q must be YYYY-MM-DD or RFC3339", value)
}

func writeUsageTable(stdout io.Writer, output usageOutput, details bool) error {
	var buffer bytes.Buffer
	w := tabwriter.NewWriter(&buffer, 0, 4, 2, ' ', 0)
	if details {
		fmt.Fprintln(w, "STARTED (UTC)\tREPO\tUSER\tACTION\tSTATUS\tMAIN\tCRITIC\tCODEX CALLS\tCLAUDE CALLS\tINPUT\tCACHED\tOUTPUT\tTOTAL\tUSAGE\tATTEMPT") //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		for _, attempt := range output.Attempts {
			critic := attempt.CriticProvider
			if critic == "" {
				critic = "-"
			}
			coverage := "complete"
			if !attempt.UsageComplete || attempt.Status == "running" {
				coverage = "unknown"
				if attempt.TotalTokens != 0 {
					coverage = "partial"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", attempt.StartedAt.UTC().Format(time.RFC3339), attempt.Repo, attempt.User, attempt.Action, attempt.Status, attempt.Provider, critic, attempt.CodexCalls, attempt.ClaudeCalls, attempt.Tokens.Input, attempt.Tokens.CachedInput, attempt.Tokens.Output, attempt.TotalTokens, coverage, attempt.ID) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		}
	} else {
		for _, group := range output.GroupBy {
			fmt.Fprintf(w, "%s\t", strings.ToUpper(group)) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		}
		fmt.Fprintln(w, "ATTEMPTS\tSUCCEEDED\tFAILED\tRUNNING\tCODEX CALLS\tCLAUDE CALLS\tINPUT\tCACHED\tOUTPUT\tTOTAL\tINCOMPLETE") //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		for _, summary := range output.Groups {
			values := map[string]string{"repo": summary.Repo, "user": summary.User, "action": summary.Action, "day": summary.Day, "month": summary.Month}
			for _, group := range output.GroupBy {
				fmt.Fprintf(w, "%s\t", values[group]) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
			}
			fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", summary.Attempts, summary.Succeeded, summary.Failed, summary.Running, summary.CodexCalls, summary.ClaudeCalls, summary.Tokens.Input, summary.Tokens.CachedInput, summary.Tokens.Output, summary.TotalTokens, summary.IncompleteUsage) //nolint:errcheck // -- The tabwriter writes to an infallible bytes.Buffer.
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	t := output.Totals
	fmt.Fprintf(&buffer, "\nTotals: %d attempts; %d succeeded, %d failed, %d running; %d Codex calls, %d Claude calls.\n", t.Attempts, t.Succeeded, t.Failed, t.Running, t.CodexCalls, t.ClaudeCalls)
	fmt.Fprintf(&buffer, "Tokens: %d input, %d cached input, %d output, %d total; %d attempts with incomplete usage.\n", t.Tokens.Input, t.Tokens.CachedInput, t.Tokens.Output, t.TotalTokens, t.IncompleteUsage)
	fmt.Fprintln(&buffer, usageTokenNote)
	if t.Running != 0 {
		fmt.Fprintln(&buffer, "Running attempts may be stale after an interruption.")
	}
	_, err := buffer.WriteTo(stdout)
	return err
}
