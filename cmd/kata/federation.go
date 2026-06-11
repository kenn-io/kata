package main

import (
	"bufio"
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
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
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
		federationLeaveCmd(),
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
			project, err := resolveFederationProject(ctx, client, baseURL, args, false)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
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
	var actor string
	var allowInsecure bool
	var adoptExistingFlag bool
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
			pushCapable := federationCapabilitiesContain(internalCaps, "push")
			if adoptExistingFlag && !pushCapable {
				return &cliError{Message: "--adopt-existing requires push capability", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if err := validateFederationJoinCapabilities(internalCaps, pushCapable); err != nil {
				return err
			}
			ctx := cmd.Context()
			hubBaseURL := strings.TrimRight(hubURL, "/")
			hubClient, err := federationEnrollHTTPClient(ctx, hubBaseURL, allowInsecure)
			if err != nil {
				return federationEnrollHTTPClientError(err)
			}
			project, err := resolveFederationProject(ctx, hubClient, hubBaseURL, args, true)
			if err != nil {
				return err
			}
			requestActor := strings.TrimSpace(actor)
			if requestActor == "" {
				requestActor, _ = resolveActor(ctx, flags.As, nil)
			}
			metadata, err := enableAndReadFederationMetadata(ctx, hubClient, hubBaseURL, project.ID, requestActor)
			if err != nil {
				return err
			}
			adoptExisting := adoptExistingFlag
			if !adoptExisting && pushCapable {
				if spokeUID, ok := federationSpokeProjectUID(ctx, metadata.ProjectName, spokeInstance); ok {
					// A same-name spoke project is normally adopted — unless it
					// already shares the hub project's UID (it previously left
					// this federation). Then the join is a plain rejoin and
					// adoption would needlessly rewrite its event history.
					adoptExisting = spokeUID == "" || spokeUID != metadata.ProjectUID
				}
			}
			status, bs, err := httpDoJSON(ctx, hubClient, http.MethodPost,
				hubBaseURL+"/api/v1/federation/enrollments",
				map[string]any{
					"spoke_instance_uid":              spokeInstance,
					"project_id":                      project.ID,
					"capabilities":                    internalCaps,
					"token":                           token,
					"actor":                           requestActor,
					"allow_adoption_snapshot_authors": adoptExisting,
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
				HubURL:                 hubBaseURL,
				HubProjectID:           metadata.ProjectID,
				HubProjectUID:          metadata.ProjectUID,
				ProjectName:            metadata.ProjectName,
				ReplayHorizonEventID:   metadata.ReplayHorizonEventID,
				BaselineThroughEventID: metadata.BaselineThroughEventID,
				Token:                  enrollment.Token,
				Capabilities:           internalCaps,
				DisplayCapabilities:    externalCaps,
				Actor:                  enrollment.Actor,
				PushEnabled:            pushCapable,
				AllowInsecure:          allowInsecure,
			}
			bundle.AdoptExisting = adoptExisting
			return printFederationEnrollment(cmd, project.Name, spokeInstance, enrollment, bundle)
		},
	}
	cmd.Flags().StringVar(&spokeInstance, "spoke-instance", "", "spoke instance UID from `kata federation identity`")
	cmd.Flags().StringVar(&hubURL, "hub-url", "", "hub URL reachable by the spoke")
	cmd.Flags().StringVar(&capabilities, "capabilities", "pull,push,lease", "comma-separated capabilities: pull,push,lease")
	cmd.Flags().StringVar(&token, "token", "", "explicit enrollment token (default: generated)")
	cmd.Flags().StringVar(&actor, "actor", "", "actor bound to this spoke enrollment")
	cmd.Flags().BoolVar(&allowInsecure, "allow-insecure", false, "allow plaintext HTTP hub URL for enrollment and later spoke transport")
	cmd.Flags().BoolVar(&adoptExistingFlag, "adopt-existing", false, "mark enrollment for adopting an existing spoke project")
	return cmd
}

func federationEnrollHTTPClient(ctx context.Context, hubBaseURL string, allowInsecure bool) (*http.Client, error) {
	return clientpkg.NewHTTPClient(ctx, hubBaseURL, clientpkg.Opts{
		Timeout:       envHTTPTimeout(defaultHTTPTimeout),
		AllowInsecure: allowInsecure,
	})
}

func federationEnrollHTTPClientError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "refusing to attach bearer token to plaintext non-loopback URL") {
		return fmt.Errorf("%w; for a trusted private/plain HTTP federation hub, rerun `kata federation enroll` with --allow-insecure", err)
	}
	return err
}

