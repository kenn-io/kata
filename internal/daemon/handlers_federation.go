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
		body, err := federationStatusBody(ctx, cfg.DB, cfg.federationCredentialStore(), nil, includeContains(in.Include, "archived"))
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
		body, err := federationStatusBody(ctx, cfg.DB, cfg.federationCredentialStore(), &in.ProjectID, false)
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
			return nil, internalAPIError(err)
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
			return nil, internalAPIError(err)
		}
		return &api.RetryFederationQuarantineResponse{Body: federationQuarantineSummary(q)}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getFederationProjectMetadata",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/federation/metadata",
	}, func(ctx context.Context, in *api.FederationProjectMetadataRequest) (*api.ProjectFederationResponse, error) {
		var err error
		ctx, _, err = authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull",
			federationTransportOperation("getFederationProjectMetadata"))
		if err != nil {
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
		Summary:     "Create a federation enrollment",
		Description: "Creates a hub-side transport grant. When the request includes a caller-supplied token, an exact retry returns the same active enrollment; reusing that token for different attributes or after revocation returns 409.",
	}, func(ctx context.Context, in *api.CreateFederationEnrollmentRequest) (*api.CreateFederationEnrollmentResponse, error) {
		if !katauid.Valid(in.Body.SpokeInstanceUID) {
			return nil, api.NewError(http.StatusBadRequest, "validation", "spoke_instance_uid must be a valid instance UID", "", nil)
		}
		if _, err := db.CanonicalFederationCapabilities(in.Body.Capabilities); err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
		}
		if in.Body.AllowAdoptionSnapshotAuthors && in.Body.ProjectID == nil {
			return nil, api.NewError(http.StatusBadRequest, "validation",
				"allow_adoption_snapshot_authors requires project_id", "", nil)
		}
		var projectIDs []int64
		if in.Body.ProjectID != nil {
			if *in.Body.ProjectID <= 0 {
				return nil, api.NewError(http.StatusBadRequest, "validation",
					"project_id must be a positive integer", "", nil)
			}
			projectIDs = []int64{*in.Body.ProjectID}
		}
		var err error
		ctx, err = authorizeHostProjectScope(ctx, projectIDs, nil, in.Body.ProjectID == nil)
		if err != nil {
			return nil, err
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if err := db.ValidateTokenActor(actor); err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
		}
		created, err := cfg.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token:                        in.Body.Token,
			SpokeInstanceUID:             in.Body.SpokeInstanceUID,
			ProjectID:                    in.Body.ProjectID,
			Capabilities:                 in.Body.Capabilities,
			Actor:                        actor,
			AllowAdoptionSnapshotAuthors: in.Body.AllowAdoptionSnapshotAuthors,
		})
		if errors.Is(err, db.ErrFederationEnrollmentTokenConflict) {
			return nil, api.NewError(http.StatusConflict, "federation_enrollment_token_conflict",
				"federation enrollment token conflicts with an existing enrollment", "", nil)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		return &api.CreateFederationEnrollmentResponse{
			Body: federationEnrollmentToOut(created.Enrollment, created.Token),
		}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "rotateFederationEnrollment",
		Method:      "POST",
		Path:        "/api/v1/federation/enrollments/actions/rotate",
		Summary:     "Rotate a federation enrollment",
		Description: "Transactionally revokes active project-scoped grants for the spoke and installs the caller-supplied replacement token. After canonical capability normalization and token-authenticated actor resolution, an exact replay with the same replacement token, spoke instance, project, canonical capabilities, resolved actor, and adoption policy returns the same active enrollment. An attribute mismatch or revoked replacement enrollment returns 409 with code federation_enrollment_token_conflict.",
	}, func(ctx context.Context, in *api.RotateFederationEnrollmentRequest) (*api.RotateFederationEnrollmentResponse, error) {
		if !katauid.Valid(in.Body.SpokeInstanceUID) {
			return nil, api.NewError(http.StatusBadRequest, "validation", "spoke_instance_uid must be a valid instance UID", "", nil)
		}
		if _, err := db.CanonicalFederationCapabilities(in.Body.Capabilities); err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
		}
		if in.Body.ProjectID <= 0 {
			return nil, api.NewError(http.StatusBadRequest, "validation",
				"project_id must be a positive integer", "", nil)
		}
		if in.Body.Token == "" {
			return nil, api.NewError(http.StatusBadRequest, "validation",
				"replacement token is required", "", nil)
		}
		var err error
		ctx, err = authorizeHostProjectScope(ctx, []int64{in.Body.ProjectID}, nil, false)
		if err != nil {
			return nil, err
		}
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if err := db.ValidateTokenActor(actor); err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
		}
		projectID := in.Body.ProjectID
		created, err := cfg.DB.RotateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token:                        in.Body.Token,
			SpokeInstanceUID:             in.Body.SpokeInstanceUID,
			ProjectID:                    &projectID,
			Capabilities:                 in.Body.Capabilities,
			Actor:                        strings.TrimSpace(actor),
			AllowAdoptionSnapshotAuthors: in.Body.AllowAdoptionSnapshotAuthors,
		})
		if errors.Is(err, db.ErrFederationEnrollmentTokenConflict) {
			return nil, api.NewError(http.StatusConflict, "federation_enrollment_token_conflict",
				"federation enrollment token conflicts with an existing enrollment", "", nil)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		return &api.RotateFederationEnrollmentResponse{
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
			return nil, internalAPIError(err)
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
			return nil, internalAPIError(err)
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
		actor := actorFor(ctx, in.Body.Actor)
		if err := db.ValidateTokenActor(actor); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		result, err := EnsureFederationReplica(
			ctx,
			cfg.DB,
			cfg.federationCredentialStore(),
			cfg.FederationWake,
			EnsureFederationReplicaParams{
				HubURL:               in.Body.HubURL,
				HubProjectID:         in.Body.HubProjectID,
				HubProjectUID:        in.Body.HubProjectUID,
				ProjectName:          in.Body.ProjectName,
				ReplayHorizonEventID: in.Body.ReplayHorizonEventID,
				Credential: config.FederationCredential{
					HubURL:        in.Body.HubURL,
					HubProjectID:  in.Body.HubProjectID,
					Token:         in.Body.Token,
					Capabilities:  in.Body.Capabilities,
					Actor:         strings.TrimSpace(actor),
					AllowInsecure: in.Body.AllowInsecure,
				},
				PushEnabled:   in.Body.PushEnabled,
				AdoptExisting: in.Body.AdoptExisting,
				ProjectEventSink: func(event db.Event) {
					deliverProjectMutation(cfg, &event)
				},
			},
		)
		if err != nil {
			return nil, federationReplicaAPIError(err)
		}
		return &api.CreateFederationReplicaResponse{Body: api.CreateFederationReplicaBody{
			Project:               dbProjectToOut(result.Project),
			Binding:               federationBindingToOut(result.Binding),
			Adopted:               result.Adopted,
			AdoptionSnapshotCount: result.AdoptionSnapshotCount,
		}}, nil
	})

	if !cfg.DisableFederationRebind {
		huma.Register(humaAPI, huma.Operation{
			OperationID: "rebindFederationReplica",
			Method:      "POST",
			Path:        "/api/v1/federation/replicas/{project_id}/actions/rebind",
		}, func(ctx context.Context, in *api.RebindFederationReplicaRequest) (*api.RebindFederationReplicaResponse, error) {
			if in.ProjectID <= 0 {
				return nil, api.NewError(http.StatusBadRequest, "validation", "project_id must be a positive integer", "", nil)
			}
			catalog, err := federationRebindCatalogByName(cfg.FederationCatalog, in.Body.HubCatalog)
			if err != nil {
				return nil, err
			}
			result, err := RebindFederationReplica(
				ctx,
				cfg.DB,
				cfg.federationCredentialStore(),
				RebindFederationReplicaParams{
					ProjectID:     in.ProjectID,
					HubCatalog:    catalog,
					FetchMetadata: cfg.FederationRebindFetchMetadata,
				},
			)
			if err != nil {
				return nil, federationReplicaAPIError(err)
			}
			oldOrigin, err := config.CanonicalHTTPOrigin(result.PreviousHubURL)
			if err != nil {
				return nil, internalAPIError(fmt.Errorf("canonicalize previous federation origin: %w", err))
			}
			newOrigin, err := config.CanonicalHTTPOrigin(result.Binding.HubURL)
			if err != nil {
				return nil, internalAPIError(fmt.Errorf("canonicalize rebound federation origin: %w", err))
			}
			if cfg.FederationWake != nil {
				cfg.FederationWake()
			}
			return &api.RebindFederationReplicaResponse{Body: api.RebindFederationReplicaResponseBody{
				Project: api.FederationRebindProjectOut{
					ID: result.Project.ID, UID: result.Project.UID, Name: result.Project.Name,
				},
				OldOrigin: oldOrigin,
				NewOrigin: newOrigin,
				State:     string(result.State),
			}}, nil
		})
	}

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
			return nil, internalAPIError(bErr)
		}

		if in.Body.Preflight && in.Body.Prepare {
			return nil, api.NewError(
				http.StatusBadRequest,
				"validation",
				"preflight and prepare cannot both be true",
				"",
				nil,
			)
		}
		if in.Body.Preflight || in.Body.Prepare {
			// Both phases surface what the real call would refuse, most
			// importantly an archive's open-issue refusal. Preflight is
			// read-only; prepare then durably blocks config reconciliation and
			// drains earlier hub operations before the irreversible hub revoke.
			// The authoritative archive check stays in RemoveProject below.
			project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
			switch {
			case errors.Is(err, db.ErrNotFound):
				return nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
			case err != nil:
				return nil, internalAPIError(err)
			}
			// Mirror RemoveProject's refusal for a live archive target; an
			// already-archived project passes (the real call resumes).
			if disposition == "archive" && project.DeletedAt == nil && !in.Body.Force {
				openIssues, err := cfg.DB.CountOpenIssues(ctx, in.ProjectID)
				if err != nil {
					return nil, internalAPIError(err)
				}
				if openIssues > 0 {
					return nil, api.NewError(http.StatusConflict, "project_has_open_issues", "project has open issues",
						"close the open issues first, or pass force=true",
						map[string]any{"open_issues": openIssues})
				}
			}
			body := api.LeaveFederationReplicaResultBody{
				Disposition: disposition,
				Project:     dbProjectToOut(project),
			}
			managed, ok := cfg.federationCredentialStore().(config.FederationManagedCredentialStore)
			if ok {
				var (
					match   config.FederationManagedCredentialReservation
					found   bool
					findErr error
				)
				if in.Body.Prepare {
					prepared, prepareErr := PrepareFederationReplicaLeave(
						ctx,
						cfg.DB,
						cfg.federationCredentialStore(),
						in.ProjectID,
					)
					match = prepared.ManagedReservation
					found = prepared.ManagedReservationFound
					findErr = prepareErr
				} else {
					match, found, findErr = managed.FindManagedFederationCredential(ctx, project.Name)
				}
				switch {
				case errors.Is(findErr, config.ErrFederationCredentialConflict):
					return nil, api.NewError(
						http.StatusConflict,
						"federation_credential_conflict",
						"managed federation credential cleanup is blocked by conflicting reservations",
						"resolve the conflicting credentials, then retry leave",
						nil,
					)
				case findErr != nil:
					return nil, internalAPIError(findErr)
				case found:
					body.PendingEnrollment = &api.PendingFederationEnrollmentCleanup{
						HubURL:        match.Credential.HubURL,
						HubProjectID:  match.Credential.HubProjectID,
						HubProjectUID: match.ProjectUID,
						AllowInsecure: match.Credential.AllowInsecure,
					}
				}
			}
			return &api.LeaveFederationReplicaResponse{Body: body}, nil
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
				return nil, internalAPIError(err)
			default:
				cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: project.ID})
				cfg.Hooks.Enqueue(*evt)
				body.Project = dbProjectToOut(project)
				body.Archived = true
			}
		}

		res, err := LeaveFederationReplica(
			ctx,
			cfg.DB,
			cfg.federationCredentialStore(),
			cfg.FederationWake,
			in.ProjectID,
		)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
		case errors.Is(err, db.ErrFederationNotSpoke):
			return nil, api.NewError(http.StatusConflict, "not_a_spoke", "federation binding is not a spoke", "", nil)
		case errors.Is(err, ErrFederationReplicaCredentialConflict),
			errors.Is(err, config.ErrFederationCredentialConflict):
			return nil, api.NewError(
				http.StatusConflict,
				"federation_credential_conflict",
				"federation credential changed during leave",
				"resolve the credential conflict in credentials.toml, then retry kata federation leave",
				nil,
			)
		case err != nil:
			return nil, internalAPIError(err)
		}
		// Zero role means there was no binding: this is the idempotent resume
		// (only the credential delete below may still have work to do), so the
		// response must not claim a detach happened.
		body.Detached = res.Role == db.FederationRoleSpoke
		if !body.Archived {
			project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
			if err != nil {
				return nil, internalAPIError(err)
			}
			body.Project = dbProjectToOut(project)
		}
		return &api.LeaveFederationReplicaResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "pollFederationProjectEvents",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/federation/events",
	}, func(ctx context.Context, in *api.FederationPollEventsRequest) (*api.PollEventsResponse, error) {
		var err error
		ctx, _, err = authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull",
			federationTransportOperation("pollFederationProjectEvents"))
		if err != nil {
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
		OperationID:  "ingestFederationProjectEvents",
		Method:       "POST",
		Path:         "/api/v1/projects/{project_id}/federation/events:ingest",
		MaxBodyBytes: 64 << 20,
	}, func(ctx context.Context, in *api.FederationIngestEventsRequest) (*api.FederationIngestEventsResponse, error) {
		ctx, principal, err := authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "push",
			federationTransportOperation("ingestFederationProjectEvents"))
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
		if err := validateFederationAdoptionBaseline(in.Body.AdoptionBaseline, in.Body.AdoptionBaselineEndEventID); err != nil {
			return nil, err
		}
		result, err := cfg.DB.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:                        in.ProjectID,
			FederationEnrollmentID:           principal.EnrollmentID,
			SpokeInstanceUID:                 principal.SpokeInstanceUID,
			BoundActor:                       principal.Actor,
			AllowSnapshotAuthorPreservation:  principal.AllowAdoptionBaseline,
			AdoptionBaseline:                 in.Body.AdoptionBaseline,
			AdoptionBaselineEndSourceEventID: in.Body.AdoptionBaselineEndEventID,
			Events:                           federationIngestEventsToDB(in.Body.Events),
		})
		if err != nil {
			return nil, federationIngestError(err)
		}
		if err := federationFailpoint("after_federation_ingest_commit_before_broadcast"); err != nil {
			return nil, api.NewError(http.StatusInternalServerError, "federation_failpoint", err.Error(), "", nil)
		}
		inserted, err := cfg.DB.EventsByUIDs(ctx, in.ProjectID, result.InsertedEventUIDs)
		if err != nil {
			return nil, internalAPIError(err)
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

func validateFederationAdoptionBaseline(adoptionBaseline string, endEventID int64) error {
	switch adoptionBaseline {
	case "":
		if endEventID != 0 {
			return api.NewError(http.StatusBadRequest, "invalid_adoption_baseline",
				"federation ingest adoption_baseline_end_event_id requires adoption_baseline", "", nil)
		}
		return nil
	case api.FederationAdoptionBaselineOpen, api.FederationAdoptionBaselineComplete:
		if endEventID <= 0 {
			return api.NewError(http.StatusBadRequest, "invalid_adoption_baseline",
				"federation ingest adoption_baseline_end_event_id must be positive", "", nil)
		}
		return nil
	default:
		return api.NewError(http.StatusBadRequest, "invalid_adoption_baseline",
			fmt.Sprintf("federation ingest adoption_baseline %q is invalid", adoptionBaseline), "", nil)
	}
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
	case errors.Is(err, ErrHostAccessDenied):
		return federationCredentialDenied()
	case errors.Is(err, db.ErrRemoteEventConflict):
		return api.NewError(http.StatusConflict, "remote_event_conflict", err.Error(), "", nil)
	case errors.Is(err, db.ErrRemoteEventHashMismatch), errors.Is(err, db.ErrFederationIngestValidation):
		return api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(http.StatusNotFound, "federation_not_found", err.Error(), "", nil)
	default:
		return internalAPIError(err)
	}
}

