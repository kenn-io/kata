package dbtest

import (
	"bytes"
	"context"
	"database/sql"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

// oldPeerMetadata freezes the pre-tally from/to merge semantics from 236600256.
// It shares no fold or merge code with production and retains raw integer values.
func oldPeerMetadata(t *testing.T, events []db.RemoteEvent) map[string]jsontext.Value {
	t.Helper()
	events = append([]db.RemoteEvent(nil), events...)
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.HLCPhysicalMS != b.HLCPhysicalMS {
			return a.HLCPhysicalMS < b.HLCPhysicalMS
		}
		if a.HLCCounter != b.HLCCounter {
			return a.HLCCounter < b.HLCCounter
		}
		if a.OriginInstanceUID != b.OriginInstanceUID {
			return a.OriginInstanceUID < b.OriginInstanceUID
		}
		return a.EventUID < b.EventUID
	})
	current := map[string]jsontext.Value{}
	for _, event := range events {
		if event.Type != "project.federation_enabled" && event.Type != "project.metadata_updated" {
			continue
		}
		var payload struct {
			Metadata map[string]jsontext.Value `json:"metadata"`
			Diff     map[string]struct {
				From jsontext.Value `json:"from"`
				To   jsontext.Value `json:"to"`
			} `json:"diff"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		switch event.Type {
		case "project.federation_enabled":
			if payload.Metadata != nil {
				current = payload.Metadata
			}
		case "project.metadata_updated":
			for key, d := range payload.Diff {
				if len(d.To) == 0 || string(d.To) == "null" {
					delete(current, key)
					continue
				}
				current[key] = oldPeerValue(t, current[key], d.From, d.To)
			}
		}
	}
	return current
}

func oldPeerValue(t *testing.T, current, from, to jsontext.Value) jsontext.Value {
	t.Helper()
	object := func(raw jsontext.Value) (map[string]jsontext.Value, bool) {
		var obj map[string]jsontext.Value
		err := json.Unmarshal(raw, &obj)
		return obj, err == nil && obj != nil
	}
	cur, curObject := object(current)
	before, fromObject := object(from)
	after, toObject := object(to)
	if !toObject || (!fromObject && len(from) > 0 && string(from) != "null") || (!curObject && len(current) > 0 && string(current) != "null") {
		return to
	}
	if cur == nil {
		cur = map[string]jsontext.Value{}
	}
	for key, prior := range before {
		next, ok := after[key]
		if !ok || string(next) == "null" {
			delete(cur, key)
			continue
		}
		_, priorObject := object(prior)
		_, nextObject := object(next)
		if priorObject && nextObject {
			cur[key] = oldPeerValue(t, cur[key], prior, next)
		} else if !bytes.Equal(oldPeerJSON(t, prior), oldPeerJSON(t, next)) {
			cur[key] = next
		}
	}
	for key, next := range after {
		if _, seen := before[key]; seen {
			continue
		}
		if string(next) == "null" {
			delete(cur, key)
			continue
		}
		cur[key] = oldPeerValue(t, cur[key], nil, next)
	}
	out, err := json.Marshal(cur, json.Deterministic(true))
	require.NoError(t, err)
	return out
}

// Keep the legacy merge oracle independent of production metadata comparison.
// UseNumber preserves one-count differences above 2^53.
func oldPeerJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	decoder := jsonv1.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	require.NoError(t, decoder.Decode(&value))
	out, err := jsonv1.Marshal(value)
	require.NoError(t, err)
	return out
}

// Contract: a root reset replaces the complete reserved namespace even on a
// peer whose ordinary metadata fold still recursively merges object values.
// SQL deliberately supplies divergent retained history and current inventory:
// a matching update event could mask a missing authoritative reset overlay.
func checkCronTallyResetInventory(t *testing.T, store db.Storage) error {
	// The legacy fold applies from/to changes rather than overwriting unchanged
	// nested fields. Keep this independent oracle check: neither a project-path
	// regression nor an oracle regression is subsumed by the issue fold test.
	ordinary := []db.RemoteEvent{
		{ProjectUID: "example-project", Type: "project.federation_enabled", HLCPhysicalMS: 1, Payload: []byte(`{"metadata":{"profile":{"label":"root","count":1,"removed":true}}}`)},
		{ProjectUID: "example-project", Type: "project.metadata_updated", HLCPhysicalMS: 2, Payload: []byte(`{"diff":{"profile":{"from":{"label":"old","count":1,"removed":true},"to":{"label":"old","count":2}}}}`)},
	}
	folded := db.FoldEvents(remoteEventsToFold(ordinary))
	legacy, err := json.Marshal(oldPeerMetadata(t, ordinary))
	require.NoError(t, err)
	require.JSONEq(t, `{"profile":{"label":"root","count":2}}`, string(folded.ProjectMetadata["example-project"]))
	require.Equal(t, metadata.NormalizeJSON(folded.ProjectMetadata["example-project"]), metadata.NormalizeJSON(legacy))
	ctx := t.Context()
	for i, mode := range []string{"removed kind", "removed key", "empty inventory"} {
		fixture := newRelayLinkBoundaryFixture(t, store, fmt.Sprintf("tally-reset-%d", i))
		a, err := uid.New()
		require.NoError(t, err)
		b, err := uid.New()
		require.NoError(t, err)
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		entry := db.CronTallyEntry{Kind: "job", UID: a, CoverageStartedAt: at, LastPrunedAt: at, OldestCreatedAt: at, NewestCreatedAt: at, Counts: db.CronTallyCounts{Succeeded: 9007199254740993}}
		workflow := entry
		workflow.Kind = "workflow"
		removed := entry
		removed.UID = b
		prior := map[string]any{db.CronTallyPrefix + a: db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": entry, "workflow": workflow}}, db.CronTallyPrefix + b: db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": removed}}}
		fields := map[string]any{"ordinary": "current"}
		if mode != "empty inventory" {
			groups := map[string]db.CronTallyEntry{"job": entry}
			if mode == "removed key" {
				groups["workflow"] = workflow
			}
			fields[db.CronTallyPrefix+a] = db.CronTallyValue{Version: 1, Groups: groups}
		}
		diff := map[string]any{}
		for k, v := range prior {
			diff[k] = map[string]any{"from": nil, "to": v}
		}
		payload, err := json.Marshal(map[string]any{"diff": diff})
		require.NoError(t, err)
		original := newRemoteEvent(t, fixture.project, nil, "project.metadata_updated", "operator", store.InstanceUID(), at.UnixMilli(), payload)
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, err = q.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, original.EventUID, original.OriginInstanceUID, fixture.project.ID, fixture.project.Name, original.Type, original.Actor, string(original.Payload), original.HLCPhysicalMS, original.HLCCounter, original.ContentHash, original.CreatedAt.Format(db.EventTimestampFormat))
		require.NoError(t, err)
		current, err := json.Marshal(fields)
		require.NoError(t, err)
		_, err = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(current), fixture.project.ID)
		require.NoError(t, err)
		peer, err := uid.New()
		require.NoError(t, err)
		grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion})
		require.NoError(t, err)
		drainRelayGrant(t, store, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch)
		measured, err := store.(db.RelayCheckpointMeasurer).RelayCheckpointSize(ctx, fixture.project.ID)
		require.NoError(t, err)
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, fixture.signer)
		require.NoError(t, err)
		require.GreaterOrEqual(t, measured.Bytes, db.RootResetSnapshotBytes(checkpoint.Snapshot), "status estimate must bound the extra clear event and its receipt")
		events, _, err := db.DecodeRootResetPayload(mustRootPin(t, store, fixture.project.UID), checkpoint.Snapshot)
		require.NoError(t, err)
		found := false
		for _, event := range events {
			if event.EventUID == original.EventUID {
				require.Equal(t, original, event)
				found = true
			}
		}
		require.True(t, found, "source history remains byte-identical")
		upgraded := db.FoldEvents(remoteEventsToFold(events))
		require.Equal(t, metadata.NormalizeJSON(current), metadata.NormalizeJSON(upgraded.ProjectMetadata[fixture.project.UID]))
		old, err := json.Marshal(oldPeerMetadata(t, events))
		require.NoError(t, err)
		require.Equal(t, metadata.NormalizeJSON(current), metadata.NormalizeJSON(old), mode)
	}
	return nil
}

func remoteEventsToFold(events []db.RemoteEvent) []db.FoldEvent {
	out := make([]db.FoldEvent, 0, len(events))
	for _, e := range events {
		out = append(out, db.FoldEvent{UID: e.EventUID, ProjectUID: e.ProjectUID, OriginInstanceUID: e.OriginInstanceUID, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, Type: e.Type, Actor: e.Actor, Payload: e.Payload})
	}
	return out
}
