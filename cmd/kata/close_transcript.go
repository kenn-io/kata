package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/harness"
	"go.kenn.io/kata/internal/transcript"
)

func closeTranscript(cmd *cobra.Command) *transcript.Transcript {
	cfg, err := config.ReadCloseTranscriptConfig()
	warn := func(message string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message) }
	if err != nil {
		warn(fmt.Sprintf("configuration unavailable (%v); skipping attachment", err))
		return nil
	}
	if !cfg.Enabled {
		return nil
	}
	agent, id := os.Getenv("KATA_TRANSCRIPT_AGENT"), os.Getenv("KATA_TRANSCRIPT_SESSION_ID")
	if agent == "" && id == "" {
		detection, outcome := harness.Detect(os.LookupEnv, harness.ProcessAncestry(), "")
		// Detected Codex without a session means its nonempty session aliases
		// conflict. Preserve that ambiguity warning alongside nested markers.
		if outcome == harness.Ambiguous || (outcome == harness.Detected && detection.Harness == "codex" && detection.SessionID == "") {
			warn("ambiguous current session; skipping attachment")
			return nil
		}
		agent, id = detection.Harness, detection.SessionID
	}
	ref := &transcript.Transcript{Agent: agent, SessionID: id}
	if err := ref.Validate(); err != nil {
		warn("current session unavailable or invalid; skipping attachment")
		return nil
	}
	// Preserve the exact harness UUID: no filesystem path, transcript body,
	// hook ownership hash, or another session is used as a fallback.
	if cfg.AgentsViewURL != "" {
		link, err := ref.Link(cfg.AgentsViewURL)
		if err != nil {
			warn("AgentsView URL invalid; attaching the session identifier only")
		} else {
			ref.URL = link
		}
	}
	return ref
}

// dropUnsupportedTranscript keeps provenance optional: older daemons reject
// the unknown field, so the close proceeds without it instead of failing.
func dropUnsupportedTranscript(
	ctx context.Context, cmd *cobra.Command, client *http.Client, baseURL string, body map[string]any,
) error {
	minimum := apiVersionCloseTranscript
	ref, _ := body["transcript"].(*transcript.Transcript)
	if ref != nil && (ref.Agent == "pi" || ref.Agent == "omp") {
		minimum = apiVersionCloseTranscriptHarnesses
	}
	err := requireDaemonAPIVersion(ctx, client, baseURL, minimum, "close transcript")
	if err == nil {
		return nil
	}
	tooOld, ok := errors.AsType[*cliError](err)
	if !ok || tooOld.Code != "daemon_api_too_old" {
		return err
	}
	message := tooOld.Message
	if minimum == apiVersionCloseTranscriptHarnesses {
		message = "daemon too old for " + ref.Agent + " transcripts"
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "close: transcript: "+message+"; skipping attachment")
	delete(body, "transcript")
	return nil
}