func federationSpokeProjectExists(ctx context.Context, projectName, spokeInstance string) bool {
	_, ok := federationSpokeProjectUID(ctx, projectName, spokeInstance)
	return ok
}

// federationSpokeProjectUID reports whether the default/spoke daemon (when its
// instance matches spokeInstance) has a project named projectName, returning
// that project's UID. The UID is "" when the daemon's list payload predates
// uid, in which case callers should fall back to the adoption default.
func federationSpokeProjectUID(ctx context.Context, projectName, spokeInstance string) (string, bool) {
	spokeURL, err := ensureDaemon(ctx)
	if err != nil {
		return "", false
	}
	spokeClient, err := clientpkg.NewHTTPClientForTarget(ctx, spokeURL, clientpkg.TargetAuth{}, clientpkg.Opts{
		Timeout: envHTTPTimeout(defaultHTTPTimeout),
	})
	if err != nil {
		return "", false
	}
	if strings.TrimSpace(spokeInstance) != "" {
		uid, err := federationSpokeInstanceUID(ctx, spokeClient, spokeURL)
		if err != nil || uid != strings.TrimSpace(spokeInstance) {
			return "", false
		}
	}
	return federationSpokeProjectNameExists(ctx, spokeClient, spokeURL, projectName)
}

func federationSpokeInstanceUID(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/instance", nil)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", apiErrFromBody(status, bs)
	}
	var body struct {
		InstanceUID string `json:"instance_uid"`
	}
	if err := json.Unmarshal(bs, &body); err != nil {
		return "", err
	}
	return strings.TrimSpace(body.InstanceUID), nil
}

