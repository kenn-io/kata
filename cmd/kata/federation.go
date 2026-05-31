package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	hubclient "go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/textsafe"
)

func newFederationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "federation",
		Short: "manage federation operations",
	}
	cmd.AddCommand(
		federationIdentityCmd(),
		federationEnableCmd(),
		federationEnrollCmd(),
		federationEnrollmentsCmd(),
		federationJoinCmd(),
		federationRevokeCmd(),
		federationStatusCmd(),
		federationQuarantineCmd(),
		newFederationLeaseCmd(),
	)
	return cmd
}

func federationIdentityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "identity",
		Short: "show this daemon's federation identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/instance", nil)
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
			var body struct {
				InstanceUID   string `json:"instance_uid"`
				Version       string `json:"version"`
				SchemaVersion int64  `json:"schema_version"`
			}
			if err := json.Unmarshal(bs, &body); err != nil {
				return err
			}
			if mode == outputAgent {
				return writeAgentKVRow(cmd.OutOrStdout(),
					agentRowField("instance_uid", body.InstanceUID),
					agentRowField("version", body.Version),
					agentRowField("schema_version", strconv.FormatInt(body.SchemaVersion, 10)),
				)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "instance: %s\n", textsafe.Line(body.InstanceUID))
			return err
		},
	}
}

func federationEnableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable [project]",
		Short: "enable federation on a hub project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			project, err := resolveFederationProject(ctx, client, baseURL, args)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			metadata, err := enableAndReadFederationMetadata(ctx, client, baseURL, project.ID, actor)
			if err != nil {
				return err
			}
			return printFederationEnable(cmd, metadata)
		},
	}
	return cmd
}

func federationEnrollCmd() *cobra.Command {
	var spokeInstance string
	var hubURL string
	var capabilities string
	var token string
	cmd := &cobra.Command{
		Use:   "enroll [project]",
		Short: "create a hub enrollment for a spoke",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(spokeInstance) == "" {
				return &cliError{Message: "--spoke-instance is required", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if strings.TrimSpace(hubURL) == "" {
				return &cliError{Message: "--hub-url is required", Kind: kindValidation, ExitCode: ExitValidation}
			}
			internalCaps, externalCaps, err := normalizeFederationCapabilities(capabilities)
			if err != nil {
				return err
			}
			if err := validateFederationJoinCapabilities(internalCaps, federationCapabilitiesContain(internalCaps, "push")); err != nil {
				return err
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
			project, err := resolveFederationProject(ctx, client, baseURL, args)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			metadata, err := enableAndReadFederationMetadata(ctx, client, baseURL, project.ID, actor)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				baseURL+"/api/v1/federation/enrollments",
				map[string]any{
					"spoke_instance_uid": spokeInstance,
					"project_id":         project.ID,
					"capabilities":       internalCaps,
					"token":              token,
				})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			var enrollment api.FederationEnrollmentOut
			if err := json.Unmarshal(bs, &enrollment); err != nil {
				return err
			}
			bundle := federationJoinBundle{
				HubURL:                 strings.TrimRight(hubURL, "/"),
				HubProjectID:           metadata.ProjectID,
				HubProjectUID:          metadata.ProjectUID,
				ProjectName:            metadata.ProjectName,
				ReplayHorizonEventID:   metadata.ReplayHorizonEventID,
				BaselineThroughEventID: metadata.BaselineThroughEventID,
				Token:                  enrollment.Token,
				Capabilities:           internalCaps,
				DisplayCapabilities:    externalCaps,
				PushEnabled:            federationCapabilitiesContain(internalCaps, "push"),
			}
			return printFederationEnrollment(cmd, project.Name, spokeInstance, enrollment, bundle)
		},
	}
	cmd.Flags().StringVar(&spokeInstance, "spoke-instance", "", "spoke instance UID from `kata federation identity`")
	cmd.Flags().StringVar(&hubURL, "hub-url", "", "hub URL reachable by the spoke")
	cmd.Flags().StringVar(&capabilities, "capabilities", "pull,push,lease", "comma-separated capabilities: pull,push,lease")
	cmd.Flags().StringVar(&token, "token", "", "explicit enrollment token (default: generated)")
	return cmd
}

func federationEnrollmentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enrollments",
		Short: "audit hub federation enrollments",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "list hub federation enrollments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/federation/enrollments", nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return printFederationEnrollments(cmd, bs)
		},
	})
	return cmd
}

func federationRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <enrollment-id>",
		Short: "revoke a hub federation enrollment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return &cliError{Message: "enrollment-id must be a positive integer", Kind: kindValidation, ExitCode: ExitValidation}
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
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				fmt.Sprintf("%s/api/v1/federation/enrollments/%d/revoke", baseURL, id), map[string]any{})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return printFederationRevoke(cmd, bs)
		},
	}
}

func federationJoinCmd() *cobra.Command {
	var bundle federationJoinBundle
	cmd := &cobra.Command{
		Use:   "join",
		Short: "join a hub project as a spoke",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectName := strings.TrimSpace(flags.Project)
			if projectName == "" {
				return &cliError{Message: "--project is required", Kind: kindValidation, ExitCode: ExitValidation}
			}
			bundle.ProjectName = projectName
			if bundle.HubURL == "" || bundle.HubProjectID <= 0 || bundle.Token == "" {
				return &cliError{
					Message:  "--hub-url, --hub-project-id, --token, and --project are required",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			internalCaps, _, err := normalizeFederationCapabilities(bundle.DisplayCapabilities)
			if err != nil {
				return err
			}
			if bundle.AdoptExisting && !bundle.PushEnabled {
				return &cliError{
					Message:  "--adopt-existing requires --push",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			if err := validateFederationJoinCapabilities(internalCaps, bundle.PushEnabled); err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := hydrateFederationJoinMetadata(ctx, &bundle); err != nil {
				return err
			}
			if bundle.HubProjectUID == "" || bundle.ReplayHorizonEventID <= 0 {
				return &cliError{
					Message:  "hub metadata did not include hub_project_uid and replay_horizon_event_id",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				baseURL+"/api/v1/federation/replicas",
				map[string]any{
					"hub_url":                   strings.TrimRight(bundle.HubURL, "/"),
					"hub_project_id":            bundle.HubProjectID,
					"hub_project_uid":           bundle.HubProjectUID,
					"project_name":              bundle.ProjectName,
					"replay_horizon_event_id":   bundle.ReplayHorizonEventID,
					"baseline_through_event_id": bundle.BaselineThroughEventID,
					"token":                     bundle.Token,
					"capabilities":              internalCaps,
					"push_enabled":              bundle.PushEnabled,
					"adopt_existing":            bundle.AdoptExisting,
				})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if currentOutputMode() == outputHuman && federationCapabilitiesContain(internalCaps, "push") && !bundle.PushEnabled {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: push capability is present but local push is disabled; rerun join with --push to send spoke edits to the hub")
			}
			return printFederationJoin(cmd, bs)
		},
	}
	cmd.Flags().StringVar(&bundle.HubURL, "hub-url", "", "hub URL")
	cmd.Flags().Int64Var(&bundle.HubProjectID, "hub-project-id", 0, "hub project ID")
	cmd.Flags().StringVar(&bundle.HubProjectUID, "hub-project-uid", "", "hub project UID")
	cmd.Flags().Int64Var(&bundle.ReplayHorizonEventID, "replay-horizon", 0, "hub replay horizon event ID")
	cmd.Flags().Int64Var(&bundle.BaselineThroughEventID, "baseline-through", 0, "baseline-through event ID")
	cmd.Flags().StringVar(&bundle.Token, "token", "", "enrollment token")
	cmd.Flags().StringVar(&bundle.DisplayCapabilities, "capabilities", "pull,push,lease", "comma-separated capabilities: pull,push,lease")
	cmd.Flags().BoolVar(&bundle.PushEnabled, "push", false, "enable spoke push")
	cmd.Flags().BoolVar(&bundle.AdoptExisting, "adopt-existing", false, "adopt matching existing local data into the federation")
	return cmd
}

func federationStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show federation status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/federation/status", nil)
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
			var body api.FederationStatusBody
			if err := json.Unmarshal(bs, &body); err != nil {
				return err
			}
			if mode == outputAgent {
				return printFederationStatusAgent(cmd, body)
			}
			return printFederationStatus(cmd, body)
		},
	}
}

type federationJoinBundle struct {
	HubURL                 string `json:"hub_url"`
	HubProjectID           int64  `json:"hub_project_id"`
	HubProjectUID          string `json:"hub_project_uid"`
	ProjectName            string `json:"project_name"`
	ReplayHorizonEventID   int64  `json:"replay_horizon_event_id"`
	BaselineThroughEventID int64  `json:"baseline_through_event_id,omitempty"`
	Token                  string `json:"token"`
	Capabilities           string `json:"capabilities,omitempty"`
	DisplayCapabilities    string `json:"-"`
	PushEnabled            bool   `json:"push_enabled,omitempty"`
	AdoptExisting          bool   `json:"adopt_existing,omitempty"`
}

func hydrateFederationJoinMetadata(ctx context.Context, bundle *federationJoinBundle) error {
	if bundle.HubProjectUID != "" && bundle.ReplayHorizonEventID > 0 {
		return nil
	}
	client, err := hubclient.NewClient(ctx, bundle.HubURL, bundle.Token,
		clientpkg.Opts{Timeout: envHTTPTimeout(defaultHTTPTimeout)})
	if err != nil {
		return err
	}
	metadata, err := client.ProjectFederation(ctx, bundle.HubProjectID)
	if err != nil {
		return err
	}
	if bundle.HubProjectUID == "" {
		bundle.HubProjectUID = metadata.ProjectUID
	}
	if bundle.ReplayHorizonEventID <= 0 {
		bundle.ReplayHorizonEventID = metadata.ReplayHorizonEventID
	}
	if bundle.BaselineThroughEventID <= 0 {
		bundle.BaselineThroughEventID = metadata.BaselineThroughEventID
	}
	return nil
}

func resolveFederationProject(ctx context.Context, client *http.Client, baseURL string, args []string) (projectRef, error) {
	if len(args) > 0 {
		return resolveProjectSelector(ctx, client, baseURL, args[0])
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return projectRef{}, err
	}
	id, name, err := resolveProjectIDAndName(ctx, baseURL, start)
	if err != nil {
		return projectRef{}, err
	}
	return projectRef{ID: id, Name: name}, nil
}

func enableAndReadFederationMetadata(ctx context.Context, client *http.Client, baseURL string, projectID int64, actor string) (api.ProjectFederationBody, error) {
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/federation/enable", baseURL, projectID),
		map[string]string{"actor": actor})
	if err != nil {
		return api.ProjectFederationBody{}, err
	}
	if status >= 400 {
		return api.ProjectFederationBody{}, apiErrFromBody(status, bs)
	}
	var metadata api.ProjectFederationBody
	if err := json.Unmarshal(bs, &metadata); err != nil {
		return api.ProjectFederationBody{}, err
	}
	return metadata, nil
}

