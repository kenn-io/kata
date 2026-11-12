package main

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newCronUsageCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use: "usage <run-uid>", Short: "show live usage for a recorded run",
		Long:    "Show live AgentsView cost and per-session usage for a recorded cron run.",
		Example: "  kata cron run usage 01HXYZ00000000000000000000\n  kata cron run usage 01HXYZ00000000000000000000 --refresh",
		Args:    cobra.ExactArgs(1),
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "refresh cached usage older than ten seconds")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := normalizedCronCLIUID(args[0])
		if err != nil {
			return err
		}
		daemon, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		var project projectRef
		if strings.TrimSpace(flags.Project) != "" {
			project, err = resolveProjectSelectorIncludingArchived(daemon, flags.Project)
		} else {
			project, err = resolveProjectArgFlagOrWorkspace(daemon, nil)
		}
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
		if err != nil {
			return err
		}
		caps, err := client.GetCronCapabilitiesWithResponse(daemon.ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: project.ID}})
		if caps == nil {
			return externalCLITransportError(caps, err)
		}
		_, err = cronCLIResponse(caps.StatusCode, caps.Body, err)
		if err != nil {
			return err
		}
		if caps.JSON200 == nil || caps.JSON200.Usage.Source != generated.Agentsview {
			return errors.New("usage source is disabled or unsupported on the selected daemon")
		}
		hc, err := longRunningClientForResolved(cmd.Context(), daemon.resolved)
		if err != nil {
			return err
		}
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		usageClient, err := kataclient.NewWithHTTPClient(daemon.baseURL, hc)
		if err != nil {
			return err
		}
		response, err := usageClient.WithResponseLimit(2<<20).GetCronRunUsageWithResponse(daemon.ctx, &generated.GetCronRunUsageRequestOptions{
			PathParams: &generated.GetCronRunUsagePath{ProjectID: project.ID, RunUID: id},
			Query:      &generated.GetCronRunUsageQuery{Refresh: &refresh},
		})
		if response == nil {
			return externalCLITransportError(response, err)
		}
		body, err := cronCLIResponse(response.StatusCode, response.Body, err)
		if err != nil {
			return err
		}
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), jsontext.Value(body))
		}
		if response.JSON200 == nil {
			return errors.New("cron usage response is empty")
		}
		if flags.Quiet {
			return nil
		}
		return writeCronUsage(cmd.OutOrStdout(), *response.JSON200)
	}
	return cmd
}

func usageAmount(value int64, estimated bool) string {
	cents := value / 10000
	if value%10000 >= 5000 {
		cents++
	}
	prefix := ""
	if estimated {
		prefix = "~"
	}
	if value > 0 && cents == 0 {
		return prefix + "<$0.01"
	}
	return fmt.Sprintf("%s$%d.%02d", prefix, cents/100, cents%100)
}

func writeCronUsage(out io.Writer, usage generated.RunUsage) error {
	switch usage.State {
	case "no_sessions", "unknown", "complete", "partial":
	default:
		return errors.New("unsupported cron usage state")
	}
	for _, session := range usage.Sessions {
		switch session.State {
		case "priced", "unpriced", "no_usage", "not_found", "ambiguous", "unauthorized", "unavailable":
		default:
			return errors.New("unsupported cron session usage state")
		}
	}
	total := "no sessions"
	switch usage.State {
	case "unknown":
		total = "unknown"
	case "complete", "partial":
		if usage.CostMicrodollars == nil {
			total = "unknown"
			break
		}
		total = usageAmount(*usage.CostMicrodollars, usage.Estimated)
		if usage.State == "partial" {
			missing := 0
			for _, s := range usage.Sessions {
				if s.State != "priced" {
					missing++
				}
			}
			total += fmt.Sprintf("+ (%d of %d sessions not priced)", missing, len(usage.Sessions))
		}
	}
	if _, err := fmt.Fprintln(out, "Cost:", total); err != nil {
		return err
	}
	if usage.SessionsIncomplete {
		if _, err := fmt.Fprintln(out, "The recorded session list is incomplete; the total may omit usage."); err != nil {
			return err
		}
	}
	for _, s := range usage.Sessions {
		desc := "AgentsView unavailable"
		switch s.State {
		case "priced":
			if s.CostMicrodollars != nil {
				desc = usageAmount(*s.CostMicrodollars, s.CostSource != nil && (*s.CostSource == "computed" || *s.CostSource == "mixed"))
			}
		case "unpriced":
			desc = "unpriced"
			if len(s.UnpricedModels) > 0 {
				desc += " (" + textsafe.Line(strings.Join(s.UnpricedModels, ", ")) + ")"
			}
		case "no_usage":
			desc = "no usage recorded"
		case "not_found":
			desc = "not in AgentsView yet"
		case "ambiguous":
			desc = "ambiguous session"
			if len(s.Candidates) > 0 {
				desc += " (" + textsafe.Line(strings.Join(s.Candidates, ", ")) + ")"
			}
		case "unauthorized":
			desc = "AgentsView rejected Kata's token"
		}
		step := ""
		if s.Step != nil {
			step = " " + textsafe.Line(*s.Step)
		}
		link := ""
		if s.URL != nil && *s.URL != "" {
			link = " " + textsafe.Line(*s.URL)
		}
		if _, err := fmt.Fprintf(out, "%s %s%s: %s%s\n", textsafe.Line(s.Agent), textsafe.Line(s.SessionID), step, desc, link); err != nil {
			return err
		}
	}
	return nil
}
