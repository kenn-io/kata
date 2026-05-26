package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

func newHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "report daemon health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// health is a probe — it must report the daemon's actual
			// state, not auto-start one and report on the spawned
			// child. Hammer-test finding #1.
			baseURL, err := discoverDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/health", nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			var b struct {
				OK            bool   `json:"ok"`
				SchemaVersion int    `json:"schema_version"`
				Uptime        string `json:"uptime"`
				DBPath        string `json:"db_path"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			mode := currentOutputMode()
			if mode == outputAgent {
				daemonStatus := "unhealthy"
				if b.OK {
					daemonStatus = "running"
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK health ok=%t daemon=%s\n", b.OK, daemonStatus)
				return err
			}
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok=%v schema_version=%d uptime=%s db=%s\n",
				b.OK, b.SchemaVersion, b.Uptime, b.DBPath)
			return err
		},
	}
}
