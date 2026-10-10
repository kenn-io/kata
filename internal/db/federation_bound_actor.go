package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// FederationAttribution reports the attribution a hub already stores, so a
// spoke's pushed events cannot rewrite attribution its bound actor did not
// create.
type FederationAttribution interface {
	// RunActor returns the stored actor of a run in the ingesting project.
	RunActor(ctx context.Context, uid string) (actor string, known bool, err error)
	// DefinitionAuthor returns the stored author of a job or workflow in the
	// ingesting project. kind is "job" or "workflow".
	DefinitionAuthor(ctx context.Context, kind, uid string) (author string, known bool, err error)
}

// FederationBoundActorCheck carries one ingest batch's attribution policy.
type FederationBoundActorCheck struct {
	BoundActor string
	// AllowSnapshotAuthorPreservation is the approved adoption grant: baseline
	// snapshots keep their historical authors.
	AllowSnapshotAuthorPreservation bool
	// AdoptionBaseline is true while an approved adoption baseline is open,
	// with or without the author grant.
	AdoptionBaseline bool
	Known            FederationAttribution
}

// ValidateFederationBoundActorPayload enforces bound-actor attribution on the
// payload of one fresh pushed event. The envelope actor is checked separately.
func ValidateFederationBoundActorPayload(ctx context.Context, ev RemoteEvent, check FederationBoundActorCheck) error {
	if err := ValidateFederationEntries(ev.Type, ev.EventUID, ev.Payload); err != nil {
		return err
	}
	boundActor := strings.TrimSpace(check.BoundActor)
	if boundActor == "" {
		return nil
	}
	switch {
	case ev.Type == "cron.run.snapshot" && check.AdoptionBaseline:
		// Run actors are observation history, not authorship. An approved
		// adoption baseline preserves them with or without the author grant.
		return nil
	case ev.Type == "cron.run.observed" || ev.Type == "cron.run.snapshot":
		return validateFederationRunActor(ctx, ev, boundActor, check.Known)
	case isCronDefinitionEvent(ev.Type):
		return validateFederationDefinitionAuthor(ctx, ev, check)
	}
	switch ev.Type {
	case "issue.snapshot":
		if check.AllowSnapshotAuthorPreservation {
			return nil
		}
		return validateFederationIssueAuthors(ev, boundActor)
	case "issue.created":
		return validateFederationIssueAuthors(ev, boundActor)
	case "issue.commented":
		return validateFederationPayloadAuthor(ev, boundActor)
	default:
		return nil
	}
}

// A run's actor is its immutable identity. A pushed observation either starts
// a run as the bound actor or continues a run the hub already holds under the
// same actor, such as one started before the spoke enabled push.
func validateFederationRunActor(ctx context.Context, ev RemoteEvent, boundActor string, known FederationAttribution) error {
	var run CronRunObservation
	if err := json.Unmarshal(ev.Payload, &run); err != nil {
		return fmt.Errorf("%w: event %s run payload is invalid JSON", ErrFederationIngestValidation, ev.EventUID)
	}
	if run.Actor == boundActor {
		return nil
	}
	stored, ok, err := known.RunActor(ctx, strings.ToUpper(run.UID))
	if err != nil {
		return err
	}
	if !ok || stored != run.Actor {
		return fmt.Errorf("%w: run actor differs from bound actor", ErrFederationIngestValidation)
	}
	return nil
}

// Definition authorship comes only from the event that created a definition
// or from an adoption snapshot; folds ignore the author of later edits. So a
// spoke cannot replay a create or snapshot for a definition the hub already
// holds outside an approved adoption baseline, and an edit that introduces a
// definition the hub has not seen must carry the bound actor. An edit to a
// known definition may still carry a pre-adoption author: the spoke can edit
// before it receives the hub's canonical adoption snapshot.
func validateFederationDefinitionAuthor(ctx context.Context, ev RemoteEvent, check FederationBoundActorCheck) error {
	kind := "workflow"
	if strings.HasPrefix(ev.Type, "cron.job.") {
		kind = "job"
	}
	payload := PayloadMap(ev.Payload)
	definitionUID, _ := StringValue(payload["uid"])
	_, known, err := check.Known.DefinitionAuthor(ctx, kind, strings.ToUpper(definitionUID))
	if err != nil {
		return err
	}
	snapshot := strings.HasSuffix(ev.Type, ".snapshot")
	created := strings.HasSuffix(ev.Type, ".created")
	if (snapshot || created) && known && !check.AdoptionBaseline {
		return fmt.Errorf("%w: event %s %s targets existing %s %s",
			ErrFederationIngestValidation, ev.EventUID, ev.Type, kind, definitionUID)
	}
	if snapshot && check.AllowSnapshotAuthorPreservation {
		return nil
	}
	if known && !snapshot && !created {
		return nil
	}
	return validateFederationPayloadAuthorIs(ev, strings.TrimSpace(check.BoundActor))
}

