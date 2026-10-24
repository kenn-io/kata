package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
)

// ScopedCheckpointRecordEffects constructs the closed signed baseline inventory.
// It does not verify signatures or infer native read-model state. Callers verify
// the checkpoint against independently trusted context before using these facts.
// Nested reference revisions/endorsements and caller input remain untouched.
func ScopedCheckpointRecordEffects(checkpoint ScopedCheckpoint) ([]ScopedEffect, error) {
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	return ScopedCollectionRecordEffects(checkpoint.Collections, checkpoint.Cut.Sequence)
}

// ScopedCollectionRecordEffects maps closed current collections at an explicit
// positive revision. It asserts no checkpoint, signature or authorization.
func ScopedCollectionRecordEffects(c ScopedCollections, revision int64) ([]ScopedEffect, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if revision <= 0 {
		return nil, errors.New("record revision must be positive")
	}
	effects := []ScopedEffect{}
	add := func(e ScopedEffect) { e.Action = "replace"; e.Revision = revision; effects = append(effects, e) }
	for _, v := range c.Projects {
		add(ScopedEffect{Collection: "projects", Key: v.UID, Project: &v})
	}
	for _, v := range c.Issues {
		add(ScopedEffect{Collection: "issues", Key: v.ProjectUID + ":" + v.UID, Issue: &v})
	}
	for _, v := range c.Comments {
		add(ScopedEffect{Collection: "comments", Key: v.ProjectUID + ":" + v.UID, Comment: &v})
	}
	for _, v := range c.Labels {
		add(ScopedEffect{Collection: "labels", Key: v.ProjectUID + ":" + v.IssueUID + ":" + v.Label, Label: &v})
	}
	for _, v := range c.Definitions {
		key, err := ScopedDefinitionKey(v.ProjectUID, v.Kind, v.UID)
		if err != nil {
			return nil, err
		}
		add(ScopedEffect{Collection: "definitions", Key: key, Definition: &v})
	}
	for _, v := range c.References {
		add(ScopedEffect{Collection: "references", Key: v.Subject.ProjectUID + ":" + v.Subject.IssueUID + ":" + v.SubjectEntityUID + ":" + v.Slot, Reference: &v})
	}
	for _, v := range c.Provenance {
		add(ScopedEffect{Collection: "provenance", Key: v.ProjectUID + ":" + v.Kind + ":" + v.EntityUID, Provenance: &v})
	}
	for _, v := range c.Artifacts {
		add(ScopedEffect{Collection: "artifacts", Key: v.ProjectUID + ":" + v.Digest, Artifact: &v})
	}
	for _, effect := range effects {
		if _, err := ScopedEffectProjectUID(effect); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(effects, scopedRecordEffectOrder)
	return effects, nil
}
func scopedRecordEffectOrder(a, b ScopedEffect) int {
	if order := strings.Compare(a.Collection, b.Collection); order != 0 {
		return order
	}
	return strings.Compare(a.Key, b.Key)
}
func scopedRecordEffectIdentity(e ScopedEffect) string { return e.Collection + "/" + e.Key }

// ScopedRecordInventory applies only already-authenticated typed baseline/batch
// assertions in contiguous signed cut order. This is structural correspondence,
// not a canonical conflict fold, authorization decision or signature verifier.
// Evidenced pre-baseline removals are optional historical tombstones only when
// the current authoritative inventory remains absent at that stable key.
func ScopedRecordInventory(checkpoint ScopedCheckpoint, batches []ScopedBatch) ([]ScopedEffect, error) {
	baseline, err := ScopedCheckpointRecordEffects(checkpoint)
	if err != nil {
		return nil, err
	}
	current := map[string]ScopedEffect{}
	for _, effect := range baseline {
		current[scopedRecordEffectIdentity(effect)] = effect
	}
	ordered := append([]ScopedBatch{}, batches...)
	slices.SortFunc(ordered, func(a, b ScopedBatch) int {
		if a.Cut.Sequence < b.Cut.Sequence {
			return -1
		}
		if a.Cut.Sequence > b.Cut.Sequence {
			return 1
		}
		return 0
	})
	sequence, previous := checkpoint.Cut.Sequence, checkpoint.Cut.Digest
	oldRemovals := map[string]ScopedEffect{}
	var last int64 = -1
	for _, batch := range ordered {
		if err := batch.Validate(); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(batch.View, checkpoint.View) || batch.Cut.Sequence == last {
			return nil, errors.New("record inventory batch changes descriptor or duplicates cut")
		}
		last = batch.Cut.Sequence
		for _, effect := range batch.Effects {
			if _, err := ScopedEffectProjectUID(effect); err != nil {
				return nil, err
			}
		}
		if batch.Cut.Sequence <= checkpoint.Cut.Sequence {
			for _, effect := range batch.Effects {
				if effect.Action == "remove" {
					oldRemovals[scopedRecordEffectIdentity(effect)] = effect
				}
			}
			continue
		}
		if sequence == int64(^uint64(0)>>1) || batch.Cut.Sequence != sequence+1 || batch.PreviousDigest != previous {
			return nil, errors.New("record inventory missing contiguous signed batch chain")
		}
		for _, effect := range batch.Effects {
			current[scopedRecordEffectIdentity(effect)] = effect
		}
		sequence, previous = batch.Cut.Sequence, batch.Cut.Digest
	}
	for identity, effect := range oldRemovals {
		if _, present := current[identity]; !present {
			current[identity] = effect
		}
	}
	result := make([]ScopedEffect, 0, len(current))
	for _, effect := range current {
		result = append(result, effect)
	}
	slices.SortFunc(result, scopedRecordEffectOrder)
	return result, nil
}

// Canonical comparison is only corroboration of derived signed assertions.
// Exact retained row/signed/body bytes and their raw digests remain separate.
func scopedRecordAssertionBytes(effect ScopedEffect) ([]byte, error) {
	raw, err := json.Marshal(effect)
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	return canonical, nil
}
