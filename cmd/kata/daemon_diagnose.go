package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/version"
	kataapi "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kit/safefileio"
)

// daemonDiagnosis contains operational identity, never authentication secrets.
type daemonDiagnosis struct {
	State               string `json:"state"`
	Message             string `json:"message,omitempty"`
	ProjectState        string `json:"project_state,omitempty"`
	Source              string `json:"source"`
	SourcePath          string `json:"source_path,omitempty"`
	Kind                string `json:"kind"`
	Profile             string `json:"profile,omitempty"`
	Home                string `json:"home,omitempty"`
	StorageID           string `json:"storage_id,omitempty"`
	ExpectedInstanceUID string `json:"expected_instance_uid,omitempty"`
	ObservedInstanceUID string `json:"observed_instance_uid,omitempty"`
	SchemaVersion       int    `json:"schema_version,omitzero"`
	BinaryVersion       string `json:"binary_version"`
	RuntimeVersion      string `json:"runtime_version,omitempty"`
	PID                 int    `json:"pid,omitzero"`
	Endpoint            string `json:"endpoint,omitempty"`
	Project             string `json:"project,omitempty"`
	ProjectUID          string `json:"project_uid,omitempty"`
	ExpectedProjectUID  string `json:"expected_project_uid,omitempty"`
	FederationRole      string `json:"federation_role,omitempty"`
	HubOrigin           string `json:"hub_origin,omitempty"`
	Writable            *bool  `json:"writable,omitempty"`
	ActorPolicy         string `json:"actor_policy,omitempty"`
	NextAction          string `json:"next_action"`
}

func daemonDiagnoseCmd() *cobra.Command {
	var expected string
	cmd := &cobra.Command{Use: "diagnose", Short: "inspect the selected daemon without starting it", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateExpectedProjectUID(expected); err != nil {
			return err
		}
		result, _, err := diagnoseDaemon(cmd.Context(), expected)
		if err != nil {
			return err
		}
		return printDaemonDiagnosis(cmd, result)
	}}
	cmd.Flags().StringVar(&expected, "expect-project-uid", "", "assert the existing bound project's UID")
	return cmd
}

func daemonRecoverCmd() *cobra.Command {
	var expected string
	cmd := &cobra.Command{Use: "recover", Short: "start the selected existing local profile and verify its identity", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateExpectedProjectUID(expected); err != nil {
			return err
		}
		result, selection, err := diagnoseDaemon(cmd.Context(), expected)
		if err != nil {
			return err
		}
		if selection.Profile == nil {
			return &cliError{Message: "daemon recover requires a registered local profile with home and instance_uid; explicit URLs stay pinned", Kind: kindValidation, ExitCode: ExitValidation}
		}
		if result.State != "ready" && result.State != "stopped_local_profile" && result.State != "version_mismatch" {
			return diagnosisError(result)
		}
		if result.SchemaVersion != db.CurrentSchemaVersion() {
			return diagnosisError(result)
		}
		if _, err := client.EnsureLocalProfileSelection(cmd.Context(), selection); err != nil {
			return cliDaemonTargetError(err)
		}
		result, _, err = diagnoseDaemonSelection(cmd.Context(), expected, selection)
		if err != nil {
			return err
		}
		if result.State != "ready" {
			return diagnosisError(result)
		}
		return printDaemonDiagnosis(cmd, result)
	}}
	cmd.Flags().StringVar(&expected, "expect-project-uid", "", "assert the existing bound project's UID before and after recovery")
	return cmd
}

func validateExpectedProjectUID(value string) error {
	if value != "" {
		if _, err := ulid.ParseStrict(value); err != nil {
			return &cliError{Message: "--expect-project-uid must be a valid ULID", Kind: kindValidation, ExitCode: ExitValidation}
		}
	}
	return nil
}

func diagnoseDaemon(ctx context.Context, expectedProject string) (daemonDiagnosis, client.DaemonSelection, error) {
	selection, err := client.InspectSelection(ctx, workspaceStartForRemote(), flags.Daemon)
	if err != nil {
		return selectionErrorDiagnosis(err), selection, nil
	}
	result, selection, err := diagnoseDaemonSelection(ctx, expectedProject, selection)
	if err != nil {
		result.State = "selection_error"
		result.Message = err.Error()
		result = withDiagnosisAction(result)
	}
	return result, selection, nil
}

func selectionErrorDiagnosis(err error) daemonDiagnosis {
	return withDiagnosisAction(daemonDiagnosis{
		State: "selection_error", Source: "unknown", Kind: "unresolved",
		BinaryVersion: version.Version, Message: err.Error(),
	})
}

