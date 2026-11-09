package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

func trackerOperationDigest(body api.TrackerOperationBody) (string, error) {
	if body.Kind == tracker.ControlAcceptRemote {
		return "", errTrackerOperationUnsupported
	}
	if !uid.Valid(body.UID) || body.Kind.Action() == "" || !tracker.ValidIdentityText(body.Resource, 2048) || (body.Selection != nil && body.Selection.Validate() != nil) {
		return "", tracker.ErrInvalid
	}
	if _, ok := trackerBindOptions[body.Provider]; !ok {
		return "", tracker.ErrInvalid
	}
	if body.Kind == tracker.ControlRebind {
		if body.Selection == nil || body.ExpectedUpdatedAt == nil || body.ExpectedUpdatedAt.IsZero() {
			return "", tracker.ErrInvalid
		}
	} else if body.ExpectedUpdatedAt != nil {
		return "", tracker.ErrInvalid
	}
	if body.Kind == tracker.ControlBind || body.Kind == tracker.ControlRebind {
		if body.Binding == nil || validateTrackerBindOptions(body.Provider, body.Binding.Config) != nil {
			return "", tracker.ErrInvalid
		}
	} else if body.Binding != nil {
		return "", tracker.ErrInvalid
	}
	raw, err := json.Marshal(body, json.Deterministic(true))
	if err != nil || len(raw) > 32<<10 {
		return "", tracker.ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func trackerOperationOut(c tracker.ControlReceipt) api.TrackerOperationOut {
	return api.TrackerOperationOut{Settlement: c.Settlement, RootInstanceUID: c.RootInstanceUID, ProjectUID: c.ProjectUID, UID: c.UID, Selection: c.Selection, Kind: c.Kind, State: c.State, Result: c.Result}
}

// runTrackerOperation records admission before any provider work. Pending after
// a crash is an outcome to inspect, never a lease that expires into reexecution.
// Replay uses the original request digest before resolving today's binding, so
// omitted selection cannot silently acquire a different account on retry.
func runTrackerOperation(ctx context.Context, cfg ServerConfig, project db.Project, principal tracker.Principal, body api.TrackerOperationBody) (api.TrackerOperationOut, error) {
	digest, err := trackerOperationDigest(body)
	if err != nil {
		return api.TrackerOperationOut{}, err
	}
	if err := issueSyncEnableAuthority(ctx, cfg.DB, project.ID, body.Provider); err != nil {
		return api.TrackerOperationOut{}, err
	}
	snapshot, err := cfg.TrackerRuntime.current()
	if err != nil {
		return api.TrackerOperationOut{}, err
	}
	ctx, err = cfg.TrackerRuntime.mappedContext(ctx, snapshot, project.UID, principal)
	if err != nil {
		return api.TrackerOperationOut{}, err
	}
	controls, ok := cfg.DB.(db.TrackerControlStore)
	if !ok {
		return api.TrackerOperationOut{}, tracker.ErrUnavailable
	}
	existing, err := controls.ReadTrackerControl(ctx, project.UID, body.UID)
	if err == nil {
		if existing.Principal != principal || existing.RequestDigest != digest {
			return api.TrackerOperationOut{}, tracker.ErrControlConflict
		}
		return trackerOperationOut(existing), nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return api.TrackerOperationOut{}, err
	}
	binding, bindingErr := cfg.DB.IssueSyncBindingByProject(ctx, project.ID)
	if bindingErr != nil && !errors.Is(bindingErr, db.ErrNotFound) {
		return api.TrackerOperationOut{}, bindingErr
	}
	selection := body.Selection
	if selection == nil && bindingErr == nil {
		selection, err = db.IssueSyncConnectionFromConfig(binding.Config)
		if err != nil {
			return api.TrackerOperationOut{}, err
		}
	}
	if selection == nil {
		return api.TrackerOperationOut{}, tracker.ErrSelectionRequired
	}
	if body.Kind == tracker.ControlRebind {
		if bindingErr != nil {
			return api.TrackerOperationOut{}, bindingErr
		}
		old, err := db.IssueSyncConnectionFromConfig(binding.Config)
		if err != nil {
			return api.TrackerOperationOut{}, err
		}
		if old == nil || !binding.UpdatedAt.Equal(*body.ExpectedUpdatedAt) || binding.Provider != body.Provider || (binding.SourceKey != body.Resource && trackerBindingResourceAlias(binding) != body.Resource) {
			return api.TrackerOperationOut{}, db.ErrIssueSyncBindingChanged
		}
	}
	if body.Kind == tracker.ControlBind && bindingErr == nil {
		old, err := db.IssueSyncConnectionFromConfig(binding.Config)
		if err != nil {
			return api.TrackerOperationOut{}, err
		}
		if old == nil || *old != *selection {
			return api.TrackerOperationOut{}, db.ErrIssueSyncBindingChanged
		}
	}
	if body.Kind != tracker.ControlBind && body.Kind != tracker.ControlRebind {
		if (body.Kind == tracker.ControlRun || body.Kind == tracker.ControlActivate || body.Kind == tracker.ControlReconcile) && bindingErr == nil && !binding.Enabled {
			return api.TrackerOperationOut{}, db.ErrIssueSyncNotEnabled
		}
		if bindingErr != nil {
			return api.TrackerOperationOut{}, bindingErr
		}
		selected, err := db.IssueSyncConnectionFromConfig(binding.Config)
		if err != nil {
			return api.TrackerOperationOut{}, err
		}
		if binding.Provider != body.Provider || selected == nil || *selected != *selection || (binding.SourceKey != body.Resource && trackerBindingResourceAlias(binding) != body.Resource) {
			return api.TrackerOperationOut{}, db.ErrIssueSyncBindingChanged
		}
	}
	ctx, connection, err := cfg.TrackerRuntime.memberConnection(ctx, project.UID, principal, *selection, body.Kind.Action(), body.Resource)
	if err != nil {
		return api.TrackerOperationOut{}, err
	}
	// Close any remote member preflight session after the complete control.
	// Cleanup outlives cancellation only for its bounded reduction-only call.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = tracker.CloseRun(cleanup)
	}()
	if connection.descriptor.Provider != body.Provider {
		return api.TrackerOperationOut{}, tracker.ErrInvalid
	}
	if body.Kind == tracker.ControlBind || body.Kind == tracker.ControlRebind {
		if _, err := trackerBindResource(connection.config, body.Resource, body.Binding.Config); err != nil {
			return api.TrackerOperationOut{}, err
		}
	}
	finish, err := cfg.TrackerRuntime.beginControlAttempt(project.UID, body.UID)
	if err != nil {
		return api.TrackerOperationOut{}, err
	}
	defer finish()
	receipt := tracker.ControlReceipt{ProcessEpoch: cfg.TrackerRuntime.epoch, Version: 1, RootInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, UID: body.UID, Principal: principal, Selection: *selection, Kind: body.Kind, SourceKey: body.Resource, RequestDigest: digest, State: tracker.ControlPending}
	receipt, fresh, err := controls.BeginTrackerControl(ctx, receipt)
	if err != nil {
		return api.TrackerOperationOut{}, err
	}
	if !fresh {
		return trackerOperationOut(receipt), nil
	}
	result, callErr := trackerOperationEffect(ctx, cfg, project, connection, binding, body, *selection)
	if callErr != nil {
		result = tracker.ControlResult{Code: trackerControlErrorCode(callErr)}
	}
	next := receipt
	next.Result = &result
	next.State = tracker.ControlComplete
	if callErr != nil {
		next.State = tracker.ControlFailed
	}
	// A committed effect must retain its outcome even if the caller disconnects.
	// Keep live project/token fences; failure leaves the original pending receipt.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := controls.CompleteTrackerControl(finalCtx, receipt, next); err != nil {
		return api.TrackerOperationOut{}, tracker.ErrControlPending
	}
	return trackerOperationOut(next), nil
}

