package main

import (
	"context"
	"errors"
	"net/http"
	"os"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/diagnostics"
	kataapi "go.kenn.io/kata/pkg/client"
)

var (
	errDoctorWorkspaceUnavailable = errors.New("workspace resolution failed")
	errDoctorNoRunningDaemon      = errors.New("no running local daemon")
)

func (s *doctorState) resolveDoctorDaemon(ctx context.Context) (client.ResolvedDaemon, error) {
	injectedURL, _ := ctx.Value(client.BaseURLKey{}).(string)
	if injectedURL != "" {
		return client.EnsureResolvedInWorkspace(ctx, "")
	}
	namedDaemon := flags.Daemon
	if !s.workspaceValid && namedDaemon == "" && os.Getenv("KATA_SERVER") == "" {
		cfg, err := config.ReadDaemonConfig()
		if err != nil {
			return client.ResolvedDaemon{}, err
		}
		if cfg != nil {
			namedDaemon = cfg.ActiveDaemon
		}
		if namedDaemon == "" {
			return client.ResolvedDaemon{}, errDoctorWorkspaceUnavailable
		}
	}
	if namedDaemon != "" {
		resolved, err := client.DiscoverResolvedNamedTargetReadOnly(ctx, namedDaemon)
		if err == nil && resolved.BaseURL == "" {
			err = errDoctorNoRunningDaemon
		}
		return resolved, err
	}
	resolved, found, err := client.DiscoverResolvedReadOnlyInWorkspace(ctx, s.workspace)
	if err == nil && !found {
		err = errDoctorNoRunningDaemon
	}
	return resolved, err
}

// doctorDiscoveryCheck maps a discovery error to its finding. A stopped local
// daemon and a runtime directory with loose permissions are normal states that
// the next ordinary command resolves, so neither is a failure.
func doctorDiscoveryCheck(err error) diagnostics.Check {
	if errors.Is(err, errDoctorWorkspaceUnavailable) {
		return skippedDoctorCheck("Workspace resolution failed")
	}
	if errors.Is(err, errDoctorNoRunningDaemon) {
		return diagnostics.Check{Status: diagnostics.StatusInfo, Summary: "No local daemon is running; the next kata command starts it", Fix: "Run kata daemon start, then rerun kata doctor to check the daemon."}
	}
	if _, ok := errors.AsType[*client.PrivateDirError](err); ok {
		return diagnostics.Check{Status: diagnostics.StatusWarn, Summary: "Local runtime directory is not private; doctor did not inspect it", Fix: "Run any kata command, such as kata list, to restore owner-only permissions. If the warning remains, check the ownership of <KATA_HOME>/runtime and that it is not a symlink."}
	}
	return diagnostics.Check{Status: diagnostics.StatusFail, Summary: "Selected daemon is unavailable or its target configuration is invalid", Fix: "Check --daemon, KATA_SERVER, .kata.local.toml and active_daemon; for an unreachable local daemon, inspect kata daemon status and its logs."}
}

func (s *doctorState) connectDoctorDaemon(ctx context.Context) (*kataapi.Client, bool, diagnostics.Check) {
	probe, cancel := context.WithTimeout(ctx, doctorRequestTimeout)
	defer cancel()
	resolved, err := s.resolveDoctorDaemon(probe)
	if err == nil && resolved.BaseURL == "" {
		err = errors.New("resolved daemon has no base URL")
	}
	if err != nil {
		return nil, false, doctorDiscoveryCheck(err)
	}
	hc, err := client.NewHTTPClientForResolved(probe, resolved, client.Opts{Timeout: doctorRequestTimeout})
	if err != nil {
		return nil, false, diagnostics.Check{Status: diagnostics.StatusFail, Summary: "Cannot construct a client for the selected daemon", Fix: "Check the selected daemon's credential and transport configuration."}
	}
	// Diagnostics follow no redirects even for uncredentialed requests.
	hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	apiClient, err := newDoctorAPIClient(resolved.BaseURL, hc)
	if err != nil {
		return nil, false, diagnostics.Check{Status: diagnostics.StatusFail, Summary: "Cannot construct a client for the selected daemon", Fix: "Check the selected daemon's credential and transport configuration."}
	}
	ping, err := apiClient.PingWithResponse(probe)
	if err != nil || ping == nil || ping.StatusCode != http.StatusOK || ping.JSON200 == nil || !ping.JSON200.Ok || ping.JSON200.Service != "kata" {
		return nil, false, diagnostics.Check{Status: diagnostics.StatusFail, Summary: "Selected endpoint did not identify a healthy Kata service", Fix: "Confirm the configured URL or local runtime record belongs to a Kata daemon."}
	}
	completeCatalog, check := doctorDaemonIdentity(probe, apiClient, resolved)
	if check.Status == diagnostics.StatusFail {
		return nil, false, check
	}
	return apiClient, completeCatalog, check
}

func doctorDaemonIdentity(ctx context.Context, apiClient *kataapi.Client, resolved client.ResolvedDaemon) (bool, diagnostics.Check) {
	instance, err := apiClient.InstanceWithResponse(ctx)
	verified := err == nil && instance != nil && instance.StatusCode == http.StatusOK && instance.JSON200 != nil
	var completeCatalog bool
	if verified {
		auth := instance.JSON200.Auth
		completeCatalog = auth.Kind != "" && auth.Scope == nil
	}
	if resolved.LocalProfile != nil {
		if !verified {
			status := 0
			if instance != nil {
				status = instance.StatusCode
			}
			return false, diagnostics.Check{Status: diagnostics.StatusFail, Summary: "Selected local profile identity could not be verified", Details: doctorHTTPStatus(status), Fix: "Check the selected profile's pinned instance_uid and daemon identity."}
		}
		if instance.JSON200.InstanceUID != resolved.LocalProfile.InstanceUID {
			return false, diagnostics.Check{Status: diagnostics.StatusFail, Summary: "Selected local profile identity does not match its configured instance_uid", Fix: "Check the selected profile's home and pinned instance_uid; do not substitute another database."}
		}
	}
	return completeCatalog, diagnostics.Check{Status: diagnostics.StatusOK, Summary: "Selected daemon identifies as Kata and is reachable"}
}