func selectedDaemonDiagnosis(selection client.DaemonSelection, expectedProject string) daemonDiagnosis {
	result := daemonDiagnosis{
		State: "local_stopped", Source: selection.Resolved.Source.String(),
		SourcePath: selection.Resolved.SourcePath, Kind: "local",
		BinaryVersion: version.Version, ExpectedProjectUID: expectedProject,
	}
	if flags.Daemon != "" {
		result.Source = "daemon_flag"
	}
	if profile := selection.Profile; profile != nil {
		result.State = "stopped_local_profile"
		result.Kind = "local_profile"
		result.Profile = profile.Name
		result.Home = profile.Home
		result.StorageID = profile.StorageID
		result.ExpectedInstanceUID = profile.InstanceUID
	} else if selection.Resolved.ConfiguredRemote() {
		result.Kind = "remote"
		result.Endpoint = safeDaemonOrigin(selection.Resolved.BaseURL)
	}
	return result
}

func currentHomeStorageIsAbsent(dsn string, openErr error) bool {
	if errors.Is(openErr, os.ErrNotExist) {
		return true
	}
	backend, err := storeopen.BackendForDSN(dsn)
	if err != nil || backend == storeopen.BackendPostgres {
		return false
	}
	path := strings.TrimPrefix(dsn, "sqlite://")
	_, err = os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func diagnoseDaemonSelection(
	ctx context.Context, expectedProject string, selection client.DaemonSelection,
) (daemonDiagnosis, client.DaemonSelection, error) {
	ctx, cancel := context.WithTimeout(ctx, envHTTPTimeout(defaultHTTPTimeout))
	defer cancel()
	var err error
	result := selectedDaemonDiagnosis(selection, expectedProject)
	result.Project = strings.TrimSpace(flags.Project)
	if result.Project == "" {
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return result, selection, err
		}
		result.Project = workspaceProjectName(start)
	}
	var store db.Storage
	if selection.Profile != nil {
		profile := *selection.Profile
		identity, err := client.InspectLocalProfileStorage(ctx, profile)
		result.ObservedInstanceUID = identity.InstanceUID
		result.SchemaVersion = identity.SchemaVersion
		if err != nil {
			result.State = "missing_profile_storage"
			result.Message = err.Error()
			if errors.Is(err, client.ErrProfileIdentityMismatch) {
				result.State = "wrong_database"
			}
			return withDiagnosisAction(result), selection, nil
		}
		if result.SchemaVersion != db.CurrentSchemaVersion() {
			result.State = "version_mismatch"
			return withDiagnosisAction(result), selection, nil
		}
		store, err = client.OpenLocalProfileReadOnly(ctx, profile)
		if err != nil {
			result.State = "missing_profile_storage"
			result.Message = err.Error()
			return withDiagnosisAction(result), selection, nil
		}
		result.State = "stopped_local_profile"
	} else if selection.Resolved.BaseURL == "" {
		result.Home, _ = config.KataHome()
		dsn, err := config.KataDSN(ctx)
		if err != nil {
			return result, selection, err
		}
		ns, err := daemon.NewNamespace()
		if err != nil {
			return result, selection, err
		}
		result.StorageID = ns.DBHash
		store, err = storeopen.OpenReadOnly(ctx, dsn)
		if err != nil || store == nil {
			if err != nil && !currentHomeStorageIsAbsent(dsn, err) {
				result.State = "unreadable_storage"
				result.Message = err.Error()
				return withDiagnosisAction(result), selection, nil
			}
			store = nil
		} else {
			if err = store.RefreshInstanceUID(ctx); err != nil {
				_ = store.Close()
				result.State = "unreadable_storage"
				result.Message = err.Error()
				return withDiagnosisAction(result), selection, nil
			}
			result.ObservedInstanceUID = store.InstanceUID()
			result.SchemaVersion, err = store.SchemaVersion(ctx)
			if err != nil {
				_ = store.Close()
				result.State = "unreadable_storage"
				result.Message = err.Error()
				return withDiagnosisAction(result), selection, nil
			}
		}
	}
	if store == nil && expectedProject != "" {
		result.State = "project_unverifiable"
		return withDiagnosisAction(result), selection, nil
	}
	if store != nil {
		defer func() { _ = store.Close() }()
		if result.SchemaVersion != db.CurrentSchemaVersion() {
			result.State = "version_mismatch"
			return withDiagnosisAction(result), selection, nil
		}
		if !inspectDiagnosisProject(ctx, store, &result) {
			return withDiagnosisAction(result), selection, nil
		}
	}
	result, selection, err = probeDaemonSelection(ctx, result, selection)
	if result.State == "ready" {
		result.Writable, result.ActorPolicy = liveDaemonCapabilities(ctx, selection.Resolved)
	}
	if err == nil && result.State == "ready" && selection.Profile != nil &&
		result.RuntimeVersion != version.Version {
		result.State = "version_mismatch"
	}
	return withDiagnosisAction(result), selection, err
}

