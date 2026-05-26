package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newDigestCmd() *cobra.Command {
	var (
		sinceStr     string
		untilStr     string
		projectIDArg int64
		allProjects  bool
		actors       []string
	)
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "summarize recent activity by actor (created / closed / commented / unblocked / ...)",
		Long: `kata digest reads the event stream over a time window and prints a
human-readable changelog grouped by actor. Use --since with a duration
(e.g. 24h, 7d) or an RFC3339 timestamp; --until defaults to now.

By default, digest is scoped to the current workspace's project. Use
--project-id to scope to a specific project, or --all-projects for a
cross-project digest.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if allProjects && projectIDArg != 0 {
				return &cliError{
					Message:  "--all-projects and --project-id are mutually exclusive",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			if strings.TrimSpace(flags.Project) != "" && (allProjects || projectIDArg != 0) {
				return &cliError{
					Message:  "--project cannot be combined with --all-projects or --project-id",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			if strings.TrimSpace(sinceStr) == "" {
				return &cliError{
					Message:  "--since is required (e.g. --since 24h)",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			now := time.Now().UTC()
			since, err := parseSinceUntil(sinceStr, now)
			if err != nil {
				return &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
			}
			var until time.Time
			if untilStr == "" {
				until = now
			} else {
				until, err = parseSinceUntil(untilStr, now)
				if err != nil {
					return &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
				}
			}
			if !until.After(since) {
				return &cliError{
					Message:  "--until must be strictly after --since",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}

			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			getURL, err := digestURL(ctx, baseURL, digestURLOpts{
				ProjectIDArg: projectIDArg,
				AllProjects:  allProjects,
				Since:        since,
				Until:        until,
				Actors:       actors,
			})
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, getURL, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			mode := currentOutputMode()
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			if mode == outputAgent {
				return printDigestAgent(cmd, bs)
			}
			return printDigestHuman(cmd, bs)
		},
	}
	cmd.Flags().StringVar(&sinceStr, "since", "", "window start (duration like 24h or RFC3339)")
	cmd.Flags().StringVar(&untilStr, "until", "", "window end (default: now)")
	cmd.Flags().Int64Var(&projectIDArg, "project-id", 0, "scope to a specific project id")
	cmd.Flags().BoolVar(&allProjects, "all-projects", false, "summarize all projects")
	cmd.Flags().StringSliceVar(&actors, "actor", nil, "limit to one or more actors (repeatable)")
	return cmd
}

// parseSinceUntil accepts either a Go duration (e.g. 24h, 30m, 7d) or an
// RFC3339 timestamp. Durations are interpreted as "now minus duration"; the
// "d" suffix is expanded to hours since Go's time.ParseDuration doesn't
// support days natively.
func parseSinceUntil(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	expanded := s
	if strings.HasSuffix(s, "d") {
		num := strings.TrimSuffix(s, "d")
		// Trust no integer-overflow wraparound here: a multi-year window is
		// the user's call, and ParseDuration will reject pathological inputs
		// (e.g. trailing "d" with non-numeric prefix).
		expanded = num + "h"
		if d, err := time.ParseDuration(expanded); err == nil {
			return now.Add(-24 * d).UTC(), nil
		}
	}
	d, err := time.ParseDuration(expanded)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time spec %q: must be a duration (24h, 7d) or RFC3339 timestamp", s)
	}
	return now.Add(-d).UTC(), nil
}

type digestURLOpts struct {
	ProjectIDArg int64
	AllProjects  bool
	Since        time.Time
	Until        time.Time
	Actors       []string
}

func digestURL(ctx context.Context, baseURL string, opts digestURLOpts) (string, error) {
	q := url.Values{}
	// RFC3339Nano (not plain RFC3339) so the daemon's millisecond-precision
	// `created_at <= until` doesn't truncate the just-emitted event out of
	// the window when --until defaults to "now".
	q.Set("since", opts.Since.UTC().Format(time.RFC3339Nano))
	q.Set("until", opts.Until.UTC().Format(time.RFC3339Nano))
	for _, a := range opts.Actors {
		q.Add("actor", a)
	}
	switch {
	case opts.AllProjects:
		return baseURL + "/api/v1/digest?" + q.Encode(), nil
	case opts.ProjectIDArg != 0:
		return fmt.Sprintf("%s/api/v1/projects/%d/digest?%s", baseURL, opts.ProjectIDArg, q.Encode()), nil
	default:
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return "", err
		}
		pid, err := resolveProjectID(ctx, baseURL, start)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s/api/v1/projects/%d/digest?%s", baseURL, pid, q.Encode()), nil
	}
}

func printDigestHuman(cmd *cobra.Command, bs []byte) error {
	var b struct {
		Since      time.Time `json:"since"`
		Until      time.Time `json:"until"`
		EventCount int       `json:"event_count"`
		ProjectID  int64     `json:"project_id"`
		Totals     struct {
			Created    int `json:"created"`
			Closed     int `json:"closed"`
			Reopened   int `json:"reopened"`
			Commented  int `json:"commented"`
			Edited     int `json:"edited"`
			Assigned   int `json:"assigned"`
			Unassigned int `json:"unassigned"`
			Labeled    int `json:"labeled"`
			Unlabeled  int `json:"unlabeled"`
			Linked     int `json:"linked"`
			Unlinked   int `json:"unlinked"`
			Unblocked  int `json:"unblocked"`
		} `json:"totals"`
		Actors []struct {
			Actor  string `json:"actor"`
			Totals struct {
				Created   int `json:"created"`
				Closed    int `json:"closed"`
				Reopened  int `json:"reopened"`
				Commented int `json:"commented"`
				Unblocked int `json:"unblocked"`
			} `json:"totals"`
			Issues []struct {
				ProjectID    int64    `json:"project_id"`
				ProjectName  string   `json:"project_name"`
				IssueShortID string   `json:"issue_short_id"`
				IssueUID     string   `json:"issue_uid"`
				Actions      []string `json:"actions"`
			} `json:"issues"`
		} `json:"actors"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "digest %s → %s  (%d events)\n",
		b.Since.Format(time.RFC3339), b.Until.Format(time.RFC3339), b.EventCount); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out,
		"  totals: created=%d closed=%d reopened=%d commented=%d unblocked=%d\n",
		b.Totals.Created, b.Totals.Closed, b.Totals.Reopened, b.Totals.Commented, b.Totals.Unblocked); err != nil {
		return err
	}
	if len(b.Actors) == 0 {
		_, err := fmt.Fprintln(out, "  (no activity)")
		return err
	}
	for _, a := range b.Actors {
		if _, err := fmt.Fprintf(out,
			"\n%s — created=%d closed=%d reopened=%d commented=%d unblocked=%d\n",
			textsafe.Line(a.Actor), a.Totals.Created, a.Totals.Closed,
			a.Totals.Reopened, a.Totals.Commented, a.Totals.Unblocked); err != nil {
			return err
		}
		for _, iss := range a.Issues {
			prefix := iss.IssueShortID
			// On cross-project digests, prefix the project name so the
			// reader can disambiguate colliding short_ids.
			if b.ProjectID == 0 && iss.ProjectName != "" {
				prefix = fmt.Sprintf("%s#%s", textsafe.Line(iss.ProjectName), iss.IssueShortID)
			}
			if _, err := fmt.Fprintf(out, "  %-14s %s\n",
				prefix, textsafe.Line(strings.Join(iss.Actions, ", "))); err != nil {
				return err
			}
		}
	}
	return nil
}