func federationReplicaAPIError(err error) error {
	if errors.Is(err, db.ErrFederationNotSpoke) {
		return api.NewError(
			http.StatusConflict, "not_a_spoke",
			"federation binding is not a spoke", "", nil,
		)
	}
	var serviceErr *FederationReplicaError
	if !errors.As(err, &serviceErr) {
		return internalAPIError(err)
	}
	switch {
	case errors.Is(err, errFederationReplicaCapabilityMismatch):
		return api.NewError(
			http.StatusBadRequest, "federation_capability_mismatch",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, ErrFederationReplicaInvalidInput):
		return api.NewError(
			http.StatusBadRequest, "validation",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, ErrFederationReplicaBindingConflict):
		return api.NewError(
			http.StatusConflict, "federation_binding_conflict",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, ErrFederationReplicaCredentialConflict):
		return api.NewError(
			http.StatusConflict, "federation_credential_conflict",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, ErrFederationReplicaHubUnavailable):
		return api.NewError(
			http.StatusBadGateway, "federation_hub_unavailable",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, errFederationReplicaProjectCollision):
		return api.NewError(
			http.StatusConflict, "federation_project_collision",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, errFederationReplicaProjectNotFound):
		return api.NewError(
			http.StatusNotFound, "federation_project_not_found",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, errFederationReplicaRejoinNameMismatch):
		return api.NewError(
			http.StatusConflict, "federation_rejoin_name_mismatch",
			serviceErr.message, serviceErr.hint, nil,
		)
	case errors.Is(err, errFederationReplicaIssueSyncConflict):
		return api.NewError(
			http.StatusConflict, "issue_sync_federation_conflict",
			serviceErr.message, serviceErr.hint, nil,
		)
	default:
		return internalAPIError(err)
	}
}

