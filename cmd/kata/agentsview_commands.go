package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
)

func newAgentsViewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agentsview", Short: "prepare and explicitly share a frozen closing-session snapshot"}
	cmd.AddCommand(newAgentsViewPrepareCmd(), newAgentsViewShareCmd())
	// The target inspection child is owned by the discovery prerequisite. Combine
	// constructors when that branch lands; never duplicate its implementation.
	return cmd
}

func newAgentsViewPrepareCmd() *cobra.Command {
	var snapshot, output string
	cmd := &cobra.Command{
		Use: "prepare <issue-ref>", Short: "prepare project snapshot consent without uploading", Args: cobra.ExactArgs(1),
		Long: "Prepare inert local consent for an archived snapshot through its captured point. Later running-session messages may be absent. Review the exact file before confirming its digest. The receiver's first accepted snapshot is immutable, including across later closes in the same session.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if snapshot == "" || output == "" {
				return externalCLIValidation("AgentsView: --snapshot and --output are required")
			}
			native, err := captureAgentsViewNative(cmd.Context())
			if err != nil {
				return err
			}
			workflow, err := newLiveAgentsViewWorkflow(cmd.Context())
			if err != nil {
				return err
			}
			consent, err := workflow.Prepare(cmd.Context(), args[0], snapshot, native)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(consent)
			if err != nil {
				return err
			}
			if err = writeAgentsViewConsent(cmd.Context(), output, raw); err != nil {
				return err
			}
			return printAgentsViewStatus(cmd, "prepared", consent, "")
		},
	}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "explicit exported snapshot path, relative to the invoking directory")
	cmd.Flags().StringVar(&output, "output", "", "new private local consent file")
	return cmd
}

func newAgentsViewShareCmd() *cobra.Command {
	var snapshot, event, digest, issue string
	cmd := &cobra.Command{
		Use: "share <consent>", Short: "explicitly retry the same frozen snapshot for its original close", Args: cobra.ExactArgs(1),
		Long: "Retry only the retained reviewed bytes and original associated close. This never closes again, scans an archive, re-exports, changes destination, or queues a retry. An ordinary close without a snapshot association cannot acquire one later.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if snapshot == "" || event == "" || digest == "" || issue == "" {
				return externalCLIValidation("AgentsView: --issue, --snapshot, --close-event and --confirm-transcript-sha256 are required")
			}
			consent, err := readAgentsViewConsent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			workflow, err := newLiveAgentsViewWorkflow(cmd.Context())
			if err != nil {
				return err
			}
			if err = workflow.Share(cmd.Context(), issue, consent, snapshot, event, digest); err != nil {
				return err
			}
			return printAgentsViewStatus(cmd, "accepted", consent, event)
		},
	}
	cmd.Flags().StringVar(&issue, "issue", "", "original issue reference, resolved through the selected daemon")
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "explicit retained snapshot path, relative to the invoking directory")
	cmd.Flags().StringVar(&event, "close-event", "", "exact original associated close event UID")
	cmd.Flags().StringVar(&digest, "confirm-transcript-sha256", "", "SHA-256 of the reviewed frozen bytes; required on every attempt")
	return cmd
}

