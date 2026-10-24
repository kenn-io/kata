package db

import (
	"errors"
	"reflect"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

func scopedIndexPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// ScopedRecordIndexes is the one index constructor for restore and native
// projection writes. Removal hints retain historical lookup information only;
// they never establish current endpoint membership or generation ownership.
func ScopedRecordIndexes(effect ScopedEffect, prior *ScopedRecordExport) (subjectUID, peerProjectUID, peerUID *string, err error) {
	project, err := ScopedEffectProjectUID(effect)
	if err != nil {
		return nil, nil, nil, err
	}
	if prior != nil && (prior.Collection != effect.Collection || prior.RecordKey != effect.Key || prior.ProjectUID != project) {
		return nil, nil, nil, errors.New("scoped prior record identity differs")
	}
	if effect.Action == "remove" {
		parts := strings.SplitN(effect.Key, ":", 4)
		switch effect.Collection {
		case "issues", "labels":
			subjectUID = scopedIndexPointer(parts[1])
		case "definitions", "provenance":
			subjectUID = scopedIndexPointer(parts[2])
		case "references":
			subjectUID = scopedIndexPointer(parts[1])
			if subjectUID == nil {
				subjectUID = scopedIndexPointer(parts[2])
			}
		}
		if prior != nil {
			if !scopedOptionalUID(prior.SubjectUID) || !scopedOptionalUID(prior.PeerProjectUID) || !scopedOptionalUID(prior.PeerUID) || (prior.PeerUID == nil) != (prior.PeerProjectUID == nil) {
				return nil, nil, nil, errors.New("invalid prior historical indexes")
			}
			if effect.Collection == "comments" || effect.Collection == "artifacts" {
				subjectUID = prior.SubjectUID
			} else if !reflect.DeepEqual(subjectUID, prior.SubjectUID) {
				return nil, nil, nil, errors.New("prior subject differs from stable record key")
			}
			if effect.Collection == "references" {
				peerProjectUID, peerUID = prior.PeerProjectUID, prior.PeerUID
			} else if prior.PeerProjectUID != nil || prior.PeerUID != nil {
				return nil, nil, nil, errors.New("non-reference prior contains peer indexes")
			}
		}
		return subjectUID, peerProjectUID, peerUID, nil
	}
	var subject, peerProject, peer string
	switch effect.Collection {
	case "projects":
	case "issues":
		subject = effect.Issue.UID
	case "comments":
		subject = effect.Comment.IssueUID
	case "labels":
		subject = effect.Label.IssueUID
	case "definitions":
		subject = effect.Definition.UID
	case "references":
		r := effect.Reference
		subject = r.Subject.IssueUID
		if subject == "" {
			subject = r.SubjectEntityUID
		}
		if r.Close != nil {
			peerProject, peer = r.Close.PeerProjectUID, r.Close.PeerIssueUID
		} else if r.Present && r.Peer != nil {
			peerProject, peer = r.Peer.ProjectUID, r.Peer.IssueUID
			if peer == "" {
				peer = r.PeerEntityUID
			}
		}
	case "provenance":
		subject = effect.Provenance.EntityUID
	case "artifacts":
		subject = effect.Artifact.IssueUID
	}
	if effect.Collection != "projects" && !uid.Valid(subject) || (peerProject == "") != (peer == "") || peer != "" && (!uid.Valid(peer) || !uid.Valid(peerProject)) {
		return nil, nil, nil, errors.New("invalid scoped record endpoint indexes")
	}
	return scopedIndexPointer(subject), scopedIndexPointer(peerProject), scopedIndexPointer(peer), nil
}

// ValidateScopedRecordEnvelope validates exact retained bytes and the closed
// effect/index wrapper. Signatures and expected owner/fences are separate duties.
func ValidateScopedRecordEnvelope(record ScopedRecordExport) error {
	if !scopedIDs(record.ViewUID, record.ProjectUID) || record.Generation <= 0 || record.VisibleRevision <= 0 || !scopedDigest(record.Digest) || ScopedOfferedDigest(record.RecordBytes) != record.Digest || !scopedOptionalUID(record.SubjectUID) || !scopedOptionalUID(record.PeerProjectUID) || !scopedOptionalUID(record.PeerUID) || (record.PeerProjectUID == nil) != (record.PeerUID == nil) {
		return errors.New("invalid scoped record envelope")
	}
	var effect ScopedEffect
	if err := scopedJSON(record.RecordBytes, &effect); err != nil {
		return err
	}
	project, err := ScopedEffectProjectUID(effect)
	if err != nil {
		return err
	}
	if effect.Collection != record.Collection || effect.Key != record.RecordKey || effect.Revision != record.VisibleRevision || (effect.Action == "replace") != record.Present || project != record.ProjectUID {
		return errors.New("scoped record wrapper differs from retained effect")
	}
	// An isolated tombstone cannot prove unavailable prior history. Feed only its
	// structural historical hints through the same constructor; recoverable key
	// identities and NULL pairing still remain mandatory.
	var prior *ScopedRecordExport
	if effect.Action == "remove" {
		prior = &record
	}
	subject, peerProject, peer, err := ScopedRecordIndexes(effect, prior)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(subject, record.SubjectUID) || !reflect.DeepEqual(peerProject, record.PeerProjectUID) || !reflect.DeepEqual(peer, record.PeerUID) {
		return errors.New("scoped record indexes differ from typed effect")
	}
	return nil
}