func inspectDiagnosisProject(ctx context.Context, store db.Storage, result *daemonDiagnosis) bool {
	if result.Project == "" {
		if result.ExpectedProjectUID != "" {
			result.State = "missing_project"
			return false
		}
		return true
	}
	project, err := store.ProjectByName(ctx, result.Project)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			result.ProjectState = "missing_project"
			if result.ExpectedProjectUID == "" {
				return true
			}
			result.State = "missing_project"
		} else {
			result.State = "unreadable_storage"
			result.Message = err.Error()
		}
		return false
	}
	result.ProjectUID = project.UID
	if result.ExpectedProjectUID != "" && project.UID != result.ExpectedProjectUID {
		result.State = "wrong_project"
		return false
	}
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		result.State = "unreadable_storage"
		result.Message = err.Error()
		return false
	}
	result.FederationRole = string(binding.Role)
	result.HubOrigin = safeDaemonOrigin(binding.HubURL)
	return true
}

func probeDaemonSelection(
	ctx context.Context, result daemonDiagnosis, selection client.DaemonSelection,
) (daemonDiagnosis, client.DaemonSelection, error) {
	var err error
	var resolved client.ResolvedDaemon
	var found bool
	if selection.Profile != nil {
		resolved, found, err = client.DiscoverLocalProfileSelection(ctx, selection)
	} else if result.Kind == "remote" {
		resolved = selection.Resolved
		found = true
	} else {
		ns, namespaceErr := daemon.NewNamespace()
		if namespaceErr != nil {
			return result, selection, namespaceErr
		}
		if _, err = os.Stat(ns.DataDir); errors.Is(err, os.ErrNotExist) {
			return withDiagnosisAction(result), selection, nil
		} else if err != nil {
			return result, selection, err
		}
		if err := safefileio.ValidatePrivateDir(ns.DataDir); err != nil {
			result.State = "local_unreachable"
			result.Message = err.Error()
			return withDiagnosisAction(result), selection, nil
		}
		resolved, found, err = client.DiscoverResolved(ctx, ns.DataDir)
		if found && selection.Resolved.Name != "" {
			resolved = selection.Resolved.WithRunning(resolved.Running())
		}
	}
	if err != nil {
		result.Message = err.Error()
		if errors.Is(err, client.ErrProfileIdentityMismatch) {
			result.State = "wrong_database"
			if mismatch, ok := errors.AsType[*client.LocalProfileIdentityError](err); ok {
				result.ObservedInstanceUID = mismatch.Observed
			}
		} else {
			result.State = "local_unreachable"
		}
		if result.Kind == "remote" {
			result.State = unavailableRemoteState(result.Endpoint)
		}
		return withDiagnosisAction(result), selection, nil
	}
	if !found {
		return withDiagnosisAction(result), selection, nil
	}
	result.Endpoint = resolved.Address
	if resolved.ConfiguredRemote() {
		result.Endpoint = safeDaemonOrigin(resolved.BaseURL)
	}
	probeTarget := resolved
	if result.Kind == "remote" {
		probeTarget.Token = ""
	}
	hc, err := client.NewHTTPClientForResolved(ctx, probeTarget, client.Opts{Timeout: envHTTPTimeout(defaultHTTPTimeout)})
	if err != nil {
		return result, selection, err
	}
	apiClient, err := kataapi.NewWithHTTPClient(resolved.BaseURL, hc)
	if err != nil {
		return result, selection, err
	}
	ping, err := apiClient.Ping(ctx)
	if err != nil {
		result.State = "local_unreachable"
		if result.Kind == "remote" {
			result.State = unavailableRemoteState(result.Endpoint)
		}
		return withDiagnosisAction(result), selection, nil
	}
	if ping == nil || !ping.Ok || ping.Service != "kata" {
		result.State = "local_unreachable"
		if result.Kind == "remote" {
			result.State = unavailableRemoteState(result.Endpoint)
		}
		return withDiagnosisAction(result), selection, nil
	}
	selection.Resolved = resolved
	result.RuntimeVersion = ping.Version
	if ping.Pid != nil {
		result.PID = int(*ping.Pid)
	}
	result.State = "ready"
	return withDiagnosisAction(result), selection, nil
}

// Capabilities describe the selected live principal, not the presence of a
// credential. Unavailable/older instance responses leave authority unknown.
func liveDaemonCapabilities(ctx context.Context, resolved client.ResolvedDaemon) (*bool, string) {
	hc, err := client.NewHTTPClientForResolved(ctx, resolved, client.Opts{Timeout: envHTTPTimeout(defaultHTTPTimeout)})
	if err != nil {
		return nil, ""
	}
	apiClient, err := kataapi.NewWithHTTPClient(resolved.BaseURL, hc)
	if err != nil {
		return nil, ""
	}
	resp, err := apiClient.InstanceWithResponse(ctx)
	if err != nil || resp == nil || resp.JSON200 == nil {
		return nil, ""
	}
	var instance struct {
		Capabilities struct {
			Writable    *bool  `json:"writable"`
			ActorPolicy string `json:"actor_policy"`
		} `json:"web_ui_capabilities"`
	}
	if json.Unmarshal(resp.Body, &instance) != nil {
		return nil, ""
	}
	return instance.Capabilities.Writable, instance.Capabilities.ActorPolicy
}