func trackerOperationEffect(ctx context.Context, cfg ServerConfig, project db.Project, connection trackerLocalConnection, binding db.IssueSyncBinding, body api.TrackerOperationBody, selection tracker.Selection) (tracker.ControlResult, error) {
	empty := tracker.ControlResult{}
	switch body.Kind {
	case tracker.ControlBind, tracker.ControlRebind:
		cfg = trackerPreflightConfig(cfg, connection)
		params, err := issueSyncEnableParams(ctx, cfg, body.Provider, &api.EnableIssueSyncRequest{ProjectID: project.ID, Provider: body.Provider, Body: *body.Binding})
		if err != nil {
			return empty, err
		}
		params.Connection = &selection
		raw, err := db.SetIssueSyncConnectionConfig(params.Config, selection)
		if err != nil {
			return empty, err
		}
		candidate := db.IssueSyncBinding{ProjectID: project.ID, Provider: params.Provider, SourceKey: params.SourceKey, Config: raw, ConnectionUID: &selection.ConnectionUID}
		if candidate.SourceKey != body.Resource && trackerBindingResourceAlias(candidate) != body.Resource {
			return empty, tracker.ErrDenied
		}
		// Preflight may have waited on the provider. Recheck both member and
		// independently configured background authority before committing a binding.
		if _, err := cfg.TrackerRuntime.selectedConnection(ctx, body.Provider, tracker.ActionBind); err != nil {
			return empty, err
		}
		routed, err := cfg.TrackerRuntime.RouteBinding(ctx, candidate)
		if err != nil {
			return empty, err
		}
		mode, err := db.IssueStatusMode(raw)
		if err != nil {
			return empty, err
		}
		if mode == "two-way" {
			if _, err := cfg.TrackerRuntime.selectedConnection(routed, body.Provider, tracker.ActionStatus); err != nil {
				return empty, err
			}
		}
		if err := cfg.TrackerRuntime.withSnapshotAdmission(ctx, func() error {
			if body.Kind == tracker.ControlRebind {
				rebinder, ok := cfg.DB.(db.TrackerRebindStore)
				if !ok {
					return tracker.ErrUnavailable
				}
				_, err := rebinder.RebindTrackerConnection(ctx, db.TrackerRebindParams{Binding: params, ExpectedUpdatedAt: *body.ExpectedUpdatedAt, ProcessEpoch: cfg.TrackerRuntime.epoch})
				return err
			}
			_, err := cfg.DB.UpsertIssueSyncBinding(ctx, params)
			return err
		}); err != nil {
			return empty, err
		}
		return tracker.ControlResult{Code: "ok", Enabled: true, ActivationRequired: body.Kind == tracker.ControlRebind || mode == "two-way"}, nil
	case tracker.ControlDisable:
		err := cfg.TrackerRuntime.withSnapshotAdmission(ctx, func() error { _, err := cfg.DB.DisableIssueSyncBinding(ctx, project.ID); return err })
		return tracker.ControlResult{Code: "ok", Enabled: false}, err
	case tracker.ControlRun, tracker.ControlReconcile, tracker.ControlActivate:
		if !binding.Enabled {
			return empty, db.ErrIssueSyncNotEnabled
		}
		if body.Kind != tracker.ControlRun {
			ctx = issuesync.WithReadOnlyStatusReconciliation(ctx)
		}
		runner := trackerRunner(cfg, body.Provider)
		result, err := runner.RunOnce(ctx, binding.ID)
		if err != nil {
			return empty, err
		}
		if body.Kind == tracker.ControlActivate {
			activation, ok := cfg.DB.(db.TrackerActivationStore)
			if !ok {
				return empty, tracker.ErrUnavailable
			}
			if _, _, err := cfg.TrackerRuntime.memberConnection(ctx, project.UID, ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority).principal, selection, tracker.ActionRecover, body.Resource); err != nil {
				return empty, err
			}
			if err := cfg.TrackerRuntime.withSnapshotAdmission(ctx, func() error {
				_, err := activation.ActivateTrackerBinding(ctx, result.Binding.ID, result.Binding.UpdatedAt, cfg.TrackerRuntime.epoch)
				return err
			}); err != nil {
				return empty, err
			}
		}
		return tracker.ControlResult{Code: "ok", Enabled: result.Binding.Enabled, Created: result.Import.Created, Updated: result.Import.Updated, Unchanged: result.Import.Unchanged, Comments: result.Import.Comments, StatusUpdated: result.StatusUpdated}, nil
	default:
		return empty, errTrackerOperationUnsupported
	}
}

