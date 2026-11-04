package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

// CronTallyPrefix reserves project metadata for root-owned pruning evidence.
const CronTallyPrefix = "cron.tally."

// CronTallyCounts records deletion actions, not distinct lifetime runs.
type CronTallyCounts struct {
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	Unknown   int64 `json:"unknown"`
}

// CronTallyEntry starts tracking at the first prune that contributes to it.
// A resurrected run can contribute again, including under a different status.
type CronTallyEntry struct {
	Kind              string          `json:"kind"`
	UID               string          `json:"uid"`
	CoverageStartedAt time.Time       `json:"coverage_started_at"`
	LastPrunedAt      time.Time       `json:"last_pruned_at"`
	OldestCreatedAt   time.Time       `json:"oldest_created_at"`
	NewestCreatedAt   time.Time       `json:"newest_created_at"`
	Counts            CronTallyCounts `json:"counts"`
}

// CronTallyValue keeps a job and workflow with the same UID independent.
type CronTallyValue struct {
	Version int                       `json:"version"`
	Groups  map[string]CronTallyEntry `json:"groups"`
}

// ErrCronTallyConflict rejects unions of different valid histories for one group.
var ErrCronTallyConflict = errors.New("cron tally conflict")

func cronTallyError(format string, args ...any) error {
	return fmt.Errorf("%w: cron tally: %s", metadata.ErrInvalidValue, fmt.Sprintf(format, args...))
}

// DecodeCronTallies validates the complete reserved namespace without passing
// integer values through floating point. Absent ordinary metadata is allowed.
func DecodeCronTallies(document JSONBlob) (map[string]CronTallyValue, error) {
	fields, err := cronTallyMetadataFields(document)
	if err != nil {
		return nil, err
	}
	values := make(map[string]CronTallyValue)
	for key, raw := range fields {
		if !strings.HasPrefix(key, CronTallyPrefix) {
			continue
		}
		value, err := decodeCronTallyValue(key, raw)
		if err != nil {
			return nil, err
		}
		values[strings.TrimPrefix(key, CronTallyPrefix)] = value
	}
	return values, nil
}

func cronTallyMetadataFields(document JSONBlob) (map[string]jsontext.Value, error) {
	if document == "" || strings.TrimSpace(string(document)) == "null" {
		return map[string]jsontext.Value{}, nil
	}
	return cronTallyObject([]byte(document))
}

// cronTallyObject also rejects duplicate members through json/v2's default.
func cronTallyObject(raw jsontext.Value, required ...string) (map[string]jsontext.Value, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, cronTallyError("expected an object")
	}
	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, cronTallyError("invalid object: %v", err)
	}
	if len(required) == 0 {
		return fields, nil
	}
	if len(fields) != len(required) {
		return nil, cronTallyError("missing or unknown member")
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return nil, cronTallyError("missing %s", name)
		}
	}
	return fields, nil
}

func decodeCronTallyValue(key string, raw jsontext.Value) (CronTallyValue, error) {
	groupUID := strings.TrimPrefix(key, CronTallyPrefix)
	if !strings.HasPrefix(key, CronTallyPrefix) || !uid.Valid(groupUID) || groupUID != strings.ToUpper(groupUID) {
		return CronTallyValue{}, cronTallyError("invalid reserved key %q", key)
	}
	fields, err := cronTallyObject(raw, "version", "groups")
	if err != nil {
		return CronTallyValue{}, err
	}
	if string(fields["version"]) != "1" {
		return CronTallyValue{}, cronTallyError("unsupported version")
	}
	groups, err := cronTallyObject(fields["groups"])
	if err != nil || len(groups) == 0 {
		return CronTallyValue{}, cronTallyError("groups must be a nonempty object")
	}
	value := CronTallyValue{Version: 1, Groups: make(map[string]CronTallyEntry, len(groups))}
	for kind, raw := range groups {
		if kind != "job" && kind != "workflow" {
			return CronTallyValue{}, cronTallyError("unknown kind %q", kind)
		}
		entry, err := decodeCronTallyEntry(kind, groupUID, raw)
		if err != nil {
			return CronTallyValue{}, err
		}
		value.Groups[kind] = entry
	}
	return value, nil
}