func printFederationEnable(cmd *cobra.Command, metadata api.ProjectFederationBody) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), metadata)
	}
	if currentOutputMode() == outputAgent {
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("project", metadata.ProjectName),
			agentRowField("project_id", strconv.FormatInt(metadata.ProjectID, 10)),
			agentRowField("replay_horizon", strconv.FormatInt(metadata.ReplayHorizonEventID, 10)),
			agentRowField("baseline_through", strconv.FormatInt(metadata.BaselineThroughEventID, 10)),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "enabled federation for %s\n", textsafe.Line(metadata.ProjectName))
	return err
}

func printFederationEnrollment(
	cmd *cobra.Command,
	projectName string,
	spokeInstance string,
	enrollment api.FederationEnrollmentOut,
	bundle federationJoinBundle,
) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			Enrollment api.FederationEnrollmentOut `json:"enrollment"`
			Join       federationJoinBundle        `json:"join"`
		}{Enrollment: enrollment, Join: bundle})
	}
	if currentOutputMode() == outputAgent {
		if err := writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("project", projectName),
			agentRowField("spoke_instance", spokeInstance),
			agentRowField("enrollment_id", strconv.FormatInt(enrollment.ID, 10)),
		); err != nil {
			return err
		}
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("join_command", federationJoinCommand(bundle)))
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "enrolled %s for %s\njoin: %s\n",
		textsafe.Line(spokeInstance), textsafe.Line(projectName), federationJoinCommand(bundle))
	return err
}

