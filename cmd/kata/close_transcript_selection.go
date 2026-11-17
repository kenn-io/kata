package main

import (
	"context"
	"net/http"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
)

// dropUnrecordedTranscript keeps the candidate session off an unkeyed close
// that will not record, including an unconfigured project under a disabled
// global default. A keyed close still carries it so a retry keeps the
// identity of the original request.
func dropUnrecordedTranscript(
	ctx context.Context, cmd *cobra.Command, client *http.Client, baseURL string, body map[string]any, unconfigured, keyed bool,
) error {
	if err := dropUnsupportedTranscript(ctx, cmd, client, baseURL, body, unconfigured); err != nil {
		return err
	}
	if selection, ok := body["transcript_selection"].(*api.TranscriptSelection); ok && !selection.Record && !keyed {
		delete(body, "transcript")
	}
	return nil
}