func decodeCronTallyEntry(kind, groupUID string, raw jsontext.Value) (CronTallyEntry, error) {
	fields, err := cronTallyObject(raw, "kind", "uid", "coverage_started_at", "last_pruned_at", "oldest_created_at", "newest_created_at", "counts")
	if err != nil {
		return CronTallyEntry{}, err
	}
	var entry CronTallyEntry
	if err := json.Unmarshal(raw, &entry, json.RejectUnknownMembers(true)); err != nil {
		return CronTallyEntry{}, cronTallyError("invalid entry: %v", err)
	}
	if entry.Kind != kind || entry.UID != groupUID {
		return CronTallyEntry{}, cronTallyError("entry identity differs from container")
	}
	counts, err := cronTallyObject(fields["counts"], "succeeded", "failed", "cancelled", "unknown")
	if err != nil {
		return CronTallyEntry{}, err
	}
	positive := false
	for _, raw := range counts {
		literal := string(raw)
		for _, c := range literal {
			if c < '0' || c > '9' {
				return CronTallyEntry{}, cronTallyError("count must be a nonnegative int64 integer")
			}
		}
		n, err := strconv.ParseInt(literal, 10, 64)
		if err != nil || n < 0 {
			return CronTallyEntry{}, cronTallyError("count must be a nonnegative int64 integer")
		}
		positive = positive || n > 0
	}
	if !positive {
		return CronTallyEntry{}, cronTallyError("entry must contain a recorded prune")
	}
	for _, name := range []string{"coverage_started_at", "last_pruned_at", "oldest_created_at", "newest_created_at"} {
		if string(fields[name]) == "null" {
			return CronTallyEntry{}, cronTallyError("missing or invalid timestamp")
		}
	}
	for _, at := range []time.Time{entry.CoverageStartedAt, entry.LastPrunedAt, entry.OldestCreatedAt, entry.NewestCreatedAt} {
		if at.Year() < 1 || at.Year() > 9999 || at.UTC().Year() < 1 || at.UTC().Year() > 9999 {
			return CronTallyEntry{}, cronTallyError("missing or invalid timestamp")
		}
	}
	if entry.CoverageStartedAt.After(entry.LastPrunedAt) || entry.OldestCreatedAt.After(entry.NewestCreatedAt) {
		return CronTallyEntry{}, cronTallyError("inverted timestamp range")
	}
	entry.CoverageStartedAt = entry.CoverageStartedAt.UTC()
	entry.LastPrunedAt = entry.LastPrunedAt.UTC()
	entry.OldestCreatedAt = entry.OldestCreatedAt.UTC()
	entry.NewestCreatedAt = entry.NewestCreatedAt.UTC()
	return entry, nil
}

// RejectCronTallyPatch fences public metadata APIs, including no-op/deletion
// patches and malformed suffixes. Internal transaction writers do not call it.
func RejectCronTallyPatch(patch map[string]jsontext.Value) error {
	if key, ok := cronTallyPatchKey(patch); ok {
		return cronTallyError("%q is reserved for internal pruning/lifecycle writes", key)
	}
	return nil
}

// ValidateCronTallyPatch decodes the patched document only when the patch
// writes the reserved prefix, so ordinary writes keep legacy keys opaque.
func ValidateCronTallyPatch(patch map[string]jsontext.Value, document JSONBlob) error {
	if _, ok := cronTallyPatchKey(patch); !ok {
		return nil
	}
	_, err := DecodeCronTallies(document)
	return err
}

func cronTallyPatchKey(patch map[string]jsontext.Value) (string, bool) {
	for key := range patch {
		if strings.HasPrefix(key, CronTallyPrefix) {
			return key, true
		}
	}
	return "", false
}

// ValidateCronTallyEvent validates data only. Direction/grant checks belong at
// the authenticated ingest boundary, after accepted duplicate detection.
func ValidateCronTallyEvent(event RemoteEvent) error {
	if event.Type != "project.metadata_updated" && event.Type != "project.federation_enabled" {
		return nil
	}
	payload, err := cronTallyObject(event.Payload)
	if err != nil {
		return err
	}
	if event.Type == "project.federation_enabled" {
		_, err := DecodeCronTallies(JSONBlob(payload["metadata"]))
		return err
	}
	if len(payload["diff"]) == 0 {
		return nil
	}
	diff, err := cronTallyObject(payload["diff"])
	if err != nil {
		return err
	}
	for key, raw := range diff {
		if !strings.HasPrefix(key, CronTallyPrefix) {
			continue
		}
		sides, err := cronTallyObject(raw, "from", "to")
		if err != nil {
			return err
		}
		for _, side := range []string{"from", "to"} {
			if string(sides[side]) == "null" {
				// From null means prior absence; to null means whole-key deletion.
				// Still validate the key when neither side supplies a value.
				groupUID := strings.TrimPrefix(key, CronTallyPrefix)
				if !uid.Valid(groupUID) || groupUID != strings.ToUpper(groupUID) {
					return cronTallyError("invalid reserved key %q", key)
				}
				continue
			}
			if _, err := decodeCronTallyValue(key, sides[side]); err != nil {
				return err
			}
		}
	}
	return nil
}

