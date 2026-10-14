package db_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A relay-sourced issue.links_changed folds without its crossing additions and
// with every removal, crossing or not, over links the project already had.
func TestStripCrossProjectLinkAdditionsFoldsMixedLinksChanged(t *testing.T) {
	const (
		project       = "00000000000000000000000001"
		base          = "00000000000000000000000010"
		sameOld       = "00000000000000000000000011"
		sameBlocks    = "00000000000000000000000012"
		sameBlockedBy = "00000000000000000000000013"
		sameRelated   = "00000000000000000000000014"
		foreignOld    = "00000000000000000000000020"
		foreignParent = "00000000000000000000000021"
		foreignNew    = "00000000000000000000000022"
		foreignBlocks = "00000000000000000000000023"
		foreignBlkBy  = "00000000000000000000000024"
		foreignRel    = "00000000000000000000000025"
	)
	foreign := map[string]bool{foreignOld: true, foreignParent: true, foreignNew: true, foreignBlocks: true, foreignBlkBy: true, foreignRel: true}
	crossProject := func(peer string) bool { return foreign[peer] }
	event := func(uid, eventType string, at int64, payload string) db.FoldEvent {
		return db.FoldEvent{UID: uid, OriginInstanceUID: "00000000000000000000000099", ProjectUID: project, IssueUID: base, Type: eventType, Actor: "relay-member", HLCPhysicalMS: at, CreatedAt: "2026-05-23T12:00:00.000Z", Payload: jsontext.Value(payload)}
	}
	// Links the project already had: a same-project edge, and crossing edges a
	// legacy peer created before relay enrollment.
	history := []db.FoldEvent{
		event("00000000000000000000000031", "issue.linked", 100, `{"issue_uid":"`+base+`","from_uid":"`+base+`","to_uid":"`+sameOld+`","type":"blocks"}`),
		event("00000000000000000000000032", "issue.linked", 101, `{"issue_uid":"`+base+`","from_uid":"`+base+`","to_uid":"`+foreignOld+`","type":"blocks"}`),
		event("00000000000000000000000033", "issue.linked", 102, `{"issue_uid":"`+base+`","from_uid":"`+base+`","to_uid":"`+foreignParent+`","type":"parent"}`),
	}
	changed := event("00000000000000000000000034", "issue.links_changed", 200, `{"issue_uid":"`+base+`",`+
		`"parent_set_uid":"`+foreignNew+`","parent_removed_uid":"`+foreignParent+`",`+
		`"blocks_added_uids":["`+sameBlocks+`","`+foreignBlocks+`"],"blocks_removed_uids":["`+sameOld+`","`+foreignOld+`"],`+
		`"blocked_by_added_uids":["`+sameBlockedBy+`","`+foreignBlkBy+`"],`+
		`"related_added_uids":["`+sameRelated+`","`+foreignRel+`"]}`)

	stripped, keep, err := db.StripCrossProjectLinkAdditions(changed, crossProject)
	require.NoError(t, err)
	require.True(t, keep, "a links_changed with same-project effects stays in the fold")
	projection := db.FoldEvents(append(history, stripped))

	present := map[db.FoldLinkKey]bool{}
	for key, state := range projection.Links {
		present[key] = state.Present
	}
	require.Equal(t, map[db.FoldLinkKey]bool{
		{FromUID: base, ToUID: sameOld, Type: "blocks"}:       false,
		{FromUID: base, ToUID: foreignOld, Type: "blocks"}:    false,
		{FromUID: base, ToUID: foreignParent, Type: "parent"}: false,
		{FromUID: base, ToUID: sameBlocks, Type: "blocks"}:    true,
		{FromUID: sameBlockedBy, ToUID: base, Type: "blocks"}: true,
		{FromUID: base, ToUID: sameRelated, Type: "related"}:  true,
	}, present, "crossing additions are stripped; same-project additions and all removals are applied")
}