func federationSpokeProjectNameExists(ctx context.Context, client *http.Client, baseURL, projectName string) (string, bool) {
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return "", false
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/projects", nil)
	if err != nil || status >= 400 {
		return "", false
	}
	var body struct {
		Projects []struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(bs, &body); err != nil {
		return "", false
	}
	for _, project := range body.Projects {
		if project.Name == projectName {
			return project.UID, true
		}
	}
	return "", false
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
			if strings.TrimSpace(bundle.Actor) == "" {
				return &cliError{Message: "--actor is required", Kind: kindValidation, ExitCode: ExitValidation}
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
					"actor":                     strings.TrimSpace(bundle.Actor),
					"allow_insecure":            bundle.AllowInsecure,
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
	cmd.Flags().StringVar(&bundle.Actor, "actor", "", "actor bound to this spoke")
	cmd.Flags().BoolVar(&bundle.AllowInsecure, "allow-insecure", false, "allow plaintext HTTP hub hostnames for private overlay networks")
	cmd.Flags().BoolVar(&bundle.PushEnabled, "push", false, "enable spoke push")
	cmd.Flags().BoolVar(&bundle.AdoptExisting, "adopt-existing", false, "adopt matching existing local data into the federation")
	return cmd
}

// resolveLeaveProject resolves the leave target like resolveFederationProject
// but includes archived projects for the explicit argument and --project
// forms: an archive-leave retry whose archive already committed must reach
// the daemon's idempotent resume, and active-only resolution would report
// the project as not found while detach/credential cleanup is pending. A
// surviving binding on the archived project is surfaced via the
// include=archived status fetch in resolveSpokeForLeave, so such a retry
// runs the normal bound path (idempotent hub revoke + teardown). The cwd
// workspace form keeps active-only resolution: its alias surface treats
// archived projects as gone.
func resolveLeaveProject(ctx context.Context, client *http.Client, baseURL string, args []string) (projectRef, error) {
	if len(args) > 0 {
		return resolveProjectSelectorIncludingArchived(ctx, client, baseURL, args[0])
	}
	if projectName := strings.TrimSpace(flags.Project); projectName != "" {
		return resolveProjectSelectorIncludingArchived(ctx, client, baseURL, projectName)
	}
	return resolveFederationProject(ctx, client, baseURL, args, false)
}

// spokeLeaveTarget captures the resolved local spoke a leave will tear down.
// When standalone is true, the project has no federation binding; leave skips
// hub revoke and confirmation for plain detach (the daemon call still runs to
// clean up a stale credential) and proceeds to archive-only for --delete.
// hubURL, hubProjectID, and instanceUID are only valid when standalone is
// false.
type spokeLeaveTarget struct {
	projectID     int64
	projectName   string
	hubURL        string
	hubProjectID  int64
	instanceUID   string
	allowInsecure bool
	standalone    bool
}

func federationLeaveCmd() *cobra.Command {
	var (
		deleteFlag    bool
		force         bool
		localOnly     bool
		hubName       string
		hubToken      string
		allowInsecure bool
		yes           bool
	)
	cmd := &cobra.Command{
		Use:   "leave [project]",
		Short: "leave a hub federation as a spoke (revoke + local teardown)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if force && !deleteFlag {
				return &cliError{Message: "--force requires --delete", Kind: kindValidation, ExitCode: ExitValidation}
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
			target, err := resolveSpokeForLeave(ctx, client, baseURL, args)
			if err != nil {
				return err
			}
			disposition := "detach"
			if deleteFlag {
				disposition = "archive"
			}
			// Daemon preflight BEFORE the irreversible hub revoke, for every
			// leave that will contact the hub: the route can refuse a detach
			// too (role drift, vanished project, actor validation), and the
			// archive disposition adds the open-issue refusal. A refusal
			// discovered only after the revoke would strand the spoke locally
			// bound with the hub side gone. Advisory only — the authoritative
			// checks stay inside the daemon's transactions.
			if !target.standalone && !localOnly {
				actor, _ := resolveActor(ctx, flags.As, nil)
				status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
					fmt.Sprintf("%s/api/v1/federation/replicas/%d/actions/leave", baseURL, target.projectID),
					map[string]any{"disposition": disposition, "force": force, "actor": actor, "preflight": true})
				if err != nil {
					return err
				}
				if status >= 400 {
					return apiErrFromBody(status, bs)
				}
			}
			// Standalone path: project has no federation binding, so there is no
			// hub contact either way. Plain leave skips the confirmation (nothing
			// is detached or archived) but must NOT skip the daemon leave call:
			// the route is the idempotent resume that deletes a stale hub
			// credential left by a partial leave (binding gone, credential delete
			// failed). --delete gates the archive on the same confirmation as the
			// bound path (no hub revoke note, since there is no hub contact here).
			if target.standalone {
				if deleteFlag {
					if err := confirmFederationLeave(cmd, target, "archive", true, yes); err != nil {
						return err
					}
				}
			} else if err := confirmFederationLeave(cmd, target, disposition, localOnly, yes); err != nil {
				return err
			}
			if !target.standalone {
				if localOnly {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: --local-only skips hub revoke; the enrollment token remains valid until you run `kata federation revoke <id>` on the hub %s\n",
						textsafe.Line(target.hubURL))
				} else {
					globals, err := revokeSpokeEnrollmentsOnHub(ctx, target, hubAuthInputs{
						hubURL:   target.hubURL,
						hubName:  hubName,
						hubToken: hubToken,
						// Union of opt-ins: the binding/status flag (which can be
						// lost with the credential during a partial-leave
						// recovery) and the explicit leave-time flag.
						allowInsecure: target.allowInsecure || allowInsecure,
					})
					if err != nil {
						return err
					}
					if len(globals) > 0 {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
							"warning: global enrollment(s) %s for this spoke remain active on the hub and still authorize this project; revoke with `kata federation revoke <id>` if intended\n",
							formatEnrollmentIDList(globals))
					}
				}
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				fmt.Sprintf("%s/api/v1/federation/replicas/%d/actions/leave", baseURL, target.projectID),
				map[string]any{"disposition": disposition, "force": force, "actor": actor})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return printFederationLeave(cmd, bs)
		},
	}
	cmd.Flags().BoolVar(&deleteFlag, "delete", false, "archive the local replica after detaching (reversible via kata projects restore)")
	cmd.Flags().BoolVar(&force, "force", false, "with --delete, override the open-issue refusal")
	cmd.Flags().BoolVar(&localOnly, "local-only", false, "skip the hub revoke when the hub is unreachable (leaves the token valid)")
	cmd.Flags().StringVar(&hubName, "hub", "", "named daemon catalog entry for hub admin auth (its URL must match the binding's hub URL)")
	cmd.Flags().StringVar(&hubToken, "hub-token", "", "explicit hub admin token (highest precedence)")
	cmd.Flags().BoolVar(&allowInsecure, "allow-insecure", false, "allow the hub revoke to send a bearer token to a plaintext HTTP hub hostname (private overlay networks); restores the join-time opt-in when it was lost with the credential")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return cmd
}