func readAgentsViewConsent(ctx context.Context, path string) (agentsViewConsent, error) {
	file, err := openAgentsViewSelectedFile(ctx, path)
	if err != nil {
		return agentsViewConsent{}, errors.New("AgentsView: cannot open retained consent")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return agentsViewConsent{}, errors.New("AgentsView: consent must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(agentsViewContextReader{ctx, file}, agentsViewRecordLimit+1))
	if err != nil {
		return agentsViewConsent{}, errors.New("AgentsView: cannot read retained consent")
	}
	return decodeAgentsViewConsent(raw)
}

func captureAgentsViewNative(ctx context.Context) (agentsViewNativeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return agentsViewNativeIdentity{}, err
	}
	cfg, err := config.ReadCloseTranscriptConfig()
	if err != nil {
		return agentsViewNativeIdentity{}, errors.New("AgentsView: native-session configuration unavailable")
	}
	native := agentsViewNativeIdentity{os.Getenv("KATA_TRANSCRIPT_AGENT"), os.Getenv("KATA_TRANSCRIPT_SESSION_ID")}
	if native.Agent == "" && native.NativeSessionID == "" {
		variables := map[string]string{"CLAUDE_CODE_SESSION_ID": "claude", "CODEX_THREAD_ID": "codex", "CODEX_SESSION_ID": "codex"}
		// Keep aliases as separate observations, including a configured variable
		// whose name collides with a built-in. Neither may conceal ambiguity.
		observe := func(agent, name string) error {
			value := os.Getenv(name)
			if value == "" {
				return nil
			}
			next := agentsViewNativeIdentity{agent, value}
			if native.Agent != "" && native != next {
				return errors.New("AgentsView: ambiguous current native session; set KATA_TRANSCRIPT_AGENT and KATA_TRANSCRIPT_SESSION_ID explicitly")
			}
			native = next
			return nil
		}
		for name, agent := range variables {
			if err := observe(agent, name); err != nil {
				return native, err
			}
		}
		for agent, name := range cfg.SessionIDEnv {
			if err := observe(agent, name); err != nil {
				return native, err
			}
		}
	}
	_, err = native.canonical()
	return native, err
}

func printAgentsViewStatus(cmd *cobra.Command, state string, c agentsViewConsent, event string) error {
	summary := struct {
		State        string `json:"state"`
		IssueUID     string `json:"issue_uid"`
		ProjectUID   string `json:"project_uid"`
		AuthorityUID string `json:"authority_uid"`
		ServiceID    string `json:"service_id"`
		Digest       string `json:"snapshot_sha256"`
		Bytes        int64  `json:"snapshot_bytes"`
		Event        string `json:"close_event_uid,omitempty"`
	}{state, c.Association.IssueUID, c.Association.ProjectUID, c.Association.AuthorityUID, c.Association.ServiceID, c.Association.SnapshotSHA256, c.Association.SnapshotBytes, event}
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), summary)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "AgentsView %s issue=%s project=%s authority=%s service=%s snapshot_sha256=%s snapshot_bytes=%d close_event_uid=%s\n", state, summary.IssueUID, summary.ProjectUID, summary.AuthorityUID, summary.ServiceID, summary.Digest, summary.Bytes, event)
	return err
}

type agentsViewCloseFlags struct{ consent, snapshot, digest string }

func (f *agentsViewCloseFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.consent, "share-transcript", "", "retained consent file for explicit project relay sharing")
	cmd.Flags().StringVar(&f.snapshot, "snapshot", "", "explicit retained snapshot path; never resolved relative to consent")
	cmd.Flags().StringVar(&f.digest, "confirm-transcript-sha256", "", "confirm reviewed frozen snapshot SHA-256 for this attempt")
}

func (f agentsViewCloseFlags) selected(cmd *cobra.Command) (bool, error) {
	selected := cmd.Flags().Changed("share-transcript") || cmd.Flags().Changed("snapshot") || cmd.Flags().Changed("confirm-transcript-sha256")
	if selected && (f.consent == "" || f.snapshot == "" || !validAgentsViewDigest(f.digest)) {
		return true, externalCLIValidation("AgentsView: sharing requires --share-transcript, --snapshot and --confirm-transcript-sha256")
	}
	if selected && cmd.Flags().Lookup("no-transcript") != nil {
		disabled, err := cmd.Flags().GetBool("no-transcript")
		if err != nil || disabled {
			return true, externalCLIValidation("AgentsView: transcript disable conflicts with explicit sharing")
		}
	}
	return selected, nil
}

func (f agentsViewCloseFlags) run(ctx context.Context, cmd *cobra.Command, ref string, body map[string]any, headers map[string]string, dry bool) error {
	comment, _, err := prepareFollowupComment(cmd)
	if err != nil {
		return err
	}
	if comment != "" {
		return externalCLIValidation("AgentsView: --comment with sharing is unavailable in this draft; post the comment separately")
	}
	consent, err := readAgentsViewConsent(ctx, f.consent)
	if err != nil {
		return err
	}
	native, err := captureAgentsViewNative(ctx)
	if err != nil {
		return err
	}
	workflow, err := newLiveAgentsViewWorkflow(ctx)
	if err != nil {
		return err
	}
	result, err := workflow.Close(ctx, ref, consent, f.snapshot, f.digest, native, dry, body, headers)
	// Keep the ordinary close response on stdout even if upload is unconfirmed.
	// Transcript content and companion output never enter command output.
	if len(result.Raw) > 0 {
		if printErr := printMutationWithApplied(cmd, result.Raw, nil, ""); printErr != nil {
			return printErr
		}
	}
	return err
}
