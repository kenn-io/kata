package db

import (
	"encoding/json/jsontext"
	"errors"
	"maps"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/metadata"
)

// ScopedFoldPredicateFacts is copy-only evidence from the existing native fold.
// It never grants authority, enters capture bytes or replaces original events.
type ScopedFoldPredicateFacts struct {
	Parents         map[string]ScopedParentWriter
	IssueMetadata   map[string][]FoldClock
	ProjectMetadata map[string][]FoldClock
}

type ScopedParentWriter struct {
	ParentUID string
	Clock     FoldClock
}

func (ScopedFoldPredicateFacts) MarshalJSON() ([]byte, error) {
	return nil, errors.New("scoped predicate facts are in-process only")
}
func (*ScopedFoldPredicateFacts) UnmarshalJSON([]byte) error {
	return errors.New("scoped predicate facts are in-process only")
}
func (ScopedParentWriter) MarshalJSON() ([]byte, error) {
	return nil, errors.New("scoped parent writer is in-process only")
}
func (*ScopedParentWriter) UnmarshalJSON([]byte) error {
	return errors.New("scoped parent writer is in-process only")
}

type scopedMetadataSubject struct {
	uid     string
	project bool
}

// ScopedPredicateFacts returns independent maps/slices. Metadata clocks name
// distinct surviving native path writers, not just the last unrelated key.
// A decoded canonical capture is never an incremental accumulator.
func (p FoldProjection) ScopedPredicateFacts() ScopedFoldPredicateFacts {
	out := ScopedFoldPredicateFacts{Parents: maps.Clone(p.scopedParentWriters), IssueMetadata: map[string][]FoldClock{}, ProjectMetadata: map[string][]FoldClock{}}
	for subject, paths := range p.scopedMetadataWriters {
		unique := map[FoldClock]bool{}
		for _, clock := range paths {
			unique[clock] = true
		}
		if len(unique) == 0 {
			continue
		}
		clocks := make([]FoldClock, 0, len(unique))
		for clock := range unique {
			clocks = append(clocks, clock)
		}
		sort.Slice(clocks, func(i, j int) bool { return compareClock(clocks[i], clocks[j]) < 0 })
		if subject.project {
			out.ProjectMetadata[subject.uid] = clocks
		} else {
			out.IssueMetadata[subject.uid] = clocks
		}
	}
	return out
}

// This trace follows existing merge decisions; it never computes or writes
// metadata values. Length-prefixed paths are private, unambiguous map keys.
type scopedMetadataTrace struct {
	writers map[string]FoldClock
	clock   FoldClock
	path    string
	parent  *scopedMetadataTrace
}

func (p *FoldProjection) scopedMetadataTrace(subject string, project bool, clock FoldClock) *scopedMetadataTrace {
	if p.scopedMetadataWriters == nil {
		p.scopedMetadataWriters = map[scopedMetadataSubject]map[string]FoldClock{}
	}
	key := scopedMetadataSubject{uid: subject, project: project}
	if p.scopedMetadataWriters[key] == nil {
		p.scopedMetadataWriters[key] = map[string]FoldClock{}
	}
	return &scopedMetadataTrace{writers: p.scopedMetadataWriters[key], clock: clock}
}
func metadataTrace(in []*scopedMetadataTrace) *scopedMetadataTrace {
	if len(in) == 0 {
		return nil
	}
	return in[0]
}
func (t *scopedMetadataTrace) child(key string) *scopedMetadataTrace {
	if t == nil {
		return nil
	}
	return &scopedMetadataTrace{writers: t.writers, clock: t.clock, path: t.path + strconv.Itoa(len(key)) + ":" + key, parent: t}
}
func (t *scopedMetadataTrace) equal(a, b jsontext.Value) bool {
	if t.parent != nil && t.parent.path == "" {
		if len(a) == 0 || string(a) == "null" {
			a = nil
		}
		if len(b) == 0 || string(b) == "null" {
			b = nil
		}
	}
	return jsonRawEqual(a, b)
}
func (t *scopedMetadataTrace) clear() {
	if t == nil {
		return
	}
	for path, clock := range t.writers {
		// Exact duplicate originals must not erase their own surviving evidence.
		if strings.HasPrefix(path, t.path) && clock != t.clock {
			delete(t.writers, path)
		}
	}
}
func (t *scopedMetadataTrace) write() {
	if t == nil {
		return
	}
	for parent := t.parent; parent != nil; parent = parent.parent {
		delete(t.writers, parent.path)
	}
	t.writers[t.path] = t.clock
}
func (t *scopedMetadataTrace) delta(before, after jsontext.Value) {
	if t == nil || t.equal(before, after) {
		return
	}
	if rawJSONObject(before) && rawJSONObject(after) {
		a, b := parseRawObject(before), parseRawObject(after)
		keys := map[string]bool{}
		for key := range a {
			keys[key] = true
		}
		for key := range b {
			keys[key] = true
		}
		for key := range keys {
			t.child(key).delta(a[key], b[key])
		}
		return
	}
	if rawJSONObject(after) {
		values := parseRawObject(after)
		if len(values) == 0 {
			t.write()
			return
		}
		for key, value := range values {
			t.child(key).delta(nil, value)
		}
		return
	}
	t.write()
}
func (t *scopedMetadataTrace) replace(before, from, to, after jsontext.Value) {
	if t == nil {
		return
	}
	if t.equal(from, to) && t.equal(before, after) {
		return
	}
	// A whole-value decision supersedes descendant/absence witnesses even if
	// the current visible value already equals its replacement.
	t.clear()
	if t.equal(before, after) {
		t.write()
		return
	}
	t.delta(before, after)
}
func (t *scopedMetadataTrace) beginObject(before jsontext.Value) {
	if t != nil && !rawJSONObject(before) {
		t.clear()
	}
}
func (t *scopedMetadataTrace) endObject(before, after jsontext.Value) {
	if t != nil && !rawJSONObject(before) && rawJSONObject(after) && len(parseRawObject(after)) == 0 && !t.equal(before, after) {
		t.write()
	}
}

// Current snapshots replace metadata but same-value snapshots/compaction do
// not become predicate writers. Use native top-level null/absence semantics.
func (p *FoldProjection) recordScopedMetadataSnapshot(subject string, project bool, before, after jsontext.Value, clock FoldClock) {
	diff, err := metadata.Diff(before, after)
	if (err == nil && len(diff) == 0) || (err != nil && jsonRawEqual(before, after)) {
		return
	}
	t := p.scopedMetadataTrace(subject, project, clock)
	t.clear()
	t.delta(before, after)
}
func (p *FoldProjection) recordScopedParentFact(child, parent string, present bool, clock FoldClock) {
	current := p.scopedParentWriters[child].ParentUID
	next := current
	if present {
		next = parent
	} else if current == parent {
		next = ""
	}
	if next == current {
		return
	}
	if p.scopedParentWriters == nil {
		p.scopedParentWriters = map[string]ScopedParentWriter{}
	}
	p.scopedParentWriters[child] = ScopedParentWriter{ParentUID: next, Clock: clock}
}