// resolveSpokeForLeave resolves the target project and its federation status:
//   - spoke binding → returns a full spokeLeaveTarget (normal leave path).
//   - no binding     → returns spokeLeaveTarget{standalone: true} (idempotent
//     resume for plain leave; archive-only path for --delete).
//   - hub binding    → hard error "not_a_spoke" (this command does not disband
//     hubs).
func resolveSpokeForLeave(ctx context.Context, client *http.Client, baseURL string, args []string) (spokeLeaveTarget, error) {
	project, err := resolveLeaveProject(ctx, client, baseURL, args)
	if err != nil {
		return spokeLeaveTarget{}, err
	}
	// include=archived: an archived spoke can still hold a binding — either a
	// partial archive-leave (detach failed after the archive committed) or a
	// `kata projects remove` on a federated project, which archives without
	// revoking. Both must take the bound path below; the hub revoke is
	// idempotent, so the already-revoked retry is a no-op while the
	// never-revoked remove case gets its enrollment revoked instead of
	// silently stranded.
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/federation/status?include=archived", nil)
	if err != nil {
		return spokeLeaveTarget{}, err
	}
	if status >= 400 {
		return spokeLeaveTarget{}, apiErrFromBody(status, bs)
	}
	var body api.FederationStatusBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return spokeLeaveTarget{}, err
	}
	var match *api.FederationProjectStatus
	for i := range body.Statuses {
		if body.Statuses[i].ProjectID == project.ID {
			match = &body.Statuses[i]
			break
		}
	}
	// No binding at all → project is already standalone. Return the standalone
	// signal so the caller skips hub contact; the daemon leave call still runs
	// to finish any stale-credential cleanup (plain leave) or archive (--delete).
	if match == nil {
		return spokeLeaveTarget{
			projectID:   project.ID,
			projectName: project.Name,
			standalone:  true,
		}, nil
	}
	// Hub-role binding → refuse with not_a_spoke (this command only handles
	// spoke teardown; hub disbanding is out of scope).
	if match.Role != string(db.FederationRoleSpoke) {
		return spokeLeaveTarget{}, &cliError{
			Message:  fmt.Sprintf("project %s is not a spoke", project.Name),
			Code:     "not_a_spoke",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	instanceUID, err := federationSpokeInstanceUID(ctx, client, baseURL)
	if err != nil {
		return spokeLeaveTarget{}, err
	}
	return spokeLeaveTarget{
		projectID:     project.ID,
		projectName:   project.Name,
		hubURL:        strings.TrimRight(match.HubURL, "/"),
		hubProjectID:  match.HubProjectID,
		instanceUID:   instanceUID,
		allowInsecure: match.AllowInsecure,
	}, nil
}

// revokeSpokeEnrollmentsOnHub lists the hub's enrollments and revokes every
// active project-scoped one bound to this spoke instance and hub project.
// Zero active matches is success (already revoked) ONLY when no other active
// project-scoped enrollment authorizes the hub project: the spoke's instance
// UID can drift from the enrollment's (clone/import refresh, or an enroll
// created with an explicit --spoke-instance), and silently proceeding would
// strand a live token with hub access. That case aborts with the surviving
// enrollment IDs; --local-only is the explicit local-teardown escape. The
// surviving IDs may also be other spokes of a shared hub project — the abort
// names them so the operator decides. Matching GLOBAL enrollments
// (project_id NULL) are returned, not revoked: they may authorize the spoke's
// other projects on the hub, but they do keep authorizing the left project, so
// the caller warns about them. Any hub transport/auth failure aborts before
// local teardown and instructs the operator to retry with --local-only.
func revokeSpokeEnrollmentsOnHub(ctx context.Context, target spokeLeaveTarget, in hubAuthInputs) ([]int64, error) {
	cat, err := config.ReadDaemonConfig()
	if err != nil {
		return nil, err
	}
	auth, err := resolveHubAdminAuth(cat, in)
	if err != nil {
		return nil, err
	}
	hub, err := hubAdminClient(ctx, auth)
	if err != nil {
		return nil, federationLeaveHubError(err)
	}
	status, bs, err := httpDoJSON(ctx, hub, http.MethodGet, auth.url+"/api/v1/federation/enrollments", nil)
	if err != nil {
		return nil, federationLeaveHubError(err)
	}
	if status >= 400 {
		return nil, federationLeaveHubError(apiErrFromBody(status, bs))
	}
	var list api.ListFederationEnrollmentsBody
	if err := json.Unmarshal(bs, &list); err != nil {
		return nil, err
	}
	var globals, matched, foreignScoped []int64
	for _, enrollment := range list.Enrollments {
		if enrollment.RevokedAt != nil {
			continue
		}
		if enrollment.ProjectID == nil {
			if enrollment.SpokeInstanceUID == target.instanceUID {
				globals = append(globals, enrollment.ID)
			}
			continue
		}
		if *enrollment.ProjectID != target.hubProjectID {
			continue
		}
		if enrollment.SpokeInstanceUID == target.instanceUID {
			matched = append(matched, enrollment.ID)
			continue
		}
		foreignScoped = append(foreignScoped, enrollment.ID)
	}
	if len(matched) == 0 && len(foreignScoped) > 0 {
		return nil, &cliError{
			Message: fmt.Sprintf(
				"no active enrollment matches this spoke's instance UID, but enrollment(s) %s still authorize hub project %d — the instance UID can change after a clone/import, or the enrollment may belong to another spoke instance; revoke the right one with `kata federation revoke <id>` on the hub, or rerun with --local-only to tear down locally without revoking",
				formatEnrollmentIDList(foreignScoped), target.hubProjectID),
			Code:     "leave_enrollment_uid_mismatch",
			Kind:     kindConflict,
			ExitCode: ExitConflict,
		}
	}
	for _, id := range matched {
		status, bs, err := httpDoJSON(ctx, hub, http.MethodPost,
			fmt.Sprintf("%s/api/v1/federation/enrollments/%d/revoke", auth.url, id), map[string]any{})
		if err != nil {
			return nil, federationLeaveHubError(err)
		}
		if status >= 400 {
			return nil, federationLeaveHubError(apiErrFromBody(status, bs))
		}
	}
	return globals, nil
}

func formatEnrollmentIDList(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	return strings.Join(parts, ", ")
}

// federationLeaveHubError wraps a hub-side failure with guidance to retry the
// leave with --local-only, since the local teardown is intentionally not
// attempted when the hub revoke cannot complete.
func federationLeaveHubError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("hub revoke failed: %w; rerun `kata federation leave --local-only` to tear down locally, then revoke on the hub later", err)
}

