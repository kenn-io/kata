package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func linkDeltaTestCommand(t *testing.T, changedFlag string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	var parentRefs, removeParentRefs []string
	cmd.Flags().Var(newRefSliceValue(&parentRefs), "parent", "")
	cmd.Flags().Var(newRefSliceValue(&removeParentRefs), "remove-parent", "")
	if changedFlag == "parent" || changedFlag == "remove-parent" {
		if err := cmd.Flags().Set(changedFlag, "abc4"); err != nil {
			t.Fatalf("set --%s: %v", changedFlag, err)
		}
	}
	return cmd
}

func TestBuildLinksDeltaAlwaysYieldsADeltaForEverySetLinkFlag(t *testing.T) {
	tests := []struct {
		name                                         string
		changedFlag                                  string
		parentRef                                    string
		blocks, blockedBy, related                   []string
		removeParentRef                              string
		removeBlocks, removeBlockedBy, removeRelated []string
		want                                         map[string]any
	}{
		{name: "parent", changedFlag: "parent", parentRef: "abc4", want: map[string]any{"set_parent": "abc4"}},
		{name: "remove parent", changedFlag: "remove-parent", removeParentRef: "abc4", want: map[string]any{"remove_parent": "abc4"}},
		{name: "blocks", blocks: []string{"abc4"}, want: map[string]any{"add_blocks": []string{"abc4"}}},
		{name: "blocked by", blockedBy: []string{"abc4"}, want: map[string]any{"add_blocked_by": []string{"abc4"}}},
		{name: "related", related: []string{"abc4"}, want: map[string]any{"add_related": []string{"abc4"}}},
		{name: "remove blocks", removeBlocks: []string{"abc4"}, want: map[string]any{"remove_blocks": []string{"abc4"}}},
		{name: "remove blocked by", removeBlockedBy: []string{"abc4"}, want: map[string]any{"remove_blocked_by": []string{"abc4"}}},
		{name: "remove related", removeRelated: []string{"abc4"}, want: map[string]any{"remove_related": []string{"abc4"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delta, err := buildLinksDelta(
				context.Background(), linkDeltaTestCommand(t, tt.changedFlag), "", "example-project", 1,
				tt.parentRef, tt.blocks, tt.blockedBy, tt.related,
				tt.removeParentRef, tt.removeBlocks, tt.removeBlockedBy, tt.removeRelated,
			)
			if err != nil {
				t.Fatalf("buildLinksDelta: %v", err)
			}
			if delta == nil {
				t.Fatal("buildLinksDelta returned a nil delta for a set link flag")
			}
			if !reflect.DeepEqual(delta, tt.want) {
				t.Fatalf("buildLinksDelta delta = %#v, want %#v", delta, tt.want)
			}
		})
	}
}

func TestSplitRefListNeverYieldsAnEmptySlice(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty literal", in: "", want: []string{""}},
		{name: "two empty entries", in: ",", want: []string{"", ""}},
		{name: "one ref", in: "abc4", want: []string{"abc4"}},
		{name: "spaced refs", in: " abc4, def5 ", want: []string{"abc4", "def5"}},
		{name: "three empty entries", in: ",,", want: []string{"", "", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitRefList(tt.in)
			if len(got) == 0 {
				t.Fatal("splitRefList returned an empty slice")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("splitRefList(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}