func validateFederationIssueAuthors(ev RemoteEvent, boundActor string) error {
	if err := validateFederationPayloadAuthor(ev, boundActor); err != nil {
		return err
	}
	if err := validateFederationPayloadCommentAuthors(ev, boundActor); err != nil {
		return err
	}
	return validateFederationPayloadLinkAuthors(ev, boundActor)
}

func validateFederationPayloadAuthor(ev RemoteEvent, boundActor string) error {
	return validateFederationPayloadAuthorIs(ev, boundActor)
}

func validateFederationPayloadAuthorIs(ev RemoteEvent, expected string) error {
	author, ok := StringValue(PayloadMap(ev.Payload)["author"])
	if !ok || strings.TrimSpace(author) != expected {
		return fmt.Errorf("%w: event %s %s payload author %q does not match bound actor",
			ErrFederationIngestValidation, ev.EventUID, ev.Type, author)
	}
	return nil
}

func validateFederationPayloadCommentAuthors(ev RemoteEvent, boundActor string) error {
	var payload struct {
		Comments []struct {
			Author string `json:"author"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("%w: event %s %s payload is invalid JSON",
			ErrFederationIngestValidation, ev.EventUID, ev.Type)
	}
	for _, comment := range payload.Comments {
		if strings.TrimSpace(comment.Author) != boundActor {
			return fmt.Errorf("%w: event %s %s comment payload author %q does not match bound actor",
				ErrFederationIngestValidation, ev.EventUID, ev.Type, comment.Author)
		}
	}
	return nil
}

func validateFederationPayloadLinkAuthors(ev RemoteEvent, boundActor string) error {
	var payload struct {
		Links []struct {
			Author string `json:"author"`
		} `json:"links"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("%w: event %s %s payload is invalid JSON",
			ErrFederationIngestValidation, ev.EventUID, ev.Type)
	}
	for _, link := range payload.Links {
		author := strings.TrimSpace(link.Author)
		if author != "" && author != boundActor {
			return fmt.Errorf("%w: event %s %s link payload author %q does not match bound actor",
				ErrFederationIngestValidation, ev.EventUID, ev.Type, link.Author)
		}
	}
	return nil
}

// FederationAttributionSQL answers FederationAttribution from one project's
// rows inside the ingest transaction. Both backends share the bound queries.
type FederationAttributionSQL struct {
	Tx        *sql.Tx
	ProjectID int64
}

// RunActor implements FederationAttribution.
func (f FederationAttributionSQL) RunActor(ctx context.Context, uid string) (string, bool, error) {
	return f.lookup(ctx, `SELECT actor FROM cron_runs WHERE uid=$1 AND project_id=$2`, uid)
}

// DefinitionAuthor implements FederationAttribution.
func (f FederationAttributionSQL) DefinitionAuthor(ctx context.Context, kind, uid string) (string, bool, error) {
	table := "cron_workflows"
	if kind == "job" {
		table = "cron_jobs"
	}
	//nolint:gosec // The table is one of two fixed names; values are bound.
	return f.lookup(ctx, "SELECT author FROM "+table+" WHERE uid=$1 AND project_id=$2", uid)
}

func (f FederationAttributionSQL) lookup(ctx context.Context, query, uid string) (string, bool, error) {
	var value string
	err := f.Tx.QueryRowContext(ctx, query, uid, f.ProjectID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read stored federation attribution: %w", err)
	}
	return value, true, nil
}