func confirmFederationLeave(cmd *cobra.Command, target spokeLeaveTarget, disposition string, localOnly, yes bool) error {
	if yes {
		return nil
	}
	action := "detach"
	if disposition == "archive" {
		action = "archive"
	}
	revokeNote := fmt.Sprintf("revoke the hub enrollment on %s, then ", textsafe.Line(target.hubURL))
	if localOnly {
		revokeNote = ""
	}
	prompt := fmt.Sprintf("This will %sleave federation for %q (%s). Continue? [y/N]: ",
		revokeNote, textsafe.Line(target.projectName), action)
	return resolveYesNo(cmd, prompt)
}

// resolveYesNo prompts for a yes/no confirmation on a TTY and accepts y/yes
// (case-insensitive). Without a TTY it returns confirm_required, mirroring
// resolveConfirm so noninteractive callers must pass --yes.
func resolveYesNo(cmd *cobra.Command, prompt string) error {
	if !isTTY(os.Stdin) {
		return &cliError{
			Message:  "no TTY: pass --yes to proceed noninteractively",
			Code:     "confirm_required",
			Kind:     kindConfirm,
			ExitCode: ExitConfirm,
		}
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), prompt); err != nil {
		return err
	}
	r := bufio.NewReader(cmd.InOrStdin())
	//nolint:errcheck // EOF here means stdin closed; treat as a "no".
	line, _ := r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return &cliError{
			Message:  "leave cancelled",
			Code:     "confirm_mismatch",
			Kind:     kindConfirm,
			ExitCode: ExitConfirm,
		}
	}
}

