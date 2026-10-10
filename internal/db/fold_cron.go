package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronDefinitionEvent is a complete portable document. Snapshot envelopes
// carry the original winner separately; their new envelope clock is not an edit.
type CronDefinitionEvent struct {
	UID                string             `json:"uid"`
	ProjectUID         string             `json:"project_uid"`
	Name               string             `json:"name"`
	Definition         jsontext.Value     `json:"definition"`
	Author             string             `json:"author"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	DeletedAt          *time.Time         `json:"deleted_at,omitempty"`
	DefinitionEventUID string             `json:"definition_event_uid,omitempty"`
	DefinitionHLC      *CronDefinitionHLC `json:"definition_hlc,omitempty"`
}

type foldCronAuthorCandidate struct {
	author   string
	snapshot bool
	// fallback marks an author taken from an edit. Edits never establish
	// authorship; their author only stands in until a create or snapshot
	// for the definition is folded.
	fallback bool
	clock    FoldClock
}

func cronDefinitionClock(value CronDefinition) FoldClock {
	return FoldClock{HLCPhysicalMS: value.DefinitionHLC.PhysicalMS, HLCCounter: value.DefinitionHLC.Counter, OriginInstanceUID: value.DefinitionHLC.OriginInstanceUID, EventUID: value.DefinitionEventUID}
}

// Authorship comes from the event that created a definition, or from an
// adoption snapshot, which carries canonical authorship separately from the
// original definition clock. Whole-document edits, including backdated ones,
// cannot replace that author from their payload.
func (p *FoldProjection) cronDefinitionAuthor(key string, event FoldEvent, definition CronDefinition) string {
	candidate := foldCronAuthorCandidate{author: definition.Author, clock: cronDefinitionClock(definition)}
	candidate.fallback = !strings.HasSuffix(event.Type, ".created") && !strings.HasSuffix(event.Type, ".snapshot")
	if strings.HasSuffix(event.Type, ".snapshot") {
		candidate.snapshot = true
		candidate.clock = FoldClock{
			HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter,
			OriginInstanceUID: event.OriginInstanceUID, EventUID: event.UID,
		}
	}
	if p.cronAuthors == nil {
		p.cronAuthors = make(map[string]foldCronAuthorCandidate)
	}
	current, exists := p.cronAuthors[key]
	useCandidate := !exists
	if exists {
		switch {
		case candidate.fallback != current.fallback:
			useCandidate = current.fallback
		case candidate.fallback:
			useCandidate = false
		case candidate.snapshot != current.snapshot:
			useCandidate = candidate.snapshot
		case candidate.snapshot:
			useCandidate = compareClock(candidate.clock, current.clock) > 0
		default:
			useCandidate = compareClock(candidate.clock, current.clock) < 0
		}
	}
	if useCandidate {
		p.cronAuthors[key] = candidate
	}
	return p.cronAuthors[key].author
}

func parseCronDefinitionEvent(e FoldEvent) (CronDefinitionEvent, CronDefinition, error) {
	var in CronDefinitionEvent
	if err := json.Unmarshal(e.Payload, &in, json.RejectUnknownMembers(true)); err != nil {
		return in, CronDefinition{}, err
	}
	clock := CronDefinitionHLC{Version: 1, PhysicalMS: e.HLCPhysicalMS, Counter: e.HLCCounter, OriginInstanceUID: e.OriginInstanceUID}
	winner := e.UID
	if strings.HasSuffix(e.Type, ".snapshot") {
		if in.DefinitionHLC == nil || !cronUID(in.DefinitionEventUID) {
			return in, CronDefinition{}, fmt.Errorf("snapshot requires original definition clock")
		}
		clock = *in.DefinitionHLC
		winner = in.DefinitionEventUID
	} else if in.DefinitionHLC != nil || in.DefinitionEventUID != "" {
		return in, CronDefinition{}, fmt.Errorf("definition edit cannot override envelope clock")
	}
	value := CronDefinition{ID: 1, ProjectID: 1, UID: in.UID, Name: in.Name, Author: in.Author, DefinitionEventUID: winner, DefinitionHLC: clock, Revision: 1, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt, DeletedAt: in.DeletedAt}
	if err := validateCronDefinition(value); err != nil {
		return in, value, err
	}
	if !cronUID(in.ProjectUID) || (e.ProjectUID != "" && in.ProjectUID != e.ProjectUID) {
		return in, value, fmt.Errorf("cron project identity mismatch")
	}
	if strings.HasSuffix(e.Type, ".deleted") && in.DeletedAt == nil {
		return in, value, fmt.Errorf("deletion requires tombstone")
	}
	if (strings.HasSuffix(e.Type, ".created") || strings.HasSuffix(e.Type, ".restored") || strings.HasSuffix(e.Type, ".updated")) && in.DeletedAt != nil {
		return in, value, fmt.Errorf("live definition contains tombstone")
	}
	value.ID = 0
	value.ProjectID = 0
	return in, value, nil
}

func (p *FoldProjection) applyCronDefinition(e FoldEvent) {
	in, value, err := parseCronDefinitionEvent(e)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("invalid cron event %s: %v", e.UID, err))
		return
	}
	if strings.HasPrefix(e.Type, "cron.job.") {
		definition, err := cron.DecodeJob(in.Definition)
		if err != nil {
			p.Warnings = append(p.Warnings, err.Error())
			return
		}
		value.Author = p.cronDefinitionAuthor("job:"+in.UID, e, value)
		current, exists := p.CronJobs[in.UID]
		if !exists || compareClock(cronDefinitionClock(value), cronDefinitionClock(current.CronDefinition)) > 0 {
			p.CronJobs[in.UID] = FoldCronJob{CronDefinition: value, Definition: definition, ProjectUID: in.ProjectUID}
		} else {
			current.Author = value.Author
			p.CronJobs[in.UID] = current
		}
	} else {
		definition, err := cron.DecodeWorkflow(in.Definition)
		if err != nil {
			p.Warnings = append(p.Warnings, err.Error())
			return
		}
		value.Author = p.cronDefinitionAuthor("workflow:"+in.UID, e, value)
		current, exists := p.CronWorkflows[in.UID]
		if !exists || compareClock(cronDefinitionClock(value), cronDefinitionClock(current.CronDefinition)) > 0 {
			p.CronWorkflows[in.UID] = FoldCronWorkflow{CronDefinition: value, Definition: definition, ProjectUID: in.ProjectUID}
		} else {
			current.Author = value.Author
			p.CronWorkflows[in.UID] = current
		}
	}
}

// ValidateCronFederationEvent checks a cron event a spoke pushes to its hub:
// the envelope, a strict decode, and today's rules for a live definition.
// Referenced definitions may be historical and are materialized separately.
func ValidateCronFederationEvent(event RemoteEvent) error {
	return validateCronEvent(event, true)
}

// ValidateAcceptedCronEvent checks cron history that a hub or a backup already
// accepted: the envelope and a strict decode, without re-applying today's
// rules to documents written under older ones. A pull or a restore must not
// fail because a rule was tightened after the history was written.
func ValidateAcceptedCronEvent(event RemoteEvent) error {
	return validateCronEvent(event, false)
}

func validateCronEvent(event RemoteEvent, applyRules bool) error {
	if !cronUID(event.EventUID) || !cronUID(event.OriginInstanceUID) || !cronUID(event.ProjectUID) || strings.TrimSpace(event.Actor) == "" || event.HLCPhysicalMS <= 0 || event.HLCCounter < 0 {
		return fmt.Errorf("%w: invalid cron envelope", ErrFederationIngestValidation)
	}
	if event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot" {
		_, err := parseCronRunObservation(FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type})
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
		return nil
	}

	if !isCronDefinitionEvent(event.Type) {
		return fmt.Errorf("%w: unsupported cron event %s", ErrFederationIngestValidation, event.Type)
	}
	in, _, err := parseCronDefinitionEvent(FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type})
	if err == nil {
		err = validateCronDefinitionDocument(strings.HasPrefix(event.Type, "cron.job."), in.DeletedAt != nil || !applyRules, in.Definition)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
	}
	return nil
}

// validateCronDefinitionDocument applies the current rules to a document
// unless decodeOnly is set. A tombstone keeps whatever document it had, so the
// deletion of an older definition that today's rules reject still propagates;
// it only has to decode.
func validateCronDefinitionDocument(job, decodeOnly bool, document []byte) error {
	var err error
	switch {
	case job && decodeOnly:
		_, err = cron.DecodeJob(document)
	case job:
		_, err = cron.ParseJob(document)
	case decodeOnly:
		_, err = cron.DecodeWorkflow(document)
	default:
		_, err = cron.ParseWorkflow(document)
	}
	return err
}