// MergeCronTallyMetadata returns an absolute patch for a non-additive union.
// Import/adoption/merge combine disjoint identities and equal histories only.
func MergeCronTallyMetadata(target, source JSONBlob) (map[string]jsontext.Value, error) {
	destination, err := DecodeCronTallies(target)
	if err != nil {
		return nil, err
	}
	incoming, err := DecodeCronTallies(source)
	if err != nil {
		return nil, err
	}
	patch := map[string]jsontext.Value{}
	for groupUID, value := range incoming {
		current, found := destination[groupUID]
		changed := !found
		if !found {
			current = CronTallyValue{Version: 1, Groups: map[string]CronTallyEntry{}}
		}
		for kind, next := range value.Groups {
			previous, found := current.Groups[kind]
			if found && !sameCronTallyEntry(previous, next) {
				return nil, fmt.Errorf("%w: conflicting %s history for %s", ErrCronTallyConflict, kind, groupUID)
			}
			if !found {
				current.Groups[kind] = next
				changed = true
			}
		}
		if changed {
			raw, err := json.Marshal(current, json.Deterministic(true))
			if err != nil {
				return nil, err
			}
			patch[CronTallyPrefix+groupUID] = raw
		}
	}
	return patch, nil
}

// CronTallyAdoptionPatch combines a chunk before publishing its single root
// metadata event. Each accepted source remains intact in the event history.
func CronTallyAdoptionPatch(current JSONBlob, events []RemoteEvent) (map[string]jsontext.Value, error) {
	fields, err := cronTallyMetadataFields(current)
	if err != nil {
		return nil, err
	}
	patch := map[string]jsontext.Value{}
	// Replaying immutable adoption records replaces absolute values. Track that
	// result independently from the non-additive union retained by the root.
	replay := FoldProjection{ProjectMetadata: map[string]jsontext.Value{}}
	ordered := append([]RemoteEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		return compareClock(FoldClock{HLCPhysicalMS: a.HLCPhysicalMS, HLCCounter: a.HLCCounter, OriginInstanceUID: a.OriginInstanceUID, EventUID: a.EventUID}, FoldClock{HLCPhysicalMS: b.HLCPhysicalMS, HLCCounter: b.HLCCounter, OriginInstanceUID: b.OriginInstanceUID, EventUID: b.EventUID}) < 0
	})
	for _, event := range ordered {
		incoming, touched, err := CronTallyEventMetadata(event)
		if err != nil {
			return nil, err
		}
		if !touched {
			continue
		}
		if _, found := replay.ProjectMetadata[event.ProjectUID]; !found {
			replay.ProjectMetadata[event.ProjectUID] = jsontext.Value(current)
		}
		replay.apply(FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, Type: event.Type, Payload: jsontext.Value(event.Payload)})
		additions, err := MergeCronTallyMetadata(current, incoming)
		if err != nil {
			return nil, err
		}
		for key, value := range additions {
			fields[key], patch[key] = value, value
		}
		encoded, err := json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		current = JSONBlob(encoded)
	}
	for _, replayed := range replay.ProjectMetadata {
		corrections, err := MergeCronTallyMetadata(JSONBlob(replayed), current)
		if err != nil {
			return nil, err
		}
		maps.Copy(patch, corrections)
	}
	return patch, nil
}

func sameCronTallyEntry(a, b CronTallyEntry) bool {
	return a.Kind == b.Kind && a.UID == b.UID && a.Counts == b.Counts && a.CoverageStartedAt.Equal(b.CoverageStartedAt) && a.LastPrunedAt.Equal(b.LastPrunedAt) && a.OldestCreatedAt.Equal(b.OldestCreatedAt) && a.NewestCreatedAt.Equal(b.NewestCreatedAt)
}

// PreserveCronTallies keeps current root authority, including absent keys,
// while allowing ordinary metadata to refold from replicated event history.
func PreserveCronTallies(current, folded JSONBlob) (JSONBlob, error) {
	if _, err := DecodeCronTallies(current); err != nil {
		return "", err
	}
	if _, err := DecodeCronTallies(folded); err != nil {
		return "", err
	}
	local, err := cronTallyMetadataFields(current)
	if err != nil {
		return "", err
	}
	merged, err := cronTallyMetadataFields(folded)
	if err != nil {
		return "", err
	}
	for key := range merged {
		if strings.HasPrefix(key, CronTallyPrefix) {
			delete(merged, key)
		}
	}
	for key, value := range local {
		if strings.HasPrefix(key, CronTallyPrefix) {
			merged[key] = value
		}
	}
	raw, err := json.Marshal(merged, json.Deterministic(true))
	return JSONBlob(raw), err
}