var errTrackerOperationUnsupported = errors.New("tracker operation is unsupported")

func trackerRunner(cfg ServerConfig, provider string) GitHubSyncRunner {
	switch provider {
	case "notion":
		return notionSyncRunner(cfg)
	case "linear":
		return linearSyncRunner(cfg)
	case "plane":
		return planeSyncRunner(cfg)
	case "ticktick":
		return tickTickSyncRunner(cfg)
	case "todoist":
		return todoistSyncRunner(cfg)
	case "twenty":
		return twentySyncRunner(cfg)
	default:
		return githubSyncRunner(cfg)
	}
}
func trackerControlErrorCode(err error) string {
	switch {
	case errors.Is(err, db.ErrTrackerOutcomeUnknown):
		return "tracker_outcome_unknown"
	case errors.Is(err, db.ErrIssueSyncNotEnabled):
		return "tracker_binding_disabled"
	case errors.Is(err, db.ErrIssueSyncAlreadyRunning):
		return "tracker_running"
	case errors.Is(err, db.ErrIssueSyncProjectAlreadyBound):
		return "tracker_project_already_bound"
	case errors.Is(err, tracker.ErrInvalid):
		return "validation"
	case errors.Is(err, tracker.ErrDenied):
		return "tracker_denied"
	case errors.Is(err, context.DeadlineExceeded):
		return "tracker_timeout"
	case errors.Is(err, db.ErrNotFound):
		return "not_found"
	case errors.Is(err, db.ErrTrackerActivationRequired):
		return "tracker_activation_required"
	case errors.Is(err, db.ErrIssueSyncBindingChanged):
		return "tracker_binding_changed"
	case errors.Is(err, errTrackerOperationUnsupported):
		return "tracker_operation_unsupported"
	}
	// Existing preflight helpers return API errors, some containing provider
	// diagnostics. Retain only the class; never persist or return their message.
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "github_timeout", "notion_timeout", "linear_timeout", "plane_timeout", "ticktick_timeout", "todoist_timeout", "twenty_timeout", "tracker_timeout":
			return "tracker_timeout"
		case "not_found":
			return "not_found"
		case "validation":
			return "validation"
		}
	}
	return "tracker_unavailable"
}
