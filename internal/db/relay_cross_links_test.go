package db_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayCrossLinkFeatureAndParentFold(t *testing.T) {
	root, replica := "root", "replica"
	child, foreign, local := "child", "foreign", "local"
	remote := db.RemoteEvent{OriginInstanceUID: root, ProjectUID: "a", IssueUID: &child, Type: "issue.links_changed", Payload: jsontext.Value(`{"related_added_uids":["foreign"]}`)}
	projects := map[string]string{child: "a", foreign: "b", local: "a"}
	resolve := func(uid string) (string, error) { return projects[uid], nil }
	feature, err := db.RelayContentRequiredFeatures(remote, root, resolve)
	require.NoError(t, err)
	require.Equal(t, db.RelayCrossLinksEventFeature, feature)
	delete(projects, foreign)
	feature, err = db.RelayContentRequiredFeatures(remote, root, resolve)
	require.NoError(t, err)
	require.Equal(t, db.RelayCrossLinksEventFeature, feature)
	projects[foreign] = "a"
	feature, err = db.RelayContentRequiredFeatures(remote, root, resolve)
	require.NoError(t, err)
	require.Empty(t, feature)
	remote.OriginInstanceUID = replica
	projects[foreign] = "b"
	feature, err = db.RelayContentRequiredFeatures(remote, root, resolve)
	require.NoError(t, err)
	require.Empty(t, feature)

	authority := db.RelayFoldAuthority{Projects: projects, Roots: map[string]string{"a": root, "b": root}, Groups: map[string]map[string]bool{"a": {"b": true}}}
	events := []db.FoldEvent{
		{UID: "1", ProjectUID: "a", IssueUID: child, OriginInstanceUID: root, Type: "issue.links_changed", HLCPhysicalMS: 1, Payload: jsontext.Value(`{"parent_set_uid":"foreign"}`)},
		{UID: "2", ProjectUID: "a", IssueUID: child, OriginInstanceUID: replica, Type: "issue.links_changed", HLCPhysicalMS: 2, Payload: jsontext.Value(`{"parent_set_uid":"local"}`)},
	}
	projection := db.FoldEvents(events, authority.AllowParentRemoval)
	require.True(t, projection.Links[db.FoldLinkKey{FromUID: child, ToUID: foreign, Type: "parent"}].Present)
	require.False(t, projection.Links[db.FoldLinkKey{FromUID: child, ToUID: local, Type: "parent"}].Present)
	events[1].OriginInstanceUID = root
	projection = db.FoldEvents(events, authority.AllowParentRemoval)
	require.False(t, projection.Links[db.FoldLinkKey{FromUID: child, ToUID: foreign, Type: "parent"}].Present)
	require.True(t, projection.Links[db.FoldLinkKey{FromUID: child, ToUID: local, Type: "parent"}].Present)
}

func TestRelayCrossLinkPolicy(t *testing.T) {
	child, peer := "child", "peer"
	authority := db.RelayFoldAuthority{
		Projects: map[string]string{child: "a", peer: "b"},
		Roots:    map[string]string{"a": "root", "b": "root"},
		Groups:   map[string]map[string]bool{"a": {"b": true}},
	}
	event := db.FoldEvent{UID: "event", ProjectUID: "a", IssueUID: child, OriginInstanceUID: "root", Type: "issue.linked", Payload: jsontext.Value(`{"from_uid":"child","to_uid":"peer","type":"blocks"}`)}
	_, keep, err := db.StripCrossProjectLinkAdditions(event, authority.Policy(event))
	require.NoError(t, err)
	require.True(t, keep)
	authority.Roots["b"] = "other"
	_, keep, err = db.StripCrossProjectLinkAdditions(event, authority.Policy(event))
	require.NoError(t, err)
	require.False(t, keep)
	authority.Roots["b"] = "root"
	authority.Groups["a"]["b"] = false
	_, keep, err = db.StripCrossProjectLinkAdditions(event, authority.Policy(event))
	require.NoError(t, err)
	require.False(t, keep)
	authority.Groups["a"]["b"] = true
	event.OriginInstanceUID = "replica"
	event.Type = "issue.unlinked"
	_, keep, err = db.StripCrossProjectLinkAdditions(event, authority.Policy(event))
	require.NoError(t, err)
	require.False(t, keep)
	event.OriginInstanceUID = "root"
	_, keep, err = db.StripCrossProjectLinkAdditions(event, authority.Policy(event))
	require.NoError(t, err)
	require.True(t, keep)
}
