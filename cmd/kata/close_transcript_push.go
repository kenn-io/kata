package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/processtree"
	"go.kenn.io/kata/internal/transcript"
)

var closeTranscriptPushCommand = exec.CommandContext

// finish runs after the close commits and before independent comment/output
// handling. Its failure must never turn a successful close into a failed one.
func (a closeTranscriptAttachment) finish(ctx context.Context, cmd *cobra.Command, body map[string]any, response []byte) {
	ref, ok := body["transcript"].(*transcript.Transcript)
	if !ok || ref == nil || !a.target.Push || body["dry_run"] == true {
		return
	}
	if ref.URL == "" {
		warnCloseTranscript(cmd, "session identifier recorded; upload unavailable")
		return
	}
	if err := confirmCloseTranscript(ref, a.projectID, response); err != nil {
		warnCloseTranscript(cmd, "URL-only locator recorded; "+err.Error()+"; upload skipped")
		return
	}
	if err := runCloseTranscriptPush(ctx, a.target, *ref); err != nil {
		// An error can follow remote acceptance. Do not claim nothing was uploaded,
		// and never echo a child's diagnostics, content, token, or destination.
		warnCloseTranscript(cmd, "URL-only locator recorded; upload unavailable or unconfirmed")
		return
	}
	warnCloseTranscript(cmd, "locator recorded; session upload accepted")
}

// AgentsView requires its exact stored ID, not a native-ID search. The agent
// check on push prevents a deterministic prefix from selecting another agent.
func agentsViewPushSessionID(ref transcript.Transcript) string {
	if ref.Agent == "claude" {
		return ref.SessionID
	}
	return ref.Agent + ":" + ref.SessionID
}

// Normal OS/config/archive settings are needed to select the local archive.
// No provider, Kata, or ambient AgentsView credentials are inherited.
var closeTranscriptPushEnv = []string{
	"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "WINDIR", "APPDATA", "LOCALAPPDATA",
	"TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "AGENTSVIEW_DATA_DIR", "AGENT_VIEWER_DATA_DIR",
}

func runCloseTranscriptPush(ctx context.Context, target config.CloseTranscriptTarget, ref transcript.Transcript) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !target.Push {
		return errors.New("upload not enabled")
	}
	if err := ref.Validate(); err != nil {
		return errors.New("invalid transcript")
	}
	link, err := ref.Link(target.AgentsViewURL)
	base, parseErr := url.Parse(target.AgentsViewURL)
	if err != nil || parseErr != nil || base.Scheme != "https" || link != ref.URL {
		return errors.New("upload requires the recorded HTTPS target")
	}
	if target.TokenEnv == "" || strings.HasPrefix(strings.ToUpper(target.TokenEnv), "KATA_") {
		return errors.New("independent upload credential unavailable")
	}
	for _, name := range closeTranscriptPushEnv {
		if strings.EqualFold(name, target.TokenEnv) {
			return errors.New("independent upload credential unavailable")
		}
	}
	token, ok := os.LookupEnv(target.TokenEnv)
	if !ok || token == "" {
		return errors.New("selected upload credential unavailable")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := closeTranscriptPushCommand(ctx, "agentsview", "push", agentsViewPushSessionID(ref), "--to", target.AgentsViewURL, "--agent", ref.Agent, "--token-env", target.TokenEnv, "--json")
	for _, name := range closeTranscriptPushEnv {
		if value, ok := os.LookupEnv(name); ok {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Env = append(command.Env, target.TokenEnv+"="+token)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	tree, err := processtree.New(command)
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	command.Cancel = func() error { return tree.TerminateWithGrace(0) }
	if err := tree.Start(); err != nil {
		return err
	}
	err = tree.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