func printFederationEnrollments(cmd *cobra.Command, bs []byte) error {
	if currentOutputMode() == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var body api.ListFederationEnrollmentsBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		for _, enrollment := range body.Enrollments {
			state := "active"
			if enrollment.RevokedAt != nil {
				state = "revoked"
			}
			project := "*"
			if enrollment.ProjectID != nil {
				project = strconv.FormatInt(*enrollment.ProjectID, 10)
			}
			_, displayCaps, err := normalizeFederationCapabilities(enrollment.Capabilities)
			if err != nil {
				return err
			}
			if err := writeAgentKVRow(cmd.OutOrStdout(),
				agentRowField("id", strconv.FormatInt(enrollment.ID, 10)),
				agentRowField("spoke_instance", enrollment.SpokeInstanceUID),
				agentRowField("project", project),
				agentRowField("capabilities", displayCaps),
				agentRowField("state", state),
			); err != nil {
				return err
			}
		}
		return nil
	}
	if flags.Quiet {
		return nil
	}
	for _, enrollment := range body.Enrollments {
		state := "active"
		if enrollment.RevokedAt != nil {
			state = "revoked"
		}
		project := "*"
		if enrollment.ProjectID != nil {
			project = strconv.FormatInt(*enrollment.ProjectID, 10)
		}
		_, displayCaps, err := normalizeFederationCapabilities(enrollment.Capabilities)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "#%d %s project: %s capabilities: %s %s\n",
			enrollment.ID, textsafe.Line(enrollment.SpokeInstanceUID), project, displayCaps, state); err != nil {
			return err
		}
	}
	return nil
}

func printFederationRevoke(cmd *cobra.Command, bs []byte) error {
	if currentOutputMode() == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var body api.RevokeFederationEnrollmentBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("id", strconv.FormatInt(body.ID, 10)),
			agentRowField("revoked", strconv.FormatBool(body.Revoked)),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "revoked federation enrollment #%d\n", body.ID)
	return err
}

func printFederationJoin(cmd *cobra.Command, bs []byte) error {
	if currentOutputMode() == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var body api.CreateFederationReplicaBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("project", body.Project.Name),
			agentRowField("project_id", strconv.FormatInt(body.Project.ID, 10)),
			agentRowField("push_enabled", strconv.FormatBool(body.Binding.PushEnabled)),
			agentRowField("adopted", strconv.FormatBool(body.Adopted)),
			agentRowField("adoption_snapshots", strconv.FormatInt(body.AdoptionSnapshotCount, 10)),
		)
	}
	if flags.Quiet {
		return nil
	}
	if body.Adopted {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"adopted existing project %s into federation\nqueued %d issue snapshots for hub push; pre-adoption local event history was removed\nfuture edits remain local-first; acquire leases only for exclusive coordination\n",
			textsafe.Line(body.Project.Name), body.AdoptionSnapshotCount)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "joined federation project %s (push-enabled: %t)\n",
		textsafe.Line(body.Project.Name), body.Binding.PushEnabled)
	return err
}

