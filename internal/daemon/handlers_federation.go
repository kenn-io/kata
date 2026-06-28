package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

func registerFederationHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "enableProjectFederation",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/federation/enable",
	}, func(ctx context.Context, in *api.EnableProjectFederationRequest) (*api.ProjectFederationResponse, error) {
		requestedActor := in.Body.Actor
		if requestedActor == "" {
			requestedActor = "federation"
		}
		actor, err := attributedActor(ctx, requestedActor)
		if err != nil {
			return nil, err
		}
		if _, err := cfg.DB.EnableProjectFederation(ctx, in.ProjectID, actor); err != nil {
			return nil, federationError(err)
		}
		body, err := projectFederationBody(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		return &api.ProjectFederationResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getProjectFederation",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/federation",
	}, func(ctx context.Context, in *api.ProjectFederationRequest) (*api.ProjectFederationResponse, error) {
		body, err := projectFederationBody(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		return &api.ProjectFederationResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getFederationStatus",
		Method:      "GET",
		Path:        "/api/v1/federation/status",
	}, func(ctx context.Context, in *api.FederationStatusRequest) (*api.FederationStatusResponse, error) {
		body, err := federationStatusBody(ctx, cfg.DB, nil, includeContains(in.Include, "archived"))
		if err != nil {
			return nil, err
		}
		return &api.FederationStatusResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getProjectFederationStatus",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/federation/status",
	}, func(ctx context.Context, in *api.ProjectFederationStatusRequest) (*api.FederationStatusResponse, error) {
		body, err := federationStatusBody(ctx, cfg.DB, &in.ProjectID, false)
		if err != nil {
			return nil, err
		}
		return &api.FederationStatusResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "skipFederationQuarantine",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/federation/quarantine/{quarantine_id}/skip",
	}, func(ctx context.Context, in *api.SkipFederationQuarantineRequest) (*api.SkipFederationQuarantineResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if err := validateExactConfirm(in.Confirm, fmt.Sprintf("SKIP FEDERATION BATCH %d", in.QuarantineID)); err != nil {
			return nil, err
		}
		q, err := cfg.DB.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{
			ID:        in.QuarantineID,
			ProjectID: in.ProjectID,
			Actor:     actor,
			Reason:    in.Body.Reason,
			Now:       time.Now().UTC(),
		})
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(http.StatusNotFound, "federation_quarantine_not_found", "federation quarantine not found", "", nil)
		}
		if err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		return &api.SkipFederationQuarantineResponse{Body: federationQuarantineSummary(q)}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "retryFederationQuarantine",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/federation/quarantine/{quarantine_id}/retry",
	}, func(ctx context.Context, in *api.RetryFederationQuarantineRequest) (*api.RetryFederationQuarantineResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if err := validateExactConfirm(in.Confirm, fmt.Sprintf("RETRY FEDERATION BATCH %d", in.QuarantineID)); err != nil {
			return nil, err
		}
		q, err := cfg.DB.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{
			ID:        in.QuarantineID,
			ProjectID: in.ProjectID,
			Actor:     actor,
			Reason:    in.Body.Reason,
			Now:       time.Now().UTC(),
		})
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(http.StatusNotFound, "federation_quarantine_not_found", "federation quarantine not found", "", nil)
		}
		if errors.Is(err, db.ErrFederationQuarantineRetryUnsupportedDirection) {
			return nil, api.NewError(http.StatusConflict, "federation_quarantine_retry_unsupported",
				"federation quarantine retry only supports push quarantines", "", nil)
		}
		if err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		return &api.RetryFederationQuarantineResponse{Body: federationQuarantineSummary(q)}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getFederationProjectMetadata",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/federation/metadata",
	}, func(ctx context.Context, in *api.FederationProjectMetadataRequest) (*api.ProjectFederationResponse, error) {
		if _, err := authorizeFederationRequest(ctx, cfg.DB, in.Authorization, in.ProjectID, "pull"); err != nil {
			return nil, err
		}
		body, err := projectFederationBody(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		return &api.ProjectFederationResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "createFederationEnrollment",
		Method:      "POST",
		Path:        "/api/v1/federation/enrollments",
	}, func(ctx context.Context, in *api.CreateFederationEnrollmentRequest) (*api.CreateFederationEnrollmentResponse, error) {
		if !katauid.Valid(in.Body.SpokeInstanceUID) {
			return nil, api.NewError(http.StatusBadRequest, "validation", "spoke_instance_uid must be a valid instance UID", "", nil)
		}
		if _, err := db.CanonicalFederationCapabilities(in.Body.Capabilities); err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		created, err := cfg.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token:                        in.Body.Token,
			SpokeInstanceUID:             in.Body.SpokeInstanceUID,
			ProjectID:                    in.Body.ProjectID,
			Capabilities:                 in.Body.Capabilities,
			Actor:                        actor,
			AllowAdoptionSnapshotAuthors: in.Body.AllowAdoptionSnapshotAuthors,
		})
		if err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		return &api.CreateFederationEnrollmentResponse{
			Body: federationEnrollmentToOut(created.Enrollment, created.Token),
		}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listFederationEnrollments",
		Method:      "GET",
		Path:        "/api/v1/federation/enrollments",
	}, func(ctx context.Context, _ *api.ListFederationEnrollmentsRequest) (*api.ListFederationEnrollmentsResponse, error) {
		enrollments, err := cfg.DB.ListFederationEnrollments(ctx)
		if err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		out := make([]api.FederationEnrollmentOut, 0, len(enrollments))
		for _, enrollment := range enrollments {
			out = append(out, federationEnrollmentToOut(enrollment, ""))
		}
		return &api.ListFederationEnrollmentsResponse{
			Body: api.ListFederationEnrollmentsBody{Enrollments: out},
		}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "revokeFederationEnrollment",
		Method:      "POST",
		Path:        "/api/v1/federation/enrollments/{enrollment_id}/revoke",
	}, func(ctx context.Context, in *api.RevokeFederationEnrollmentRequest) (*api.RevokeFederationEnrollmentResponse, error) {
		if err := cfg.DB.RevokeFederationEnrollment(ctx, in.EnrollmentID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, api.NewError(http.StatusNotFound, "federation_enrollment_not_found", "federation enrollment not found", "", nil)
			}
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		return &api.RevokeFederationEnrollmentResponse{
			Body: api.RevokeFederationEnrollmentBody{ID: in.EnrollmentID, Revoked: true},
		}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "createFederationReplica",
		Method:      "POST",
		Path:        "/api/v1/federation/replicas",
	}, func(ctx context.Context, in *api.CreateFederationReplicaRequest) (*api.CreateFederationReplicaResponse, error) {
		if in.Body.HubURL == "" || in.Body.HubProjectID <= 0 || in.Body.HubProjectUID == "" ||
			in.Body.ProjectName == "" || in.Body.ReplayHorizonEventID <= 0 {
			return nil, api.NewError(400, "validation", "hub_url, hub_project_id, hub_project_uid, project_name, and replay_horizon_event_id are required", "", nil)
		}
		if !katauid.Valid(in.Body.HubProjectUID) {
			return nil, api.NewError(400, "validation", "hub_project_uid must be a valid UID", "", nil)
		}
		capabilities, err := normalizedReplicaCapabilities(in.Body.Capabilities)
		if err != nil {
			return nil, err
		}
		if in.Body.PushEnabled && !federationCapabilitiesContain(capabilities, "push") {
			return nil, api.NewError(400, "federation_capability_mismatch", "push-enabled federation replica requires push capability", "", nil)
		}
		if err := db.ValidateTokenActor(in.Body.Actor); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		if in.Body.AdoptExisting {
			if !in.Body.PushEnabled {
				return nil, api.NewError(400, "federation_capability_mismatch", "adopting an existing project requires push to be enabled", "", nil)
			}
			if !federationCapabilitiesContain(capabilities, "pull") || !federationCapabilitiesContain(capabilities, "push") {
				return nil, api.NewError(400, "federation_capability_mismatch", "adopting an existing project requires pull and push capabilities", "", nil)
			}
		}
		project, binding, adopted, adoptionSnapshotCount, err := ensureReplicaBindingOrAdopt(ctx, cfg.DB, in)
		if err != nil {
			return nil, err
		}
		if binding.PushEnabled && in.Body.Token != "" && !federationCapabilitiesContain(capabilities, "push") {
			return nil, api.NewError(400, "federation_capability_mismatch", "push-enabled federation replica credentials require push capability", "", nil)
		}
		if in.Body.Token != "" {
			if err := config.WriteFederationCredential(project.UID, config.FederationCredential{
				HubURL:        in.Body.HubURL,
				HubProjectID:  in.Body.HubProjectID,
				Token:         in.Body.Token,
				Capabilities:  capabilities,
				Actor:         strings.TrimSpace(in.Body.Actor),
				AllowInsecure: in.Body.AllowInsecure,
			}); err != nil {
				return nil, api.NewError(500, "internal", err.Error(), "", nil)
			}
		}
		if in.Body.PushEnabled && !binding.PushEnabled {
			binding, err = enableReplicaPush(ctx, cfg.DB, project.ID)
			if err != nil {
				return nil, err
			}
		}
		if cfg.FederationWake != nil {
			cfg.FederationWake()
		}
		return &api.CreateFederationReplicaResponse{Body: api.CreateFederationReplicaBody{
			Project:               dbProjectToOut(project),
			Binding:               federationBindingToOut(binding),
			Adopted:               adopted,
			AdoptionSnapshotCount: adoptionSnapshotCount,
		}}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "leaveFederationReplica",
		Method:      "POST",
		Path:        "/api/v1/federation/replicas/{project_id}/actions/leave",
	}, func(ctx context.Context, in *api.LeaveFederationReplicaRequest) (*api.LeaveFederationReplicaResponse, error) {
		if in.ProjectID <= 0 {
			return nil, api.NewError(http.StatusBadRequest, "validation", "project_id must be a positive integer", "", nil)
		}
		disposition := strings.TrimSpace(in.Body.Disposition)
		if disposition == "" {
			disposition = "detach"
		}
		if disposition != "detach" && disposition != "archive" {
			return nil, api.NewError(http.StatusBadRequest, "validation", `disposition must be "detach" or "archive"`, "", nil)
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		// Refuse a non-spoke before any teardown so an archive-leave on a hub
		// project does not archive it and then fail to detach. A project with no
		// binding is the idempotent resume case and is allowed (RemoveProject and
		// LeaveFederationReplica below handle existence and the standalone path).
		if binding, bErr := cfg.DB.FederationBindingByProject(ctx, in.ProjectID); bErr == nil {
			if binding.Role != db.FederationRoleSpoke {
				return nil, api.NewError(http.StatusConflict, "not_a_spoke", "federation binding is not a spoke", "", nil)
			}
		} else if !errors.Is(bErr, db.ErrNotFound) {
			return nil, api.NewError(http.StatusInternalServerError, "internal", bErr.Error(), "", nil)
		}

		if in.Body.Preflight {
			// Advisory dry-run: surface what the real call would refuse —
			// most importantly an archive's open-issue refusal — WITHOUT
			// mutating anything, so leave clients can verify archive
			// eligibility before the irreversible hub revoke. Advisory only:
			// the authoritative check stays inside RemoveProject's
			// transaction below.
			project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
			switch {
			case errors.Is(err, db.ErrNotFound):
				return nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
			case err != nil:
				return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
			}
			// Mirror RemoveProject's refusal for a live archive target; an
			// already-archived project passes (the real call resumes).
			if disposition == "archive" && project.DeletedAt == nil && !in.Body.Force {
				openIssues, err := cfg.DB.CountOpenIssues(ctx, in.ProjectID)
				if err != nil {
					return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
				}
				if openIssues > 0 {
					return nil, api.NewError(http.StatusConflict, "project_has_open_issues", "project has open issues",
						"close the open issues first, or pass force=true",
						map[string]any{"open_issues": openIssues})
				}
			}
			return &api.LeaveFederationReplicaResponse{Body: api.LeaveFederationReplicaResultBody{
				Disposition: disposition,
				Project:     dbProjectToOut(project),
			}}, nil
		}

		body := api.LeaveFederationReplicaResultBody{Disposition: disposition}
		// Archive FIRST when requested. RemoveProject's own transaction is the
		// authoritative open-issue check, so a refused archive never tears down
		// federation — there is no external-preflight TOCTOU and no
		// "detached-but-not-archived" partial state. Only a committed archive —
		// from this call or a prior partial leave — proceeds to the detach
		// below.
		if disposition == "archive" {
			project, evt, err := cfg.DB.RemoveProject(ctx, db.RemoveProjectParams{
				ProjectID: in.ProjectID, Actor: actor, Force: in.Body.Force,
			})
			var openErr *db.ProjectHasOpenIssuesError
			switch {
			case errors.Is(err, db.ErrNotFound):
				return nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
			case errors.As(err, &openErr):
				return nil, api.NewError(http.StatusConflict, "project_has_open_issues", "project has open issues",
					"close the open issues first, or pass force=true",
					map[string]any{"open_issues": openErr.OpenIssues})
			case errors.Is(err, db.ErrProjectAlreadyArchived):
				// Idempotent resume: a prior archive-leave committed the archive
				// but failed before the detach or credential cleanup below.
				// Refusing here would strand that state forever, so keep going;
				// Archived stays false because this call archived nothing, and
				// the project fill below uses ProjectByID, which includes
				// archived rows.
			case err != nil:
				return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
			default:
				cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: project.ID})
				cfg.Hooks.Enqueue(*evt)
				body.Project = dbProjectToOut(project)
				body.Archived = true
			}
		}

		res, err := cfg.DB.LeaveFederationReplica(ctx, in.ProjectID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
		case errors.Is(err, db.ErrFederationNotSpoke):
			return nil, api.NewError(http.StatusConflict, "not_a_spoke", "federation binding is not a spoke", "", nil)
		case err != nil:
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		// Zero role means there was no binding: this is the idempotent resume
		// (only the credential delete below may still have work to do), so the
		// response must not claim a detach happened.
		body.Detached = res.Role == db.FederationRoleSpoke
		if res.ProjectUID != "" {
			if err := config.DeleteFederationCredential(res.ProjectUID); err != nil {
				return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
			}
		}
		if !body.Archived {
			project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
			if err != nil {
				return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
			}
			body.Project = dbProjectToOut(project)
		}
		if cfg.FederationWake != nil {
			cfg.FederationWake()
		}
		return &api.LeaveFederationReplicaResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "pollFederationProjectEvents",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/federation/events",
	}, func(ctx context.Context, in *api.FederationPollEventsRequest) (*api.PollEventsResponse, error) {
		if _, err := authorizeFederationRequest(ctx, cfg.DB, in.Authorization, in.ProjectID, "pull"); err != nil {
			return nil, err
		}
		if in.ProjectID <= 0 {
			return nil, api.NewError(http.StatusBadRequest, "validation", "project_id must be a positive integer", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		return doPollEvents(ctx, cfg, in.AfterID, in.Limit, in.ProjectID)
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "ingestFederationProjectEvents",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/federation/events:ingest",
	}, func(ctx context.Context, in *api.FederationIngestEventsRequest) (*api.FederationIngestEventsResponse, error) {
		principal, err := authorizeFederationRequest(ctx, cfg.DB, in.Authorization, in.ProjectID, "push")
		if err != nil {
			return nil, err
		}
		if in.ProjectID <= 0 {
			return nil, api.NewError(http.StatusBadRequest, "validation", "project_id must be a positive integer", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		if err := validateFederationIngestSchemaVersion(in.Body.SchemaVersion); err != nil {
			return nil, err
		}
		result, err := cfg.DB.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:                       in.ProjectID,
			FederationEnrollmentID:          principal.EnrollmentID,
			SpokeInstanceUID:                principal.SpokeInstanceUID,
			BoundActor:                      principal.Actor,
			AllowSnapshotAuthorPreservation: principal.AllowAdoptionSnapshotAuthors,
			Events:                          federationIngestEventsToDB(in.Body.Events),
		})
		if err != nil {
			return nil, federationIngestError(err)
		}
		if err := federationFailpoint("after_federation_ingest_commit_before_broadcast"); err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "federation_failpoint", err.Error(), "", nil)
		}
		inserted, err := cfg.DB.EventsByUIDs(ctx, in.ProjectID, result.InsertedEventUIDs)
		if err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
		for _, evt := range inserted {
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(evt)
		}
		return &api.FederationIngestEventsResponse{Body: api.FederationIngestEventsBody{
			Accepted:          result.Accepted,
			Duplicates:        result.Duplicates,
			PushCursorEventID: result.PushCursorEventID,
		}}, nil
	})
}

