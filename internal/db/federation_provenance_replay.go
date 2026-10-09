package db

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Full backups are owner-trusted authority restores. Validate all proof bytes
// before either backend clears its target. Project merges cannot establish pins;
// enrolled federation resets verify against the receiver's existing authority.
func validateProvenanceReplay(records []ImportRecord) error {
	type keyIdentity struct{ project, key string }
	type eventIdentity struct{ project, event string }
	type entityIdentity struct{ project, kind, entity string }
	type sequenceIdentity struct {
		project         string
		epoch, sequence int64
	}
	projects := make(map[string]int64)
	projectUIDByID := make(map[int64]string)
	authorities := make(map[string]string)
	keys := make(map[keyIdentity]*RootKeyPin)
	active := make(map[string]int)
	receipts := make(map[eventIdentity]*AttributionReceipt)
	sequences := make(map[sequenceIdentity]bool)
	entities := make(map[entityIdentity]bool)
	issues := make(map[int64]*IssueExport)
	events := make(map[string]*EventExport)
	for _, record := range records {
		switch rec := record.(type) {
		case *ProjectExport:
			projects[rec.UID] = rec.ID
			projectUIDByID[rec.ID] = rec.UID
		case *IssueExport:
			issues[rec.ID] = rec
		case *EventExport:
			events[rec.UID] = rec
		case *RootKeyPin:
			id := keyIdentity{rec.ProjectUID, rec.KeyID}
			if keys[id] != nil {
				return errors.New("duplicate root key in backup")
			}
			if previous := authorities[rec.ProjectUID]; previous != "" && previous != rec.AuthorityUID {
				return errors.New("backup keys disagree on root authority")
			}
			authorities[rec.ProjectUID] = rec.AuthorityUID
			keys[id] = rec
			if !rec.Retired {
				active[rec.ProjectUID]++
			}
		case *AttributionReceipt:
			id := eventIdentity{rec.ProjectUID, rec.EventUID}
			sequence := sequenceIdentity{rec.ProjectUID, rec.ResetEpoch, rec.Sequence}
			if receipts[id] != nil || sequences[sequence] {
				return errors.New("duplicate receipt identity or sequence in backup")
			}
			receipts[id], sequences[sequence] = rec, true
		}
	}
	for _, record := range records {
		switch rec := record.(type) {
		case *IssueExport:
			if project := projectUIDByID[rec.ProjectID]; project != "" {
				entities[entityIdentity{project, "issue", rec.UID}] = true
			}
		case *CommentExport:
			issue := issues[rec.IssueID]
			if issue != nil {
				if project := projectUIDByID[issue.ProjectID]; project != "" {
					entities[entityIdentity{project, "comment", rec.UID}] = true
				}
			}
		}
	}
	for id := range keys {
		if _, ok := projects[id.project]; !ok || active[id.project] != 1 {
			return errors.New("backup must contain the root key's project and one active pin")
		}
	}
	// A retained reset is authority-bearing recovery state, so validate its
	// signed bytes before either backend starts target replacement.
	for _, record := range records {
		metadata, ok := record.(*MetaKV)
		if !ok || !strings.HasPrefix(metadata.Key, RelayResetMetadataPrefix) {
			continue
		}
		var checkpoint RelayResetCheckpoint
		if err := json.Unmarshal([]byte(metadata.Value), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("invalid backup relay reset: %w", err)
		}
		baseKey := RelayResetMetadataPrefix + checkpoint.Manifest.ProjectUID
		if metadata.Key != baseKey && metadata.Key != baseKey+"."+checkpoint.Translation.Authority.BindingUID {
			return errors.New("backup relay reset metadata changes project")
		}
		pin := keys[keyIdentity{checkpoint.Manifest.ProjectUID, checkpoint.Manifest.KeyID}]
		if pin == nil {
			return errors.New("backup relay reset has no retained root pin")
		}
		if err := VerifyRootResetManifest(*pin, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
			return fmt.Errorf("invalid backup relay reset signature: %w", err)
		}
		if err := ValidateRelayResetTranslation(checkpoint.Translation.Authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
			return fmt.Errorf("invalid backup relay reset translation: %w", err)
		}
		if _, _, err := DecodeRootResetPayload(*pin, checkpoint.Snapshot); err != nil {
			return fmt.Errorf("invalid backup relay reset payload: %w", err)
		}
	}
	seenTransitions := make(map[string]bool)
	seenPrevious := make(map[keyIdentity]bool)
	for _, record := range records {
		metadata, ok := record.(*MetaKV)
		if !ok || !strings.HasPrefix(metadata.Key, RootKeyTransitionMetadataPrefix) {
			continue
		}
		var transition RootKeyTransition
		if err := json.Unmarshal([]byte(metadata.Value), &transition, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("invalid backup root transition: %w", err)
		}
		previousID := keyIdentity{transition.Next.ProjectUID, transition.PreviousKeyID}
		previous, next := keys[previousID], keys[keyIdentity{transition.Next.ProjectUID, transition.Next.KeyID}]
		if previous == nil || next == nil || transition.Next.Retired || metadata.Key != RootKeyTransitionMetadataKey(transition) || seenTransitions[metadata.Key] || seenPrevious[previousID] || next.AuthorityUID != transition.Next.AuthorityUID {
			return errors.New("duplicate, orphan or mismatched root transition in backup")
		}
		if err := VerifyRootKeyTransition(*previous, transition); err != nil {
			return fmt.Errorf("invalid backup root transition: %w", err)
		}
		seenTransitions[metadata.Key], seenPrevious[previousID] = true, true
	}
	for id, receipt := range receipts {
		pin := keys[keyIdentity{id.project, receipt.KeyID}]
		if pin == nil {
			return errors.New("backup receipt has no root pin")
		}
		if err := VerifyRootReceipt(*pin, *receipt); err != nil {
			return fmt.Errorf("invalid backup receipt: %w", err)
		}
		if event := events[id.event]; event != nil {
			if event.ProjectID != projects[id.project] || event.ContentHash != receipt.ContentHash || event.Actor != receipt.SourceActor {
				return ErrRemoteEventHashMismatch
			}
			handle, _, err := EventCreationProvenance(RemoteEvent{ProjectUID: id.project, EventUID: id.event, Type: event.Type, IssueUID: event.IssueUID, Payload: event.Payload})
			if err != nil {
				return err
			}
			if handle != receipt.Teammate {
				return errors.New("backup receipt teammate disagrees with source")
			}
		}
	}
	seenRefs := make(map[entityIdentity]bool)
	for _, record := range records {
		ref, ok := record.(*EntityProvenance)
		if !ok {
			continue
		}
		id := entityIdentity{ref.ProjectUID, ref.Kind, ref.EntityUID}
		if seenRefs[id] || !entities[id] || receipts[eventIdentity{ref.ProjectUID, ref.EventUID}] == nil {
			return errors.New("duplicate or orphan creation provenance in backup")
		}
		seenRefs[id] = true
		if event := events[ref.EventUID]; event != nil {
			_, refs, err := EventCreationProvenance(RemoteEvent{ProjectUID: ref.ProjectUID, EventUID: ref.EventUID, Type: event.Type, IssueUID: event.IssueUID, Payload: event.Payload})
			if err != nil {
				return err
			}
			found := slices.Contains(refs, *ref)
			if !found {
				return errors.New("creation provenance disagrees with source event")
			}
		}
	}
	return nil
}
