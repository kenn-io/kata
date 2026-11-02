package commentref

import (
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
	"testing"
	"time"
)

// The close-verification contract requires independent principals and explicit
// dispute resolution; a later confirmation never erases a refutation.
func TestVerdicts(t *testing.T) {
	claim := graphRecord(1, "issue", "", "")
	confirm := graphRecord(2, "other", claim.UID, "confirm")
	confirm.Teammate = "reviewer"
	refute := graphRecord(3, "other", claim.UID, "refute")
	rebuttal := graphRecord(4, "other", refute.UID, "refute")
	reopened := graphRecord(5, "other", rebuttal.UID, "refute")
	for _, tc := range []struct {
		name            string
		rows            []Record
		confirmed, open int
	}{
		{"empty", nil, 0, 0}, {"self", []Record{claim, graphRecord(2, "issue", claim.UID, "confirm")}, 0, 0},
		{"teammate", []Record{claim, confirm}, 1, 0},
		{"conflict", []Record{claim, refute, confirm}, 0, 1},
		{"rebuttal", []Record{claim, refute, rebuttal}, 0, 0},
		{"reopened", []Record{claim, refute, rebuttal, reopened}, 0, 1},
		{"replaced claim", []Record{claim, refute, graphRecord(6, "other", claim.UID, "supersede")}, 0, 0},
		{"withdrawn refutation", []Record{claim, refute, graphRecord(6, "other", refute.UID, "supersede")}, 0, 0},
		{"other target", []Record{graphRecord(1, "other", "", ""), refute}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(tc.rows, "issue", nil)
			require.Len(t, v.Confirmed, tc.confirmed)
			require.Len(t, v.OpenRefutations, tc.open)
		})
	}
	v := Evaluate([]Record{claim, confirm, refute}, "issue", func(uid string) bool { return uid != refute.UID && uid != confirm.UID })
	require.Empty(t, v.Confirmed)
	require.Empty(t, v.OpenRefutations)
	require.Equal(t, 2, v.Redacted)
}

func TestVerdictsLocalRebuttalDoesNotBecomeNewBlocker(t *testing.T) {
	claim := graphRecord(1, "issue", "", "")
	r := graphRecord(2, "issue", claim.UID, "refute")
	b := graphRecord(3, "issue", r.UID, "refute")
	require.Empty(t, Evaluate([]Record{claim, r, b}, "issue", nil).OpenRefutations)
}

func TestVerdictsBoundedAndCyclic(t *testing.T) {
	claim := graphRecord(1, "issue", "", "")
	rows := []Record{claim}
	for n := 2; n < 13; n++ {
		rows = append(rows, graphRecord(n, "other", rows[len(rows)-1].UID, "refute"))
	}
	require.Len(t, Evaluate(rows, "issue", nil).OpenRefutations, 1, "depth exhaustion must not resolve a dispute")
	rows[2].ReplyToUID = rows[1].UID
	rows[1].ReplyToUID = rows[2].UID
	require.NotPanics(t, func() { Evaluate(rows, "issue", nil) })
}

func TestVerdictsConfirmationAndOrderProperties(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(1, 80))
		claim := graphRecord(1, "issue", "", "")
		r := graphRecord(2, "other", claim.UID, "refute")
		rows := []Record{claim, r}
		for n := range count {
			c := graphRecord(n+3, "other", claim.UID, "confirm")
			c.Teammate = "reviewer"
			rows = append(rows, c)
		}
		before := Evaluate(rows, "issue", nil)
		require.Len(ht, before.OpenRefutations, 1)
		require.Empty(ht, before.Confirmed)
		clean := Evaluate(append([]Record{claim}, rows[2:]...), "issue", nil)
		require.Equal(ht, count, clean.Confirmed[0].Confirmations)
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		require.Equal(ht, before, Evaluate(rows, "issue", nil))
	}, hegel.WithTestCases(30), hegel.WithDatabase(t.TempDir()))
}

// H2: the confirmation must still certify the current text and remain live.
func TestVerdictsConfirmationLifecycle(t *testing.T) {
	for _, state := range []string{"edited", "confirmation withdrawn", "claim replaced", "withdrawal superseded", "edited refutation"} {
		t.Run(state, func(t *testing.T) {
			claim := graphRecord(1, "issue", "", "")
			c := graphRecord(2, "other", claim.UID, "confirm")
			c.Teammate = "reviewer"
			rows := []Record{claim, c}
			switch state {
			case "edited":
				edited := time.Unix(3, 0)
				rows[0].EditedAt = &edited
			case "confirmation withdrawn", "withdrawal superseded":
				withdraw := graphRecord(3, "other", c.UID, "supersede")
				rows = append(rows, withdraw)
				if state == "withdrawal superseded" {
					rows = append(rows, graphRecord(4, "other", withdraw.UID, "supersede"))
				}
			case "claim replaced":
				rows = append(rows, graphRecord(3, "issue", claim.UID, "supersede"))
			case "edited refutation":
				r := graphRecord(3, "other", claim.UID, "refute")
				rows = append(rows, r)
				edited := time.Unix(4, 0)
				rows[0].EditedAt = &edited
			}
			v := Evaluate(rows, "issue", nil)
			require.Empty(t, v.Confirmed)
			if state == "edited refutation" {
				require.Len(t, v.OpenRefutations, 1, "editing text never dismisses a dispute")
			}
		})
	}
}