func printDigestAgent(cmd *cobra.Command, bs []byte) error {
	var b struct {
		Since      time.Time `json:"since"`
		Until      time.Time `json:"until"`
		EventCount int       `json:"event_count"`
		Actors     []struct {
			Actor  string `json:"actor"`
			Issues []struct {
				ProjectName  string   `json:"project_name"`
				IssueShortID string   `json:"issue_short_id"`
				Actions      []string `json:"actions"`
			} `json:"issues"`
		} `json:"actors"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	count := 0
	for _, a := range b.Actors {
		count += len(a.Issues)
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "OK digest count=%d events=%d since=%s until=%s\n",
		count, b.EventCount,
		agentValue(b.Since.Format(time.RFC3339)),
		agentValue(b.Until.Format(time.RFC3339))); err != nil {
		return err
	}
	for _, a := range b.Actors {
		for _, iss := range a.Issues {
			fields := []agentField{
				agentRowField("actor", a.Actor),
				agentRowField("issue", iss.IssueShortID),
				agentRowListField("actions", iss.Actions),
			}
			if iss.ProjectName != "" {
				fields = append(fields, agentRowField("project", iss.ProjectName))
			}
			if err := writeAgentKVRow(out, fields...); err != nil {
				return err
			}
		}
	}
	return nil
}