func normalizeFederationCapabilities(raw string) (internalCaps, displayCaps string, err error) {
	if strings.TrimSpace(raw) == "" {
		raw = "pull,push,lease"
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		capability := strings.TrimSpace(part)
		switch capability {
		case "lease":
			capability = "claim"
		case "claim", "pull", "push":
		default:
			return "", "", &cliError{
				Message:  fmt.Sprintf("unknown federation capability %q", strings.TrimSpace(part)),
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
		seen[capability] = true
	}
	order := []string{"claim", "pull", "push"}
	var internal []string
	var display []string
	for _, capability := range order {
		if !seen[capability] {
			continue
		}
		internal = append(internal, capability)
		if capability == "claim" {
			display = append(display, "lease")
		} else {
			display = append(display, capability)
		}
	}
	if len(internal) == 0 {
		return "", "", &cliError{
			Message:  "at least one federation capability is required",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return strings.Join(internal, ","), strings.Join(display, ","), nil
}

func federationCapabilitiesContain(capabilities, want string) bool {
	for _, part := range strings.Split(capabilities, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func validateFederationJoinCapabilities(capabilities string, pushEnabled bool) error {
	if !federationCapabilitiesContain(capabilities, "pull") {
		return &cliError{
			Message:  "federation join requires pull capability",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if pushEnabled && !federationCapabilitiesContain(capabilities, "push") {
		return &cliError{
			Message:  "federation join --push requires push capability",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return nil
}

func federationJoinCommand(bundle federationJoinBundle) string {
	args := []string{
		invokedKataCommand(), "federation", "join",
		"--project", bundle.ProjectName,
		"--hub-url", bundle.HubURL,
		"--hub-project-id", strconv.FormatInt(bundle.HubProjectID, 10),
		"--token", bundle.Token,
		"--capabilities", bundle.DisplayCapabilities,
	}
	if bundle.PushEnabled {
		args = append(args, "--push")
	}
	if bundle.AdoptExisting {
		args = append(args, "--adopt-existing")
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func invokedKataCommand() string {
	if len(os.Args) == 0 || strings.TrimSpace(os.Args[0]) == "" {
		return "kata"
	}
	return filepath.Base(os.Args[0])
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("-_./:=,", r) {
			continue
		}
		safe = false
		break
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func federationQuarantineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quarantine",
		Short: "manage federation quarantines",
	}
	cmd.AddCommand(federationQuarantineSkipCmd())
	return cmd
}

func federationQuarantineSkipCmd() *cobra.Command {
	var confirm string
	var reason string
	cmd := &cobra.Command{
		Use:   "skip <id>",
		Short: "skip a quarantined federation batch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return &cliError{
					Message:  "quarantine id must be a positive integer",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			expected := fmt.Sprintf("SKIP FEDERATION BATCH %d", id)
			confirm, err := resolveConfirm(cmd, confirm, expected,
				fmt.Sprintf("Type %q to skip this federation batch: ", expected), confirmPromptFull)
			if err != nil {
				return err
			}
			return runFederationQuarantineSkip(cmd.Context(), cmd, id, confirm, reason)
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "exact confirmation string")
	cmd.Flags().StringVar(&reason, "reason", "", "skip reason")
	return cmd
}

func runFederationQuarantineSkip(ctx context.Context, cmd *cobra.Command, id int64, confirm, reason string) error {
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/federation/status", nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	var body api.FederationStatusBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	projectID, err := federationProjectForQuarantine(body, id)
	if err != nil {
		return err
	}
	actor, _ := resolveActor(flags.As, nil)
	status, bs, err = httpDoJSONWithHeader(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/federation/quarantine/%d/skip", baseURL, projectID, id),
		map[string]string{"X-Kata-Confirm": confirm},
		map[string]any{"actor": actor, "reason": reason})
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
	if flags.Quiet {
		return nil
	}
	if mode == outputAgent {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK federation-quarantine-skip id=%d\n", id)
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "quarantine #%d skipped\n", id)
	return err
}

func federationProjectForQuarantine(body api.FederationStatusBody, id int64) (int64, error) {
	for _, status := range body.Statuses {
		for _, quarantine := range status.ActiveQuarantines {
			if quarantine.ID == id {
				return status.ProjectID, nil
			}
		}
	}
	return 0, &cliError{
		Message:  fmt.Sprintf("federation quarantine %d not found", id),
		Code:     "federation_quarantine_not_found",
		Kind:     kindNotFound,
		ExitCode: ExitNotFound,
	}
}

func printFederationStatus(cmd *cobra.Command, body api.FederationStatusBody) error {
	out := cmd.OutOrStdout()
	if len(body.Statuses) == 0 {
		_, err := fmt.Fprintln(out, "no federation bindings")
		return err
	}
	for i, status := range body.Statuses {
		if i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(out, "%s\n", textsafe.Line(status.ProjectName)); err != nil {
			return err
		}
		lines := []string{
			fmt.Sprintf("role: %s", textsafe.Line(status.Role)),
			fmt.Sprintf("enabled: %t", status.Enabled),
			fmt.Sprintf("push-enabled: %t", status.PushEnabled),
			fmt.Sprintf("pull cursor: %d", status.PullCursorEventID),
			fmt.Sprintf("push cursor: %d", status.PushCursorEventID),
			fmt.Sprintf("pending push: %d", status.PendingPushCount),
			fmt.Sprintf("last successful sync: %s", formatFederationStatusTime(status.LastSuccessfulSyncAt)),
			fmt.Sprintf("last error: %s", formatFederationStatusError(status.LastErrorAt, status.LastError)),
			fmt.Sprintf("live leases: %d", status.LiveClaimCount),
			fmt.Sprintf("pending leases: %d", status.PendingClaimCount),
			fmt.Sprintf("enrollments: %d", status.EnrollmentCount),
			fmt.Sprintf("active quarantine: %d", status.ActiveQuarantineCount),
			fmt.Sprintf("reset blocker: %s", formatFederationResetBlocker(status.ResetBlocker)),
			fmt.Sprintf("unresolved violations: %d", status.UnresolvedViolationCount),
			fmt.Sprintf("recent violations: %d", status.RecentViolationCount),
		}
		for _, line := range lines {
			if _, err := fmt.Fprintf(out, "  %s\n", line); err != nil {
				return err
			}
		}
		for _, violation := range status.RecentViolations {
			if _, err := fmt.Fprintf(out, "  recent violation: %s %s by %s on spoke %s at %s (%s)\n",
				textsafe.Line(violation.ShortID),
				textsafe.Line(violation.OffendingEventType),
				textsafe.Line(violation.Actor),
				textsafe.Line(violation.OffendingOriginInstanceUID),
				violation.At.UTC().Format(time.RFC3339),
				textsafe.Line(violation.Reason)); err != nil {
				return err
			}
		}
		for _, quarantine := range status.ActiveQuarantines {
			if _, err := fmt.Fprintf(out, "  quarantine #%d: %s events %d-%d at %s (%s)\n",
				quarantine.ID,
				textsafe.Line(quarantine.Direction),
				quarantine.FirstEventID,
				quarantine.LastEventID,
				quarantine.CreatedAt.UTC().Format(time.RFC3339),
				textsafe.Line(quarantine.Error)); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatFederationStatusTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatFederationStatusError(at *time.Time, msg *string) string {
	if msg == nil || *msg == "" {
		return "none"
	}
	if at == nil {
		return textsafe.Line(*msg)
	}
	return at.UTC().Format(time.RFC3339) + " " + textsafe.Line(*msg)
}

func formatFederationResetBlocker(blocker string) string {
	if blocker == "" {
		return "none"
	}
	return textsafe.Line(blocker)
}

func printFederationStatusAgent(cmd *cobra.Command, body api.FederationStatusBody) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "OK federation-status count=%d\n", len(body.Statuses)); err != nil {
		return err
	}
	for _, status := range body.Statuses {
		if err := writeAgentKVRow(out,
			agentRowField("project", status.ProjectName),
			agentRowField("role", status.Role),
			agentRowField("enabled", strconv.FormatBool(status.Enabled)),
			agentRowField("push_enabled", strconv.FormatBool(status.PushEnabled)),
			agentRowField("pull_cursor", strconv.FormatInt(status.PullCursorEventID, 10)),
			agentRowField("push_cursor", strconv.FormatInt(status.PushCursorEventID, 10)),
			agentRowField("pending_push", strconv.FormatInt(status.PendingPushCount, 10)),
			agentRowField("last_sync", formatFederationStatusTime(status.LastSuccessfulSyncAt)),
			agentRowField("last_error", formatFederationStatusError(status.LastErrorAt, status.LastError)),
			agentRowField("live_leases", strconv.FormatInt(status.LiveClaimCount, 10)),
			agentRowField("pending_leases", strconv.FormatInt(status.PendingClaimCount, 10)),
			agentRowField("enrollments", strconv.FormatInt(status.EnrollmentCount, 10)),
			agentRowField("active_quarantine", strconv.FormatInt(status.ActiveQuarantineCount, 10)),
			agentRowField("reset_blocker", formatFederationResetBlocker(status.ResetBlocker)),
			agentRowField("unresolved_violations", strconv.FormatInt(status.UnresolvedViolationCount, 10)),
			agentRowField("recent_violations", strconv.FormatInt(status.RecentViolationCount, 10)),
		); err != nil {
			return err
		}
		for _, violation := range status.RecentViolations {
			if err := writeAgentKVRow(out,
				agentRowField("violation_issue", violation.ShortID),
				agentRowField("event", violation.OffendingEventType),
				agentRowField("actor", violation.Actor),
				agentRowField("offending_instance", violation.OffendingOriginInstanceUID),
				agentRowField("at", violation.At.UTC().Format(time.RFC3339)),
				agentRowField("reason", violation.Reason),
			); err != nil {
				return err
			}
		}
		for _, quarantine := range status.ActiveQuarantines {
			if err := writeAgentKVRow(out,
				agentRowField("quarantine_id", strconv.FormatInt(quarantine.ID, 10)),
				agentRowField("direction", quarantine.Direction),
				agentRowField("first_event", strconv.FormatInt(quarantine.FirstEventID, 10)),
				agentRowField("last_event", strconv.FormatInt(quarantine.LastEventID, 10)),
				agentRowField("created_at", quarantine.CreatedAt.UTC().Format(time.RFC3339)),
				agentRowField("error", quarantine.Error),
			); err != nil {
				return err
			}
		}
	}
	return nil
}