func federationRebindCatalogByName(
	catalog []config.CatalogDaemonConfig,
	requested string,
) (config.CatalogDaemonConfig, error) {
	name := strings.TrimSpace(requested)
	if name == "" {
		return config.CatalogDaemonConfig{}, api.NewError(
			http.StatusBadRequest, "validation", "hub_catalog is required", "", nil,
		)
	}
	var match config.CatalogDaemonConfig
	matches := 0
	for _, entry := range catalog {
		if entry.Name == name {
			match = entry
			matches++
		}
	}
	if matches != 1 {
		return config.CatalogDaemonConfig{}, api.NewError(
			http.StatusBadRequest, "validation",
			"hub_catalog must name exactly one configured daemon", "", nil,
		)
	}
	return match, nil
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

func federationStatusBody(
	ctx context.Context,
	store db.Storage,
	credentialStore config.FederationCredentialStore,
	projectID *int64,
	includeArchived bool,
) (api.FederationStatusBody, error) {
	bindings, err := federationStatusBindings(ctx, store, projectID)
	if err != nil {
		return api.FederationStatusBody{}, err
	}
	out := api.FederationStatusBody{Statuses: make([]api.FederationProjectStatus, 0, len(bindings))}
	for _, binding := range bindings {
		status, err := federationProjectStatus(ctx, store, credentialStore, binding, includeArchived)
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
			return nil, internalAPIError(err)
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
		return nil, internalAPIError(err)
	}
	return []db.FederationBinding{binding}, nil
}

func federationProjectStatus(
	ctx context.Context,
	store db.Storage,
	credentialStore config.FederationCredentialStore,
	binding db.FederationBinding,
	includeArchived bool,
) (api.FederationProjectStatus, error) {
	project, err := activeProjectByID(ctx, store, binding.ProjectID)
	if includeArchived && isProjectNotFound(err) {
		// include=archived: an archived project's binding is still real —
		// leave needs it to run the bound path (idempotent hub revoke +
		// teardown) instead of misclassifying the spoke as standalone.
		project, err = store.ProjectByID(ctx, binding.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			return api.FederationProjectStatus{}, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
		} else if err != nil {
			return api.FederationProjectStatus{}, internalAPIError(err)
		}
	} else if err != nil {
		return api.FederationProjectStatus{}, err
	}
	syncStatus, err := store.FederationSyncStatusByProject(ctx, binding.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		syncStatus = db.FederationSyncStatus{}
	} else if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	pendingPush, pendingHighWater, err := federationPendingPushStats(ctx, store, binding)
	if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	enrollments, err := federationEnrollmentCount(ctx, store, binding)
	if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	liveClaims, err := federationLiveClaimCount(ctx, store, binding.ProjectID)
	if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	pendingClaims, err := federationPendingClaimCount(ctx, store, binding.ProjectID)
	if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	activeQuarantines, err := store.ActiveFederationQuarantinesByProject(ctx, binding.ProjectID)
	if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	recentViolations, unresolvedViolationCount, err := store.UnresolvedClaimViolationsForProject(ctx, binding.ProjectID, 5)
	if err != nil {
		return api.FederationProjectStatus{}, internalAPIError(err)
	}
	var credentialMetadata config.FederationCredentialMetadata
	if binding.Role == db.FederationRoleSpoke {
		credentialMetadata = config.FederationCredentialMetadataFromStore(ctx, credentialStore, project.UID)
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
			return api.ProjectFederationBody{}, internalAPIError(err)
		}
		if resetTo > 0 {
			binding, _, err = store.RefreshProjectFederationBaseline(ctx, projectID, "federation")
			if err != nil {
				return api.ProjectFederationBody{}, internalAPIError(err)
			}
		}
	}
	through := binding.ReplayHorizonEventID
	maxSnapshot, err := store.MaxFederationBaselineEventID(ctx, projectID, binding.ReplayHorizonEventID)
	if err != nil {
		return api.ProjectFederationBody{}, internalAPIError(err)
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
	return internalAPIError(err)
}

// issueSyncFederationConflict reports a 409 for the lifecycle rule that an
// issue-synced project cannot become a federation spoke.
func issueSyncFederationConflict() error {
	return api.NewError(409, "issue_sync_federation_conflict",
		"project has issue sync enabled; run GitHub sync on the federation hub, or disable issue sync before joining this project as a spoke", "", nil)
}
