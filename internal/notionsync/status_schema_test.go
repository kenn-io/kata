package notionsync

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func groupSchema() DataSource {
	ds := schema()
	ds.Properties[1].Options = []Option{
		{ID: "ready", Name: "Queued"}, {ID: "active", Name: "Working"},
		{ID: "complete-b", Name: "Delivered"}, {ID: "complete-a", Name: "Accepted"},
	}
	ds.Properties[1].Groups = []Group{
		{ID: "g-done", Name: "Complete", OptionIDs: []string{"complete-b", "complete-a"}},
		{ID: "g-active", Name: "In progress", OptionIDs: []string{"active"}},
		{ID: "g-todo", Name: "To-do", OptionIDs: []string{"ready"}},
	}
	return ds
}

func TestStatusSchemaClassificationAndTargets(t *testing.T) {
	ds := groupSchema()
	c, err := ResolveConfig(ds, Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	resolved, err := ResolveStatusSchema(c, ds)
	require.NoError(t, err)
	for _, tc := range []struct {
		id   *string
		want string
	}{{nil, "open"}, {new("ready"), "open"}, {new("active"), "open"}, {new("complete-a"), "closed"}, {new("complete-b"), "closed"}} {
		got, err := resolved.Classify(tc.id)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	_, err = resolved.Classify(new("missing"))
	require.Error(t, err)
	target, err := resolved.Target("open")
	require.NoError(t, err)
	require.Equal(t, "ready", target)
	_, err = resolved.Target("in-progress")
	require.Error(t, err)
}

func TestLiveGroupOrderAndNamesDoNotChangeClassification(t *testing.T) {
	ds := groupSchema()
	c, err := ResolveConfig(ds, Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	_, err = ResolveStatusSchema(c, ds)
	require.NoError(t, err)
	ds.Properties[1].Groups[0].Name = "Renamed completion"
	ds.Properties[1].Groups[0].OptionIDs = []string{"complete-a", "complete-b"}
	slices.Reverse(ds.Properties[1].Groups)
	slices.Reverse(ds.Properties[1].Options)
	ds.Properties[1].Options[0].Name = "Renamed option"
	after, err := ResolveStatusSchema(c, ds)
	require.NoError(t, err)
	target, err := after.Target("closed")
	require.NoError(t, err)
	require.Equal(t, "complete-a", target)
	ds.Properties[1].Groups[1].OptionIDs = nil
	ds.Properties[1].Groups[2].OptionIDs = append(ds.Properties[1].Groups[2].OptionIDs, "active")
	changed, err := ResolveStatusSchema(c, ds)
	require.NoError(t, err)
	state, err := changed.Classify(new("active"))
	require.NoError(t, err)
	require.Equal(t, "closed", state)
}

func TestStatusSchemaOverridesAndExplicitGroupSelectors(t *testing.T) {
	ds := groupSchema()
	ds.Properties[1].Groups[0].Name = "Finished tasks"
	ds.Properties[1].Groups[2].Name = "Ready tasks"
	_, err := ResolveConfig(ds, Selectors{StatusSync: "two-way"}, "")
	require.Error(t, err)
	c, err := ResolveConfig(ds, Selectors{StatusSync: "two-way", CompleteGroup: "g-done", TodoGroup: "Ready tasks", ClosedStatus: "Accepted", OpenStatus: "ready"}, "")
	require.NoError(t, err)
	require.Equal(t, "complete-a", c.ClosedStatusID)
	require.Equal(t, "ready", c.OpenStatusID)
	resolved, err := ResolveStatusSchema(c, ds)
	require.NoError(t, err)
	target, err := resolved.Target("closed")
	require.NoError(t, err)
	require.Equal(t, "complete-a", target)
	_, err = ResolveConfig(ds, Selectors{StatusSync: "two-way", CompleteGroup: "g-done", TodoGroup: "g-todo", ClosedStatus: "active"}, "")
	require.Error(t, err)
}

func TestStatusSchemaRejectsMalformedMembershipAndWriteTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DataSource, *Config)
	}{
		{"duplicate option", func(ds *DataSource, _ *Config) {
			ds.Properties[1].Options = append(ds.Properties[1].Options, ds.Properties[1].Options[0])
		}},
		{"duplicate group", func(ds *DataSource, _ *Config) {
			ds.Properties[1].Groups = append(ds.Properties[1].Groups, ds.Properties[1].Groups[0])
		}},
		{"unknown member", func(ds *DataSource, _ *Config) {
			ds.Properties[1].Groups[0].OptionIDs = append(ds.Properties[1].Groups[0].OptionIDs, "missing")
		}},
		{"duplicate member", func(ds *DataSource, _ *Config) {
			ds.Properties[1].Groups[0].OptionIDs = append(ds.Properties[1].Groups[0].OptionIDs, "complete-a")
		}},
		{"overlap", func(ds *DataSource, _ *Config) {
			ds.Properties[1].Groups[2].OptionIDs = append(ds.Properties[1].Groups[2].OptionIDs, "complete-a")
		}},
		{"missing group", func(ds *DataSource, _ *Config) { ds.Properties[1].Groups = ds.Properties[1].Groups[1:] }},
		{"empty Complete", func(ds *DataSource, _ *Config) { ds.Properties[1].Groups[0].OptionIDs = nil }},
		{"empty To-do", func(ds *DataSource, _ *Config) { ds.Properties[1].Groups[2].OptionIDs = nil }},
		{"same selected groups", func(_ *DataSource, c *Config) { c.TodoGroupID = c.CompleteGroupID }},
		{"deleted override", func(_ *DataSource, c *Config) { c.ClosedStatusID = "missing" }},
		{"moved override", func(_ *DataSource, c *Config) { c.ClosedStatusID = "active" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := groupSchema()
			c, err := ResolveConfig(ds, Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			tc.mutate(&ds, &c)
			_, err = ResolveStatusSchema(c, ds)
			require.Error(t, err)
		})
	}
}

func TestGroupModeReenablePreservesIdentityAndAllowsExplicitLegacyOptIn(t *testing.T) {
	legacy := configFixture(t)
	grouped, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	require.NoError(t, ValidateReenable(legacy, grouped))
	oneWay := grouped
	oneWay.StatusSync = "one-way"
	require.NoError(t, ValidateReenable(grouped, oneWay))
	oneWay.CompleteGroupID = "replacement"
	require.Error(t, ValidateReenable(grouped, oneWay))
	grouped.StatusSync = "one-way"
	require.Error(t, ValidateReenable(legacy, grouped))
	_, err = ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way", DoneStatuses: []string{"Delivered"}}, "")
	require.Error(t, err)
	_, err = ResolveConfig(groupSchema(), Selectors{StatusSync: "invalid"}, "")
	require.Error(t, err)
}

func TestGroupModeAllowsInitialTodoSelectionAtTwoWayOptIn(t *testing.T) {
	ds := groupSchema()
	ds.Properties[1].Groups[2].Name = "Waiting"
	previous, err := ResolveConfig(ds, Selectors{}, "")
	require.NoError(t, err)
	require.Empty(t, previous.TodoGroupID)
	next, err := ResolveConfig(ds, Selectors{StatusSync: "two-way", TodoGroup: "g-todo"}, "")
	require.NoError(t, err)
	require.NoError(t, ValidateReenable(previous, next))
}

func TestGroupConfigRoundTripPreservesOverrideAndMode(t *testing.T) {
	c, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way", ClosedStatus: "complete-a", OpenStatus: "ready"}, "2026-09-01")
	require.NoError(t, err)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	decoded, err := DecodeConfig(raw)
	require.NoError(t, err)
	require.Equal(t, c, decoded)
	resolved, err := ResolveStatusSchema(decoded, groupSchema())
	require.NoError(t, err)
	target, err := resolved.Target("closed")
	require.NoError(t, err)
	require.Equal(t, "complete-a", target)
}

// Contract: custom option names and array order do not confer completion.
func TestStatusMembershipWithGeneratedWorkflowOptions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ds := groupSchema()
		p := &ds.Properties[1]
		p.Options = nil
		for i := range p.Groups {
			p.Groups[i].OptionIDs = nil
		}
		requested := rapid.IntRange(2, int(^uint(0)>>1)).Draw(t, "option count")
		count := min(requested, 40) // Bound test materialization, not the generated domain.
		roles := make([]int, count)
		for i := range count {
			role := rapid.IntRange(0, 2).Draw(t, "group")
			if i == 0 {
				role = 0
			}
			if i == 1 {
				role = 2
			}
			roles[i] = role
			id := "option-" + strconv.Itoa(i)
			p.Options = append(p.Options, Option{ID: id, Name: rapid.String().Draw(t, "option name")})
			// group fixture order is Complete, In progress, To-do.
			p.Groups[2-role].OptionIDs = append(p.Groups[2-role].OptionIDs, id)
		}
		if rapid.Bool().Draw(t, "reverse options") {
			slices.Reverse(p.Options)
		}
		if rapid.Bool().Draw(t, "reverse groups") {
			slices.Reverse(p.Groups)
		}
		c, err := ResolveConfig(ds, Selectors{StatusSync: "two-way"}, "")
		require.NoError(t, err)
		resolved, err := ResolveStatusSchema(c, ds)
		require.NoError(t, err)
		for i, role := range roles {
			want := "open"
			if role == 2 {
				want = "closed"
			}
			got, err := resolved.Classify(new("option-" + strconv.Itoa(i)))
			require.NoError(t, err)
			require.Equal(t, want, got)
		}
	})
}

func TestGeneratedOverlappingMembershipRejects(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ds := groupSchema()
		id := "opaque-" + rapid.String().Draw(t, "opaque option ID")
		ds.Properties[1].Options[3].ID = id
		ds.Properties[1].Groups[0].OptionIDs[1] = id
		ds.Properties[1].Groups[2].OptionIDs = append(ds.Properties[1].Groups[2].OptionIDs, id)
		_, err := ResolveConfig(ds, Selectors{StatusSync: "two-way"}, "")
		require.Error(t, err)
	})
}

// Contract: workflow group membership replaces a duplicated completed-option list.
func TestResolveConfigUsesGroupsWithoutRepeatedOptions(t *testing.T) {
	c, err := ResolveConfig(groupSchema(), Selectors{}, "")
	require.NoError(t, err)
	require.Equal(t, "g-done", c.CompleteGroupID)
	require.Empty(t, c.DoneStatusIDs)
	resolved, err := ResolveStatusSchema(c, groupSchema())
	require.NoError(t, err)
	target, err := resolved.Target("closed")
	require.NoError(t, err)
	require.Equal(t, "complete-b", target)
	state, err := resolved.Classify(new("active"))
	require.NoError(t, err)
	require.Equal(t, "open", state)
}
