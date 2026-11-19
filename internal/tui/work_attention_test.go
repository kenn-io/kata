package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withWorkAttention(level, msg string) issueOpt {
	return func(i *Issue) {
		if level != "" {
			i.Metadata.WorkAttention = jsontext.Value(`"` + level + `"`)
		}
		if msg != "" {
			i.Metadata.WorkAttentionMsg = jsontext.Value(`"` + msg + `"`)
		}
	}
}

func TestWorkAttention_OnlyOpenKnownLevelsReport(t *testing.T) {
	deleted := testIssue("gone", withWorkAttention("stuck", "x"))
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	deleted.DeletedAt = &at
	for name, tc := range map[string]struct {
		iss  Issue
		want string
	}{
		"ok":          {testIssue("a", withWorkAttention("ok", "m")), "ok"},
		"stuck":       {testIssue("a", withWorkAttention("stuck", "m")), "stuck"},
		"needs-human": {testIssue("a", withWorkAttention("needs-human", "m")), "needs-human"},
		"unset":       {testIssue("a"), ""},
		"unknown":     {testIssue("a", withWorkAttention("panic", "m")), ""},
		"closed":      {testIssue("a", withWorkAttention("stuck", "m"), withStatus("closed")), ""},
		"deleted":     {deleted, ""},
	} {
		t.Run(name, func(t *testing.T) {
			level, msg := tc.iss.workAttention()
			assert.Equal(t, tc.want, level)
			if tc.want == "" {
				assert.Empty(t, msg)
			} else {
				assert.Equal(t, "m", msg)
			}
		})
	}
}

func TestWorkAttention_ListRowsMarkOnlyStuckAndNeedsHuman(t *testing.T) {
	rows := buildRows([]queueRow{
		{issue: testIssue("stk1", withWorkAttention("stuck", ""))},
		{issue: testIssue("hum1", withWorkAttention("needs-human", ""))},
		{issue: testIssue("fine", withWorkAttention("ok", ""))},
	}, 0, 40, false, viewChrome{})

	assert.Contains(t, rows[0][4], "! issue stk1")
	assert.Contains(t, rows[1][4], "! issue hum1")
	assert.NotContains(t, rows[2][4], "!")
}

func TestWorkAttention_ListRowMarkerKeepsTitleWidth(t *testing.T) {
	rows := buildRows([]queueRow{
		{issue: testIssue("stk1", withWorkAttention("stuck", ""), withTitle(strings.Repeat("long ", 20)))},
	}, 0, 30, false, viewChrome{})
	assert.LessOrEqual(t, len([]rune(stripANSI(rows[0][4]))), 30)
	assert.True(t, strings.HasPrefix(stripANSI(rows[0][4]), "! long"))
}

func TestWorkAttention_DetailShowsValueAndMessage(t *testing.T) {
	render := func(iss Issue) string {
		return stripANSI(strings.Join(renderDocumentMetadata(100, iss, nil, nil, scope{}, uidDisplayNone), "\n"))
	}
	got := render(testIssue("stk1", withWorkAttention("stuck", "waiting on the CI fix")))
	assert.Contains(t, got, "attention: stuck - waiting on the CI fix")

	got = render(testIssue("stk1", withWorkAttention("needs-human", "")))
	assert.Contains(t, got, "attention: needs-human")
	assert.NotContains(t, got, " - ")

	got = render(testIssue("fine", withWorkAttention("ok", "implementing")))
	assert.Contains(t, got, "attention: ok - implementing")
}

func TestWorkAttention_DetailKeepsMessageOnOneRow(t *testing.T) {
	iss := testIssue("stk1", withWorkAttention("stuck", `bad\u001b[31m red\nsecond line\ttabbed`))
	rows := renderDocumentMetadata(100, iss, nil, nil, scope{}, uidDisplayNone)
	var attention []string
	for _, r := range rows {
		if strings.Contains(stripANSI(r), "attention:") {
			attention = append(attention, r)
		}
	}
	require.Len(t, attention, 1)
	assert.NotContains(t, attention[0], "\x1b[31m")
	assert.NotContains(t, attention[0], "\n")
	assert.NotContains(t, attention[0], "\t")
	assert.Contains(t, stripANSI(attention[0]), "second line")
}

func TestWorkAttention_WireDecodesFromOrdinaryListRow(t *testing.T) {
	var resp struct {
		Issues []Issue `json:"issues"`
	}
	body := `{"issues":[{"short_id":"abc4","status":"open","metadata":{"scheduled_on":"2026-11-02","work.attention":"stuck","work.attention_msg":"blocked"}}]}`
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	level, msg := resp.Issues[0].workAttention()
	assert.Equal(t, "stuck", level)
	assert.Equal(t, "blocked", msg)
	assert.Equal(t, "2026-11-02", resp.Issues[0].scheduledOn())
}
