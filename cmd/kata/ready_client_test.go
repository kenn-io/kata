package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSelectNextReadyIssue(t *testing.T) {
	priority := func(value int64) *int64 { return &value }
	candidate := func(shortID string, p *int64) readyIssueForCLI {
		return readyIssueForCLI{
			Raw:      json.RawMessage(`{"short_id":"` + shortID + `"}`),
			ShortID:  shortID,
			Priority: p,
		}
	}

	tests := []struct {
		name       string
		candidates []readyIssueForCLI
		want       string
		wantFound  bool
	}{
		{name: "empty"},
		{
			name:       "first unprioritized",
			candidates: []readyIssueForCLI{candidate("first", nil), candidate("second", nil)},
			want:       "first",
			wantFound:  true,
		},
		{
			name:       "prioritized beats unprioritized",
			candidates: []readyIssueForCLI{candidate("unprioritized", nil), candidate("prioritized", priority(4))},
			want:       "prioritized",
			wantFound:  true,
		},
		{
			name: "lowest numeric priority wins",
			candidates: []readyIssueForCLI{
				candidate("p2", priority(2)),
				candidate("p1", priority(1)),
				candidate("p0", priority(0)),
			},
			want:      "p0",
			wantFound: true,
		},
		{
			name: "API order breaks equal priority ties",
			candidates: []readyIssueForCLI{
				candidate("first-p1", priority(1)),
				candidate("second-p1", priority(1)),
			},
			want:      "first-p1",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := selectNextReadyIssue(tt.candidates)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.want, got.ShortID)
		})
	}
}
