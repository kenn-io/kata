package db_test

import (
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"strings"
	"testing"
)

// Latest current comment rows belong to the selected issue even while its
// tombstone withdraws every live reply edge. Narrowing never reveals peers.
func TestScopedProjectCommentPrecisionTombstone(t *testing.T) {
	fixture := dbtest.ScopedOwnerRootIdleFixture(t)
	view := fixture.View
	projectorUID := func(n int) string { return fmt.Sprintf("%026d", 90000+n) }
	c := db.CanonicalViewState{CaptureUID: projectorUID(94), ProjectCuts: map[string]int64{}, Fold: db.FoldProjection{Issues: map[string]db.FoldIssue{}, Comments: map[string]db.FoldComment{}, Labels: map[db.FoldLabelKey]db.FoldElementState{}, Links: map[db.FoldLinkKey]db.FoldElementState{}}}
	projectUIDs := []string{view.Projects[0].ProjectUID, view.Projects[1].ProjectUID, projectorUID(3)}
	for n, projectUID := range projectUIDs {
		c.Projects = append(c.Projects, db.Project{UID: projectUID, Name: fmt.Sprintf("project-%d", n), Metadata: db.JSONBlob(`{}`)})
		issueUID := projectorUID(n + 11)
		c.Fold.Issues[issueUID] = db.FoldIssue{UID: issueUID, ProjectUID: projectUID, ShortID: fmt.Sprintf("i%d", n), Title: "Current issue", Author: "author", Status: "open", CreatedAt: "2026-01-01T00:00:00Z"}
	}
	project := func(t *testing.T, state db.CanonicalViewState, descriptor db.ScopedViewDescriptor) db.ScopedCollections {
		t.Helper()
		out, err := db.ProjectScopedState(state, descriptor)
		require.NoError(t, err)
		return out
	}
	reference := func(t *testing.T, state db.ScopedCollections, kind, entity string) db.ScopedReference {
		t.Helper()
		for _, r := range state.References {
			if r.Kind == kind && r.SubjectEntityUID == entity {
				return r
			}
		}
		t.Fatalf("missing %s slot for %s", kind, entity)
		return db.ScopedReference{}
	}
	projectedBytes := func(t *testing.T, value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		return raw
	}
	a, b, hidden := projectorUID(11), projectorUID(12), projectorUID(13)
	ca, cb, ch := projectorUID(31), projectorUID(32), projectorUID(33)
	const precise = "2026-01-01T00:00:00.123456789Z"
	deleted := "2026-01-02T00:00:00Z"
	issue := c.Fold.Issues[a]
	issue.DeletedAt = &deleted
	c.Fold.Issues[a] = issue
	c.Fold.Comments[ca] = db.FoldComment{UID: ca, IssueUID: a, Author: "creator", Body: "latest tombstone comment", CreatedAt: precise, EditedAt: deleted, ReplyToUID: cb, ReplyKind: "reply"}
	c.Fold.Comments[cb] = db.FoldComment{UID: cb, IssueUID: b, Author: "peer-creator", Body: "latest live comment", CreatedAt: precise, ReplyToUID: ca, ReplyKind: "reply"}
	c.Fold.Comments[ch] = db.FoldComment{UID: ch, IssueUID: hidden, Author: "hidden-creator", Body: "hidden comment", CreatedAt: precise}
	c.Creation = append(c.Creation,
		db.ScopedProvenance{ProjectUID: projectUIDs[0], Kind: "comment", EntityUID: ca, Creator: "creator", AttributionStatus: "legacy"},
		db.ScopedProvenance{ProjectUID: projectUIDs[1], Kind: "comment", EntityUID: cb, Creator: "peer-creator", AttributionStatus: "legacy"},
		db.ScopedProvenance{ProjectUID: projectUIDs[2], Kind: "comment", EntityUID: ch, Creator: "hidden-creator", AttributionStatus: "legacy"})
	projected := project(t, c, view)
	if len(projected.Comments) != 2 {
		t.Fatalf("retained exact selected comment rows: %+v", projected.Comments)
	}
	for _, comment := range projected.Comments {
		if comment.CreatedAt.Nanosecond() != 123456789 {
			t.Fatalf("native precision lost: %s", comment.CreatedAt)
		}
		if comment.UID == ca && comment.Body != "latest tombstone comment" {
			t.Fatal("latest body was replaced")
		}
		r := reference(t, projected, "reply", comment.UID)
		if r.Present || r.Peer != nil || r.PeerEntityUID != "" || r.Relation != "" {
			t.Fatalf("tombstone reply exposed peer: %+v", r)
		}
	}
	narrow, err := db.RestrictScopedState(projected, nil, []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Comments) != 1 || narrow.Comments[0].UID != ca {
		t.Fatal("narrowing lost selected tombstone comment")
	}
	creation := false
	for _, provenance := range narrow.Provenance {
		creation = creation || provenance.Kind == "comment" && provenance.EntityUID == ca && provenance.Creator == "creator"
	}
	if !creation {
		t.Fatal("selected comment lost its permitted creation provenance")
	}
	encoded := string(projectedBytes(t, narrow))
	for _, secret := range []string{b, cb, hidden, ch, "hidden comment"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("restricted output leaked %s", secret)
		}
	}
	// Restore re-enables only the live authorized edges, with unchanged body,
	// creation time and comment identity rather than reconstructed history.
	issue.DeletedAt = nil
	c.Fold.Issues[a] = issue
	restored := project(t, c, view)
	if !reference(t, restored, "reply", ca).Present || !reference(t, restored, "reply", cb).Present {
		t.Fatal("restored live reply relation missing")
	}
	narrow, err = db.RestrictScopedState(restored, nil, []string{a})
	if err != nil {
		t.Fatal(err)
	}
	r := reference(t, narrow, "reply", ca)
	if r.Present || r.Peer != nil || r.PeerEntityUID != "" {
		t.Fatal("live subject leaked restricted reply peer")
	}
	if narrow.Comments[0] != projected.Comments[0] {
		t.Fatal("restore changed latest native comment row")
	}
}
