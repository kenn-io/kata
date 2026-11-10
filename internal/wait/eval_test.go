package wait

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- pure condition-evaluation unit tests -------------------------------

func TestWaitEvalCondition(t *testing.T) {
	type snapshot struct{ status, attention string }
	cases := []struct {
		name       string
		mode       string
		st         snapshot
		wantSat    bool
		wantReason string
	}{
		{"closed-mode-open-pending", "closed", snapshot{status: "open"}, false, ""},
		{"closed-mode-closed", "closed", snapshot{status: "closed"}, true, "closed"},
		{"needs-human-open-no-attention", "needs-human", snapshot{status: "open"}, false, ""},
		{"needs-human-open-ok", "needs-human", snapshot{status: "open", attention: "ok"}, false, ""},
		{"needs-human-match", "needs-human", snapshot{status: "open", attention: "needs-human"}, true, "attention"},
		{"needs-human-stuck-no-match", "needs-human", snapshot{status: "open", attention: "stuck"}, false, ""},
		{"stuck-match", "stuck", snapshot{status: "open", attention: "stuck"}, true, "attention"},
		{"stuck-needs-human-no-match", "stuck", snapshot{status: "open", attention: "needs-human"}, false, ""},
		{"attention-ok-no-match", "attention", snapshot{status: "open", attention: "ok"}, false, ""},
		{"attention-empty-no-match", "attention", snapshot{status: "open", attention: ""}, false, ""},
		{"attention-needs-human", "attention", snapshot{status: "open", attention: "needs-human"}, true, "attention"},
		{"attention-stuck", "attention", snapshot{status: "open", attention: "stuck"}, true, "attention"},
		{"attention-novel-level", "attention", snapshot{status: "open", attention: "on-fire"}, true, "attention"},
		{"needs-human-but-closed", "needs-human", snapshot{status: "closed"}, true, "closed"},
		{"attention-but-closed", "attention", snapshot{status: "closed", attention: "ok"}, true, "closed"},
		{"stuck-but-closed", "stuck", snapshot{status: "closed", attention: "stuck"}, true, "closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var evaluator Evaluator
			sat, reason := evaluator.Eval(c.mode, c.st.status, c.st.attention, nil)
			assert.Equal(t, c.wantSat, sat)
			assert.Equal(t, c.wantReason, reason)
		})
	}
}

func TestWaitReplySlotTransition(t *testing.T) {
	var evaluator Evaluator
	matched, _ := evaluator.Eval("reply", "open", "", jsontext.Value(`{"message":"old","re":"finding"}`))
	require.False(t, matched)
	matched, _ = evaluator.Eval("reply", "open", "", jsontext.Value(`{ "re": "finding", "message": "old" }`))
	require.False(t, matched)
	matched, reason := evaluator.Eval("reply", "open", "", nil)
	require.True(t, matched)
	require.Equal(t, "reply", reason)
	evaluator = Evaluator{}
	matched, _ = evaluator.Eval("reply", "open", "", nil)
	require.False(t, matched)
	matched, _ = evaluator.Eval("reply", "open", "", jsontext.Value(`"new"`))
	require.True(t, matched)
}

func FuzzWaitReplyFirstPollBaseline(f *testing.F) {
	f.Add("old", "new")
	f.Fuzz(func(t *testing.T, old, next string) {
		if len(old)+len(next) > 2048 {
			return
		}
		var evaluator Evaluator
		rawOld, e := json.Marshal(old)
		if e != nil {
			return
		}
		rawNew, e := json.Marshal(next)
		if e != nil {
			return
		}
		matched, _ := evaluator.Eval("reply", "open", "", rawOld)
		require.False(t, matched)
		matched, _ = evaluator.Eval("reply", "open", "", rawNew)
		require.Equal(t, old != next, matched)
	})
}
