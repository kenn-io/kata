package config

import (
	"go.kenn.io/kata/internal/tracker"
	"testing"
)

func trackerFixture() TrackerConnectionConfig {
	return TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "g1", Provider: "notion", Label: "Example connection", Notion: &NotionSyncConfig{TokenEnv: "EXAMPLE_TRACKER_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Principal: tracker.Principal{AuthorityInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Kind: tracker.SubjectAccount, Subject: "member"}, Actions: []tracker.Action{tracker.ActionDiscover, tracker.ActionBind}, Sources: []string{"notion:source"}}}}
}

// Contract: named connections require their own explicit selector. They never
// inherit a daemon default, inspect an environment or probe provider accounts.
func TestTrackerConfigRequiresSelectedProviderAndExplicitSelector(t *testing.T) {
	c := trackerFixture()
	if _, err := NormalizeTrackerConnections([]TrackerConnectionConfig{c}); err != nil {
		t.Fatal(err)
	}
	bad := c
	bad.Notion = &NotionSyncConfig{}
	if _, err := NormalizeTrackerConnections([]TrackerConnectionConfig{bad}); err == nil {
		t.Fatal("implicit default credential accepted")
	}
	bad = c
	bad.Linear = &LinearSyncConfig{TokenEnv: "EXAMPLE_LINEAR_TOKEN"}
	if _, err := NormalizeTrackerConnections([]TrackerConnectionConfig{bad}); err == nil {
		t.Fatal("multiple credential providers accepted")
	}
	bad = c
	bad.Provider = "linear"
	if _, err := NormalizeTrackerConnections([]TrackerConnectionConfig{bad}); err == nil {
		t.Fatal("provider mismatch accepted")
	}
	if _, err := NormalizeTrackerConnections([]TrackerConnectionConfig{c, c}); err == nil {
		t.Fatal("duplicate UID accepted")
	}
}

func TestTrackerConfigSnapshotsDoNotAliasCaller(t *testing.T) {
	c := trackerFixture()
	got, err := NormalizeTrackerConnections([]TrackerConnectionConfig{c})
	if err != nil {
		t.Fatal(err)
	}
	c.Notion.TokenEnv = "CHANGED_SELECTOR"
	c.Grants[0].Actions[0] = tracker.ActionStatus
	c.Grants[0].Sources[0] = "other"
	if got[0].Notion.TokenEnv != "EXAMPLE_TRACKER_TOKEN" || got[0].Grants[0].Actions[0] != tracker.ActionDiscover || got[0].Grants[0].Sources[0] != "notion:source" {
		t.Fatal("registry aliased mutable caller")
	}
}

func TestTrackerRootMappingsAreQualified(t *testing.T) {
	c := trackerFixture()
	m := TrackerAccessMapping{ProjectUID: c.Grants[0].ProjectUID, Principal: c.Grants[0].Principal, Actor: "member"}
	if err := ValidateTrackerAccess([]TrackerAccessMapping{m}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTrackerAccess([]TrackerAccessMapping{m, m}); err == nil {
		t.Fatal("ambiguous mapping accepted")
	}
}