func printFederationLeave(cmd *cobra.Command, bs []byte) error {
	if currentOutputMode() == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var body api.LeaveFederationReplicaResultBody
	if err := json.Unmarshal(bs, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("project", body.Project.Name),
			agentRowField("disposition", body.Disposition),
			agentRowField("detached", strconv.FormatBool(body.Detached)),
			agentRowField("archived", strconv.FormatBool(body.Archived)),
		)
	}
	if flags.Quiet {
		return nil
	}
	if body.Archived {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"left federation and archived %s (restore with `kata projects restore %s`)\n",
			textsafe.Line(body.Project.Name), textsafe.Line(body.Project.Name))
		return err
	}
	if !body.Detached {
		// Idempotent resume: the daemon found no binding (it still cleans up
		// any stale hub credential), so nothing was left this time.
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"project %s is already standalone\n", textsafe.Line(body.Project.Name))
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"left federation: %s is now a standalone local project\n", textsafe.Line(body.Project.Name))
	return err
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
	Actor                  string `json:"actor,omitempty"`
	AllowInsecure          bool   `json:"allow_insecure,omitempty"`
	PushEnabled            bool   `json:"push_enabled,omitempty"`
	AdoptExisting          bool   `json:"adopt_existing,omitempty"`
}

var fetchFederationJoinMetadata = func(ctx context.Context, bundle federationJoinBundle) (api.ProjectFederationBody, error) {
	client, err := hubclient.NewClient(ctx, bundle.HubURL, bundle.Token,
		clientpkg.Opts{Timeout: envHTTPTimeout(defaultHTTPTimeout), AllowInsecure: bundle.AllowInsecure})
	if err != nil {
		return api.ProjectFederationBody{}, err
	}
	return client.ProjectFederation(ctx, bundle.HubProjectID)
}

func hydrateFederationJoinMetadata(ctx context.Context, bundle *federationJoinBundle) error {
	if bundle.HubProjectUID != "" && bundle.ReplayHorizonEventID > 0 {
		return nil
	}
	metadata, err := fetchFederationJoinMetadata(ctx, *bundle)
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

func resolveFederationProject(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	args []string,
	createMissing bool,
) (projectRef, error) {
	if len(args) > 0 {
		return resolveProjectSelector(ctx, client, baseURL, args[0])
	}
	if projectName := strings.TrimSpace(flags.Project); projectName != "" {
		if !createMissing {
			return resolveFederationProjectByName(ctx, client, baseURL, projectName)
		}
		return ensureFederationProjectByName(ctx, client, baseURL, projectName)
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return projectRef{}, err
	}
	id, name, err := resolveProjectIDAndNameWithClient(ctx, client, baseURL, start)
	if err != nil {
		return projectRef{}, err
	}
	return projectRef{ID: id, Name: name}, nil
}

func resolveFederationProjectByName(ctx context.Context, client *http.Client, baseURL, name string) (projectRef, error) {
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/projects/resolve", map[string]any{"name": name})
	if err != nil {
		return projectRef{}, err
	}
	if status >= 400 {
		return projectRef{}, apiErrFromBody(status, bs)
	}
	var resp struct {
		Project struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal(bs, &resp); err != nil {
		return projectRef{}, err
	}
	return projectRef{ID: resp.Project.ID, Name: resp.Project.Name}, nil
}

func ensureFederationProjectByName(ctx context.Context, client *http.Client, baseURL, name string) (projectRef, error) {
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/projects", map[string]any{"name": name})
	if err != nil {
		return projectRef{}, fmt.Errorf("POST /api/v1/projects: %w", err)
	}
	if status >= 300 {
		return projectRef{}, apiErrFromBody(status, bs)
	}
	var resp struct {
		Project struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal(bs, &resp); err != nil {
		return projectRef{}, err
	}
	return projectRef{ID: resp.Project.ID, Name: resp.Project.Name}, nil
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
	capabilities, err := hubclient.NormalizeCapabilities(raw)
	if err != nil {
		return "", "", &cliError{
			Message:  err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return capabilities.API, capabilities.Display, nil
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
		"--actor", bundle.Actor,
	}
	if bundle.PushEnabled {
		args = append(args, "--push")
	}
	if bundle.AllowInsecure {
		args = append(args, "--allow-insecure")
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
	actor, _ := resolveActor(ctx, flags.As, nil)
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