func validateFederationIngestSchemaVersion(schemaVersion int) error {
	current := db.CurrentSchemaVersion()
	if schemaVersion <= 0 {
		return api.NewError(http.StatusBadRequest, "invalid_federation_schema",
			"federation ingest schema_version is required", "", nil)
	}
	if schemaVersion > current {
		return api.NewError(http.StatusBadRequest, "unsupported_federation_schema",
			fmt.Sprintf("federation ingest schema_version %d is newer than hub schema_version %d", schemaVersion, current), "", nil)
	}
	return nil
}

func federationIngestEventsToDB(events []api.FederationIngestEventEnvelope) []db.FederationIngestEvent {
	out := make([]db.FederationIngestEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, db.FederationIngestEvent{
			SourceEventID: ev.EventID,
			Event: db.RemoteEvent{
				EventUID:          ev.EventUID,
				OriginInstanceUID: ev.OriginInstanceUID,
				ProjectUID:        ev.ProjectUID,
				ProjectName:       ev.ProjectName,
				IssueUID:          ev.IssueUID,
				RelatedIssueUID:   ev.RelatedIssueUID,
				Type:              ev.Type,
				Actor:             ev.Actor,
				HLCPhysicalMS:     ev.HLCPhysicalMS,
				HLCCounter:        ev.HLCCounter,
				ContentHash:       ev.ContentHash,
				Payload:           ev.Payload,
				CreatedAt:         ev.CreatedAt,
			},
		})
	}
	return out
}

