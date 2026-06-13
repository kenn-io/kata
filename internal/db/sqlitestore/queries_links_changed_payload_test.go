package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// TestLinksChangedPayload_GoldenBytes pins the exact wire bytes produced by
// linksChangedPayload. The invariant regressed once (a map round-trip
// alphabetized keys); full-byte equality catches field-order regressions,
// omitempty drift, and key renames before they reach consumers.
func TestLinksChangedPayload_GoldenBytes(t *testing.T) {
	const ts = "2026-06-11T12:00:00.000Z"

	p1 := db.PeerIdentity{
		ShortID: "abc1",
		UID:     "01HX00000000000000000PEER01",
		Project: "hub-project",
	}
	p2 := db.PeerIdentity{
		ShortID: "def2",
		UID:     "01HX00000000000000000PEER02",
		Project: "spoke-project",
	}
	parentSet := db.PeerIdentity{
		ShortID: "ghi3",
		UID:     "01HX00000000000000000PEER03",
		Project: "hub-project",
	}
	parentRemoved := db.PeerIdentity{
		ShortID: "jkl4",
		UID:     "01HX00000000000000000PEER04",
		Project: "hub-project",
	}

	// Fully populated: ParentSet + ParentRemoved + multiple slice families,
	// two entries in BlocksAdded to exercise multi-element ordering.
	full := db.AtomicEditChanges{
		ParentSet:        &parentSet,
		ParentRemoved:    &parentRemoved,
		BlocksAdded:      []db.PeerIdentity{p1, p2},
		BlocksRemoved:    []db.PeerIdentity{p1},
		BlockedByAdded:   []db.PeerIdentity{p2},
		BlockedByRemoved: []db.PeerIdentity{},
		RelatedAdded:     []db.PeerIdentity{p1},
		RelatedRemoved:   []db.PeerIdentity{p2},
	}
	bs, err := sqlitestore.LinksChangedPayloadForTest(full, ts)
	require.NoError(t, err)
	assert.Equal(t,
		`{"parent_set":"ghi3","parent_set_uid":"01HX00000000000000000PEER03","parent_removed":"jkl4","parent_removed_uid":"01HX00000000000000000PEER04","blocks_added":["abc1","def2"],"blocks_added_uids":["01HX00000000000000000PEER01","01HX00000000000000000PEER02"],"blocks_removed":["abc1"],"blocks_removed_uids":["01HX00000000000000000PEER01"],"blocked_by_added":["def2"],"blocked_by_added_uids":["01HX00000000000000000PEER02"],"related_added":["abc1"],"related_added_uids":["01HX00000000000000000PEER01"],"related_removed":["def2"],"related_removed_uids":["01HX00000000000000000PEER02"],"updated_at":"2026-06-11T12:00:00.000Z"}`,
		string(bs),
		"full payload byte order must match legacy wire shape",
	)

	// Sparse: only one family present; all others must be omitted (omitempty).
	sparse := db.AtomicEditChanges{
		BlocksAdded: []db.PeerIdentity{p1},
	}
	bs2, err := sqlitestore.LinksChangedPayloadForTest(sparse, ts)
	require.NoError(t, err)
	assert.Equal(t,
		`{"blocks_added":["abc1"],"blocks_added_uids":["01HX00000000000000000PEER01"],"updated_at":"2026-06-11T12:00:00.000Z"}`,
		string(bs2),
		"sparse payload must omit empty families",
	)
}