func unavailableRemoteState(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err == nil {
		host := parsed.Hostname()
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() || strings.EqualFold(host, "localhost") {
			return "unknown_loopback_endpoint"
		}
	}
	return "unavailable_remote"
}

func safeDaemonOrigin(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func withDiagnosisAction(result daemonDiagnosis) daemonDiagnosis {
	switch result.State {
	case "ready":
		result.NextAction = "selected daemon is ready"
	case "unhealthy":
		result.NextAction = "inspect the selected daemon's health report and logs"
	case "stopped_local_profile":
		result.NextAction = "run kata daemon recover to start this existing profile"
	case "local_stopped":
		result.NextAction = "start the current home with kata daemon start"
	case "unknown_loopback_endpoint":
		result.NextAction = "start the configured server or tunnel; retain the workspace URL"
	case "unavailable_remote":
		result.NextAction = "restore the configured server connection; retain the selected target"
	case "missing_profile_storage":
		result.NextAction = "restore access to this profile's existing storage; recovery never initializes it"
	case "unreadable_storage":
		result.NextAction = "restore access to the current home's existing storage metadata before starting its daemon"
	case "wrong_database":
		result.NextAction = "check the profile home and pinned instance_uid; do not substitute another database"
	case "missing_project":
		result.NextAction = "check the bound project in this database; recovery never creates it"
	case "wrong_project":
		result.NextAction = "check the bound project UID; a same-named project cannot satisfy recovery"
	case "project_unverifiable":
		result.NextAction = "project UID assertion requires readable local storage; restore it or select its pinned local profile"
	case "version_mismatch":
		result.NextAction = "use a binary compatible with the storage schema"
		if result.Kind == "local_profile" {
			result.NextAction += "; recover may replace a compatible profile process"
		}
	case "selection_error":
		result.NextAction = "repair the selected routing configuration; current-home processes are listed separately"
	default:
		result.NextAction = "inspect the selected process and its credentials; do not switch databases"
	}
	return result
}

func diagnosisError(result daemonDiagnosis) error {
	data, _ := json.Marshal(result)
	return &cliError{Message: result.State + ": " + result.NextAction, Code: result.State, Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail, Data: data}
}

func printDaemonDiagnosis(cmd *cobra.Command, result daemonDiagnosis) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), result)
	}
	if currentOutputMode() == outputAgent {
		writable := "unknown"
		if result.Writable != nil {
			writable = strconv.FormatBool(*result.Writable)
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK daemon state=%s source=%s kind=%s profile=%s home=%s instance_uid=%s project_uid=%s writable=%s actor_policy=%s project_state=%s message=%s next_action=%s\n", result.State, agentValue(result.Source), result.Kind, agentValue(result.Profile), agentValue(result.Home), agentValue(result.ObservedInstanceUID), agentValue(result.ProjectUID), writable, agentValue(result.ActorPolicy), agentValue(result.ProjectState), agentValue(result.Message), agentValue(result.NextAction))
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Selected daemon: %s\n  source: %s\n", result.State, result.Source)
	if err != nil {
		return err
	}
	if result.Message != "" {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  reason: %s\n", result.Message); err != nil {
			return err
		}
	}
	if result.ProjectState != "" {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  project state: %s\n", result.ProjectState); err != nil {
			return err
		}
	}
	if result.Profile != "" {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  profile: %s\n  home: %s\n", result.Profile, result.Home); err != nil {
			return err
		}
	}
	if result.Endpoint != "" {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  endpoint: %s\n", result.Endpoint); err != nil {
			return err
		}
	}
	if result.ObservedInstanceUID != "" {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  instance UID: %s\n", result.ObservedInstanceUID); err != nil {
			return err
		}
	}
	if result.Project != "" {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  project: %s (%s)\n", result.Project, result.ProjectUID); err != nil {
			return err
		}
	}
	if result.Writable != nil {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  writable: %t\n  actor policy: %s\n", *result.Writable, result.ActorPolicy); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "  next: %s\n", result.NextAction)
	return err
}

func requireCurrentHomeDaemonCommand() error {
	if flags.Daemon == "" {
		return nil
	}
	return &cliError{Message: "this daemon command administers the current KATA_HOME and does not accept --daemon; set KATA_HOME explicitly or use daemon recover for a local profile", Kind: kindValidation, ExitCode: ExitValidation}
}