func federationIngestError(err error) error {
	switch {
	case errors.Is(err, db.ErrRemoteEventConflict):
		return api.NewError(http.StatusConflict, "remote_event_conflict", err.Error(), "", nil)
	case errors.Is(err, db.ErrRemoteEventHashMismatch), errors.Is(err, db.ErrFederationIngestValidation):
		return api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(http.StatusNotFound, "federation_not_found", err.Error(), "", nil)
	default:
		return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
}

func normalizedReplicaCapabilities(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	capabilities, err := db.CanonicalFederationCapabilities(raw)
	if err != nil {
		return "", api.NewError(400, "validation", err.Error(), "", nil)
	}
	return capabilities, nil
}

func federationCapabilitiesContain(capabilities, want string) bool {
	for _, part := range strings.Split(capabilities, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func ensureReplicaBindingOrAdopt(
	ctx context.Context,
	store db.Storage,
	in *api.CreateFederationReplicaRequest,
) (db.Project, db.FederationBinding, bool, int64, error) {
	if in.Body.AdoptExisting {
		if result, adopted, err := adoptExistingReplica(ctx, store, in); err != nil {
			return db.Project{}, db.FederationBinding{}, false, 0, err
		} else if adopted {
			return result.Project, result.Binding, true, result.AdoptionSnapshotCount, nil
		}
	}
	project, binding, err := ensureReplicaBinding(ctx, store, in)
	return project, binding, false, 0, err
}

func adoptExistingReplica(
	ctx context.Context,
	store db.Storage,
	in *api.CreateFederationReplicaRequest,
) (db.AdoptProjectIntoFederationResult, bool, error) {
	projectName := strings.TrimSpace(in.Body.ProjectName)
	if err := config.ValidateProjectName(projectName); err != nil {
		return db.AdoptProjectIntoFederationResult{}, false, api.NewError(400, "validation", err.Error(), "", nil)
	}
	if project, err := store.ProjectByUID(ctx, in.Body.HubProjectUID); err == nil {
		if project.DeletedAt != nil {
			return db.AdoptProjectIntoFederationResult{}, false,
				api.NewError(409, "federation_project_collision", "hub project UID belongs to an archived local project", "", nil)
		}
		binding, bindErr := store.FederationBindingByProject(ctx, project.ID)
		if bindErr != nil {
			if errors.Is(bindErr, db.ErrNotFound) {
				if project.Name != projectName {
					return db.AdoptProjectIntoFederationResult{}, false,
						api.NewError(409, "federation_project_collision",
							fmt.Sprintf("hub project UID belongs to local project %q; cannot adopt local project %q", project.Name, projectName), "", nil)
				}
				// An explicit adopt_existing on an unbound UID-holder is
				// honored: adoption is the actor-safe transmission path for
				// local events authored by other actors (raw push would be
				// rejected by the hub's actor binding). The post-leave rejoin
				// railroading is prevented upstream — the enroll command and
				// the TUI no longer request adoption for UID-holders.
				result, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
					ProjectID:            project.ID,
					HubURL:               in.Body.HubURL,
					HubProjectID:         in.Body.HubProjectID,
					HubProjectUID:        in.Body.HubProjectUID,
					ReplayHorizonEventID: in.Body.ReplayHorizonEventID,
					Actor:                strings.TrimSpace(in.Body.Actor),
					AllowInsecure:        in.Body.AllowInsecure,
				})
				if err != nil {
					if errors.Is(err, db.ErrIssueSyncFederationBinding) {
						return db.AdoptProjectIntoFederationResult{}, false, issueSyncFederationConflict()
					}
					return db.AdoptProjectIntoFederationResult{}, false, api.NewError(500, "internal", err.Error(), "", nil)
				}
				return result, true, nil
			}
			return db.AdoptProjectIntoFederationResult{}, false, api.NewError(500, "internal", bindErr.Error(), "", nil)
		}
		if details := replicaBindingConflictDetails(binding, in); len(details) > 0 {
			return db.AdoptProjectIntoFederationResult{}, false,
				api.NewError(409, "federation_binding_conflict",
					"existing federation binding differs from the requested hub: "+strings.Join(details, ", "), "", nil)
		}
		if project.Name != projectName {
			return db.AdoptProjectIntoFederationResult{}, false,
				api.NewError(409, "federation_project_collision",
					fmt.Sprintf("hub project UID is already bound to local project %q; cannot adopt local project %q", project.Name, projectName), "", nil)
		}
		return db.AdoptProjectIntoFederationResult{}, false, nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.AdoptProjectIntoFederationResult{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	existing, err := store.ProjectByNameIncludingArchived(ctx, projectName)
	if errors.Is(err, db.ErrNotFound) {
		return db.AdoptProjectIntoFederationResult{}, false,
			api.NewError(404, "federation_project_not_found", "adoption requested but no local project exists with this name", "", nil)
	}
	if err != nil {
		return db.AdoptProjectIntoFederationResult{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if existing.UID == in.Body.HubProjectUID {
		return db.AdoptProjectIntoFederationResult{}, false,
			api.NewError(409, "federation_project_collision", "hub project UID already exists locally but is not bound to federation", "", nil)
	}
	if existing.DeletedAt != nil {
		return db.AdoptProjectIntoFederationResult{}, true,
			api.NewError(409, "federation_project_collision", "a deleted project with this name cannot be adopted into federation", "", nil)
	}
	if binding, err := store.FederationBindingByProject(ctx, existing.ID); err == nil {
		return db.AdoptProjectIntoFederationResult{}, true,
			api.NewError(409, "federation_binding_conflict",
				fmt.Sprintf("project already has %q federation binding", binding.Role), "", nil)
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.AdoptProjectIntoFederationResult{}, true, api.NewError(500, "internal", err.Error(), "", nil)
	}
	result, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID:            existing.ID,
		HubURL:               in.Body.HubURL,
		HubProjectID:         in.Body.HubProjectID,
		HubProjectUID:        in.Body.HubProjectUID,
		ReplayHorizonEventID: in.Body.ReplayHorizonEventID,
		Actor:                strings.TrimSpace(in.Body.Actor),
		AllowInsecure:        in.Body.AllowInsecure,
	})
	if err != nil {
		if errors.Is(err, db.ErrIssueSyncFederationBinding) {
			return db.AdoptProjectIntoFederationResult{}, true, issueSyncFederationConflict()
		}
		return db.AdoptProjectIntoFederationResult{}, true, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return result, true, nil
}

func ensureReplicaBinding(
	ctx context.Context,
	store db.Storage,
	in *api.CreateFederationReplicaRequest,
) (db.Project, db.FederationBinding, error) {
	projectName := strings.TrimSpace(in.Body.ProjectName)
	if err := config.ValidateProjectName(projectName); err != nil {
		return db.Project{}, db.FederationBinding{}, api.NewError(400, "validation", err.Error(), "", nil)
	}
	project, err := store.ProjectByUID(ctx, in.Body.HubProjectUID)
	createdProject := false
	if errors.Is(err, db.ErrNotFound) {
		if existing, lookupErr := store.ProjectByNameIncludingArchived(ctx, projectName); lookupErr == nil {
			if existing.UID != in.Body.HubProjectUID {
				return db.Project{}, db.FederationBinding{}, api.NewError(409, "federation_project_collision", "a project with this name already has a different UID; rerun with --adopt-existing --push to adopt it into federation", "", nil)
			}
		} else if !errors.Is(lookupErr, db.ErrNotFound) {
			return db.Project{}, db.FederationBinding{}, api.NewError(500, "internal", lookupErr.Error(), "", nil)
		}
		project, err = store.CreateProjectWithUID(ctx, projectName, in.Body.HubProjectUID)
		if err != nil {
			return db.Project{}, db.FederationBinding{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		createdProject = true
	} else if err != nil {
		return db.Project{}, db.FederationBinding{}, api.NewError(500, "internal", err.Error(), "", nil)
	} else if project.DeletedAt != nil {
		return db.Project{}, db.FederationBinding{}, api.NewError(409, "federation_project_collision",
			fmt.Sprintf("an archived local project %q already has the hub project UID; restore it with `kata projects restore` first", project.Name), "", nil)
	}

	replayHorizon := in.Body.ReplayHorizonEventID
	cursor := replayHorizon - 1
	if cursor < 0 {
		cursor = 0
	}
	pushEnabled := false
	pushCursor := int64(0)
	existing, err := store.FederationBindingByProject(ctx, project.ID)
	if err == nil {
		if details := replicaBindingConflictDetails(existing, in); len(details) > 0 {
			return db.Project{}, db.FederationBinding{}, api.NewError(409, "federation_binding_conflict",
				"existing federation binding differs from the requested hub: "+strings.Join(details, ", "), "", nil)
		}
		replayHorizon = existing.ReplayHorizonEventID
		cursor = existing.PullCursorEventID
		pushEnabled = existing.PushEnabled
		pushCursor = existing.PushCursorEventID
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.Project{}, db.FederationBinding{}, api.NewError(500, "internal", err.Error(), "", nil)
	} else if !createdProject {
		// An unbound local project holding the hub project UID is the normal
		// post-leave state: leave removes the binding but the project keeps the
		// shared identity. A join naming that project is a rejoin — rebind it.
		// Pull restarts just below the replay horizon (event-UID dedup absorbs
		// the overlap) and a push-enabled rejoin re-offers local-origin events
		// from cursor 0 so the hub dedups what it already has and absorbs edits
		// made while the project was standalone.
		//
		// Trust model: a spoke and the hubs it federates with trust each other
		// (docs/design/federation.md "Tokens And Trust Boundaries" / "No
		// Multi-Tenant Authorization Model"). The UID is an unguessable ULID, so
		// a hub reporting it as a project identity means it IS the project that
		// federated there; we do not defend against a hostile hub forging a known
		// UID to capture local data (out of scope). The operator-facing rejoin
		// preview (CLI/TUI) is the confirmation surface.
		if project.Name != projectName {
			return db.Project{}, db.FederationBinding{}, api.NewError(409, "federation_rejoin_name_mismatch",
				fmt.Sprintf("hub project UID is held by local project %q, which previously left this federation; rerun join with --project %q to rejoin it", project.Name, project.Name), "", nil)
		}
		if in.Body.PushEnabled {
			pushEnabled = true
		}
	}

	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               in.Body.HubURL,
		HubProjectID:         in.Body.HubProjectID,
		HubProjectUID:        in.Body.HubProjectUID,
		ReplayHorizonEventID: replayHorizon,
		PullCursorEventID:    cursor,
		PushEnabled:          pushEnabled,
		PushCursorEventID:    pushCursor,
		Actor:                strings.TrimSpace(in.Body.Actor),
		AllowInsecure:        in.Body.AllowInsecure,
		Enabled:              true,
	})
	if err != nil {
		if errors.Is(err, db.ErrIssueSyncFederationBinding) {
			return db.Project{}, db.FederationBinding{}, issueSyncFederationConflict()
		}
		return db.Project{}, db.FederationBinding{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return project, binding, nil
}

func enableReplicaPush(ctx context.Context, store db.Storage, projectID int64) (db.FederationBinding, error) {
	localCursor, err := maxLocalOriginEventID(ctx, store, projectID)
	if err != nil {
		return db.FederationBinding{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	binding, err := store.EnableFederationPush(ctx, projectID, localCursor)
	if err != nil {
		return db.FederationBinding{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return binding, nil
}

func maxLocalOriginEventID(ctx context.Context, store db.Storage, projectID int64) (int64, error) {
	return store.MaxLocalOriginEventID(ctx, projectID)
}

func replicaBindingConflictDetails(existing db.FederationBinding, in *api.CreateFederationReplicaRequest) []string {
	existingActor := strings.TrimSpace(existing.Actor)
	requestedActor := strings.TrimSpace(in.Body.Actor)
	actorCompatible := existingActor == requestedActor || (existingActor == "" && requestedActor != "")
	details := make([]string, 0, 5)
	if existing.Role != db.FederationRoleSpoke {
		details = append(details, fmt.Sprintf("role existing=%s requested=%s", existing.Role, db.FederationRoleSpoke))
	}
	if existing.HubURL != in.Body.HubURL {
		details = append(details, fmt.Sprintf("hub_url existing=%s requested=%s", existing.HubURL, in.Body.HubURL))
	}
	if existing.HubProjectID != in.Body.HubProjectID {
		details = append(details, fmt.Sprintf("hub_project_id existing=%d requested=%d", existing.HubProjectID, in.Body.HubProjectID))
	}
	if existing.HubProjectUID != in.Body.HubProjectUID {
		details = append(details, fmt.Sprintf("hub_project_uid existing=%s requested=%s", existing.HubProjectUID, in.Body.HubProjectUID))
	}
	if !actorCompatible {
		details = append(details, fmt.Sprintf("actor existing=%s requested=%s", existingActor, requestedActor))
	}
	return details
}

func federationBindingToOut(binding db.FederationBinding) api.FederationBindingOut {
	return api.FederationBindingOut{
		ProjectID:            binding.ProjectID,
		Role:                 string(binding.Role),
		HubURL:               binding.HubURL,
		HubProjectID:         binding.HubProjectID,
		HubProjectUID:        binding.HubProjectUID,
		ReplayHorizonEventID: binding.ReplayHorizonEventID,
		PullCursorEventID:    binding.PullCursorEventID,
		PushEnabled:          binding.PushEnabled,
		PushCursorEventID:    binding.PushCursorEventID,
		Actor:                binding.Actor,
		Enabled:              binding.Enabled,
		CreatedAt:            binding.CreatedAt,
		UpdatedAt:            binding.UpdatedAt,
		LastSyncAt:           binding.LastSyncAt,
	}
}

func federationEnrollmentToOut(enrollment db.FederationEnrollment, token string) api.FederationEnrollmentOut {
	return api.FederationEnrollmentOut{
		ID:               enrollment.ID,
		SpokeInstanceUID: enrollment.SpokeInstanceUID,
		ProjectID:        enrollment.ProjectID,
		Capabilities:     enrollment.Capabilities,
		Actor:            enrollment.Actor,
		CreatedAt:        enrollment.CreatedAt,
		UpdatedAt:        enrollment.UpdatedAt,
		RevokedAt:        enrollment.RevokedAt,
		Token:            token,
	}
}

func federationStatusBody(ctx context.Context, store db.Storage, projectID *int64, includeArchived bool) (api.FederationStatusBody, error) {
	bindings, err := federationStatusBindings(ctx, store, projectID)
	if err != nil {
		return api.FederationStatusBody{}, err
	}
	out := api.FederationStatusBody{Statuses: make([]api.FederationProjectStatus, 0, len(bindings))}
	for _, binding := range bindings {
		status, err := federationProjectStatus(ctx, store, binding, includeArchived)
		if err != nil {
			if projectID == nil && isProjectNotFound(err) {
				continue
			}
			return api.FederationStatusBody{}, err
		}
		out.Statuses = append(out.Statuses, status)
	}
	return out, nil
}

func isProjectNotFound(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) &&
		apiErr != nil &&
		apiErr.Status == http.StatusNotFound &&
		apiErr.Code == "project_not_found"
}

func federationStatusBindings(ctx context.Context, store db.Storage, projectID *int64) ([]db.FederationBinding, error) {
	if projectID == nil {
		bindings, err := store.ListFederationBindings(ctx)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		return bindings, nil
	}
	if _, err := activeProjectByID(ctx, store, *projectID); err != nil {
		return nil, err
	}
	binding, err := store.FederationBindingByProject(ctx, *projectID)
	if errors.Is(err, db.ErrNotFound) {
		return []db.FederationBinding{}, nil
	}
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return []db.FederationBinding{binding}, nil
}

func federationProjectStatus(ctx context.Context, store db.Storage, binding db.FederationBinding, includeArchived bool) (api.FederationProjectStatus, error) {
	project, err := activeProjectByID(ctx, store, binding.ProjectID)
	if includeArchived && isProjectNotFound(err) {
		// include=archived: an archived project's binding is still real —
		// leave needs it to run the bound path (idempotent hub revoke +
		// teardown) instead of misclassifying the spoke as standalone.
		project, err = store.ProjectByID(ctx, binding.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			return api.FederationProjectStatus{}, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
		} else if err != nil {
			return api.FederationProjectStatus{}, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
		}
	} else if err != nil {
		return api.FederationProjectStatus{}, err
	}
	syncStatus, err := store.FederationSyncStatusByProject(ctx, binding.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		syncStatus = db.FederationSyncStatus{}
	} else if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	pendingPush, pendingHighWater, err := federationPendingPushStats(ctx, store, binding)
	if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	enrollments, err := federationEnrollmentCount(ctx, store, binding)
	if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	liveClaims, err := federationLiveClaimCount(ctx, store, binding.ProjectID)
	if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	pendingClaims, err := federationPendingClaimCount(ctx, store, binding.ProjectID)
	if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	activeQuarantines, err := store.ActiveFederationQuarantinesByProject(ctx, binding.ProjectID)
	if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	recentViolations, unresolvedViolationCount, err := store.UnresolvedClaimViolationsForProject(ctx, binding.ProjectID, 5)
	if err != nil {
		return api.FederationProjectStatus{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	var credentialMetadata config.FederationCredentialMetadata
	if binding.Role == db.FederationRoleSpoke {
		credentialMetadata = config.FederationCredentialMetadataFor(project.UID)
	}
	// The credential's allow_insecure is only meaningful for the hub it was
	// recorded for: a stale credential from an older enrollment (e.g. a
	// tokenless rejoin that skipped the credential rewrite) must not authorize
	// plaintext bearer transport to the binding's CURRENT hub. Same URL
	// normalization as the leave client's catalog matching.
	credentialAllowInsecure := credentialMetadata.AllowInsecure &&
		credentialMetadata.HubProjectID == binding.HubProjectID &&
		strings.TrimRight(credentialMetadata.HubURL, "/") == strings.TrimRight(binding.HubURL, "/")
	return api.FederationProjectStatus{
		ProjectID:     project.ID,
		ProjectUID:    project.UID,
		ProjectName:   project.Name,
		Role:          string(binding.Role),
		Enabled:       binding.Enabled,
		PushEnabled:   binding.PushEnabled,
		BoundActor:    binding.Actor,
		HubURL:        binding.HubURL,
		HubProjectID:  binding.HubProjectID,
		HubProjectUID: binding.HubProjectUID,
		Capabilities:  credentialMetadata.Capabilities,
		// Opt-ins union: the binding is the durable record (it survives a
		// credential loss during leave recovery); the same-hub credential copy
		// keeps bindings recorded before allow_insecure was persisted working.
		AllowInsecure:               binding.AllowInsecure || credentialAllowInsecure,
		CredentialStatus:            credentialMetadata.Status,
		PullCursorEventID:           binding.PullCursorEventID,
		PushCursorEventID:           binding.PushCursorEventID,
		PendingPushCount:            pendingPush,
		PendingPushHighWaterEventID: pendingHighWater,
		EnrollmentCount:             enrollments,
		LiveClaimCount:              liveClaims,
		PendingClaimCount:           pendingClaims,
		ActiveQuarantineCount:       int64(len(activeQuarantines)),
		ActiveQuarantines:           federationQuarantineSummaries(activeQuarantines),
		ResetBlocker:                federationResetBlocker(pendingPush, activeQuarantines),
		UnresolvedViolationCount:    unresolvedViolationCount,
		RecentViolationCount:        int64(len(recentViolations)),
		RecentViolations:            federationViolationSummaries(recentViolations),
		LastSyncAt:                  binding.LastSyncAt,
		LastSuccessfulSyncAt: latestTime(binding.LastSyncAt,
			syncStatus.LastPullSuccessAt, syncStatus.LastPushSuccessAt),
		LastPullStartedAt: syncStatus.LastPullStartedAt,
		LastPullSuccessAt: syncStatus.LastPullSuccessAt,
		LastPushStartedAt: syncStatus.LastPushStartedAt,
		LastPushSuccessAt: syncStatus.LastPushSuccessAt,
		LastErrorAt:       syncStatus.LastErrorAt,
		LastError:         syncStatus.LastError,
		LastResetAt:       syncStatus.LastResetAt,
	}, nil
}

func federationQuarantineSummaries(quarantines []db.FederationQuarantine) []api.FederationQuarantineSummary {
	out := make([]api.FederationQuarantineSummary, 0, len(quarantines))
	for _, q := range quarantines {
		out = append(out, federationQuarantineSummary(q))
	}
	return out
}

func federationQuarantineSummary(q db.FederationQuarantine) api.FederationQuarantineSummary {
	return api.FederationQuarantineSummary{
		ID:           q.ID,
		Direction:    string(q.Direction),
		FirstEventID: q.FirstEventID,
		LastEventID:  q.LastEventID,
		EventUIDs:    q.EventUIDs,
		Error:        q.Error,
		CreatedAt:    q.CreatedAt,
	}
}

func federationResetBlocker(pendingPush int64, quarantines []db.FederationQuarantine) string {
	if len(quarantines) > 0 {
		return "quarantine"
	}
	if pendingPush > 0 {
		return "pending_push"
	}
	return ""
}

func federationViolationSummaries(violations []db.ClaimViolationSummary) []api.FederationViolationSummary {
	out := make([]api.FederationViolationSummary, 0, len(violations))
	for _, v := range violations {
		out = append(out, api.FederationViolationSummary{
			EventID:                    v.EventID,
			EventUID:                   v.EventUID,
			IssueUID:                   v.IssueUID,
			ShortID:                    v.IssueShortID,
			OffendingEventUID:          v.OffendingEventUID,
			OffendingEventType:         v.OffendingEventType,
			OffendingOriginInstanceUID: v.OffendingOriginInstanceUID,
			Reason:                     v.Reason,
			Actor:                      v.Actor,
			At:                         v.At,
		})
	}
	return out
}

func federationPendingPushStats(ctx context.Context, store db.Storage, binding db.FederationBinding) (int64, int64, error) {
	if binding.Role != db.FederationRoleSpoke || !binding.PushEnabled {
		return 0, 0, nil
	}
	return store.PendingFederationPushStats(ctx, binding.ProjectID, store.InstanceUID(), binding.PushCursorEventID)
}

func federationEnrollmentCount(ctx context.Context, store db.Storage, binding db.FederationBinding) (int64, error) {
	if binding.Role != db.FederationRoleHub {
		return 0, nil
	}
	return store.CountActiveFederationEnrollments(ctx, binding.ProjectID)
}

func federationLiveClaimCount(ctx context.Context, store db.Storage, projectID int64) (int64, error) {
	return store.CountLiveClaims(ctx, projectID)
}

func federationPendingClaimCount(ctx context.Context, store db.Storage, projectID int64) (int64, error) {
	return store.CountPendingClaims(ctx, projectID)
}

func latestTime(times ...*time.Time) *time.Time {
	var latest *time.Time
	for _, candidate := range times {
		if candidate == nil {
			continue
		}
		if latest == nil || candidate.After(*latest) {
			latest = candidate
		}
	}
	return latest
}

func projectFederationBody(ctx context.Context, store db.Storage, projectID int64) (api.ProjectFederationBody, error) {
	project, err := activeProjectByID(ctx, store, projectID)
	if err != nil {
		return api.ProjectFederationBody{}, err
	}
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if err != nil {
		return api.ProjectFederationBody{}, federationError(err)
	}
	if binding.Role == db.FederationRoleHub && binding.Enabled {
		resetTo, err := store.PurgeResetCheck(ctx, binding.ReplayHorizonEventID, projectID)
		if err != nil {
			return api.ProjectFederationBody{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if resetTo > 0 {
			binding, _, err = store.RefreshProjectFederationBaseline(ctx, projectID, "federation")
			if err != nil {
				return api.ProjectFederationBody{}, api.NewError(500, "internal", err.Error(), "", nil)
			}
		}
	}
	through := binding.ReplayHorizonEventID
	maxSnapshot, err := store.MaxFederationBaselineEventID(ctx, projectID, binding.ReplayHorizonEventID)
	if err != nil {
		return api.ProjectFederationBody{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if maxSnapshot > 0 {
		through = maxSnapshot
	}
	return api.ProjectFederationBody{
		ProjectID:              project.ID,
		ProjectUID:             project.UID,
		ProjectName:            project.Name,
		ReplayHorizonEventID:   binding.ReplayHorizonEventID,
		BaselineThroughEventID: through,
	}, nil
}

func federationError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(404, "federation_not_found", "federation metadata not found", "", nil)
	}
	if errors.Is(err, db.ErrIssueSyncFederationBinding) {
		return issueSyncFederationConflict()
	}
	return api.NewError(500, "internal", err.Error(), "", nil)
}

// issueSyncFederationConflict reports a 409 for the lifecycle rule that an
// issue-synced project cannot become a federation spoke.
func issueSyncFederationConflict() error {
	return api.NewError(409, "issue_sync_federation_conflict",
		"project has issue sync enabled; run GitHub sync on the federation hub, or disable issue sync before joining this project as a spoke", "", nil)
}
