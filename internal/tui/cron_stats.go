package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/pkg/client/generated"
)

// cronStats is the 30-day statistics line for the open job detail. events is
// the cron refresh generation the read was started for.
type cronStats struct {
	uid    string
	events uint64
	seq    uint64
	body   *generated.CronRunStatsResponseBody
	err    error
}

// cronStatsMsg answers one statistics read; seq names the read it answers.
type cronStatsMsg struct {
	conn, scope, seq uint64
	body             *generated.CronRunStatsResponseBody
	err              error
}

// cronStatsJob is the job whose detail is shown, if any. A job missing from
// the loaded data has no detail to show the line in.
func (m Model) cronStatsJob() string {
	n := len(m.cron.stack)
	if m.view != viewCron || n == 0 || m.cron.data == nil {
		return ""
	}
	if ref := m.cron.stack[n-1].ref; ref.kind == cronJobs && m.cron.data.job(ref.uid) != nil {
		return ref.uid
	}
	return ""
}

// cronStatsDue reports whether the open job detail lacks statistics from the
// current refresh generation.
func (m Model) cronStatsDue() bool {
	uid := m.cronStatsJob()
	return uid != "" && (m.cron.stats.uid != uid || m.cron.stats.events != m.cron.events)
}

// cronStatsLeave drops the line once its job detail is no longer shown, and
// fences off its pending read, so returning to the job reads afresh.
func (m Model) cronStatsLeave() Model {
	if m.cron.stats.uid != "" && m.cron.stats.uid != m.cronStatsJob() {
		m.cron.stats = cronStats{seq: m.cron.stats.seq + 1}
	}
	return m
}

// cronStatsBegin starts a read for the open job detail. A different job drops
// the previous line; the same job keeps it until the new result arrives.
func (m Model) cronStatsBegin() (Model, tea.Cmd) {
	uid := m.cronStatsJob()
	if uid == "" {
		return m, nil
	}
	seq := m.cron.stats.seq + 1
	if m.cron.stats.uid != uid {
		m.cron.stats = cronStats{uid: uid}
	}
	m.cron.stats.seq, m.cron.stats.events = seq, m.cron.events
	reader, id := m.api, m.scope.projectID
	msg := cronStatsMsg{conn: m.connGen, scope: m.scopeGen, seq: seq}
	return m, func() tea.Msg {
		msg.body, msg.err = cronRead(func(ctx context.Context, id int64) (*generated.CronRunStatsResponseBody, error) {
			return reader.CronRunStats(ctx, id, uid)
		}, id)
		return msg
	}
}

// cronStatsWhileListing runs while the cron list read is out. A newly opened
// job detail reads at once. A refresh fences off the older read instead, and
// its own read starts with the list read that the refresh then requires.
func (m Model) cronStatsWhileListing() (Model, tea.Cmd) {
	uid := m.cronStatsJob()
	switch {
	case uid == "":
	case m.cron.stats.uid != uid:
		return m.cronStatsBegin()
	case m.cron.stats.events != m.cron.events:
		m.cron.stats.seq, m.cron.stats.events = m.cron.stats.seq+1, m.cron.events
	}
	return m, nil
}

// cronStatsLoaded keeps only the answer to the latest read on this daemon and
// project, so a result for a job no longer open is ignored.
func (m Model) cronStatsLoaded(msg cronStatsMsg) Model {
	if msg.conn != m.connGen || msg.scope != m.scopeGen || msg.seq != m.cron.stats.seq {
		return m
	}
	m.cron.stats.body, m.cron.stats.err = msg.body, msg.err
	return m
}

// cronStatsField adds the job detail line. Loading, failure, and an older
// daemon each show a notice in place of the numbers.
func cronStatsField(d *cronDoc, s cronStats, uid string) {
	label, value := "30d", "loading…"
	var tooOld apiVersionError
	switch {
	case s.uid != uid:
	case s.body != nil:
		label, value = cronStatsText(*s.body)
	case errors.As(s.err, &tooOld):
		value = "needs daemon API " + kataapi.CronRunStatsAPIVersion + " or newer"
	case s.err != nil:
		value = "unavailable: " + sanitizeForLine(s.err.Error())
	}
	d.field(label, value)
}

// cronStatsText renders counts, success rate, duration percentiles, and the
// failing streak, omitting whatever the window has no data for. A truncated
// scan covers only the span since its oldest run, so its label names that
// start instead of 30 days.
func cronStatsText(s generated.CronRunStatsResponseBody) (label, value string) {
	label, parts := "30d", []string{cronCount(int(s.Scanned), "run")}
	if s.Truncated {
		label, parts = "Since", []string{cronTime(&s.Window.Since), fmt.Sprintf("%d+ runs", s.Scanned)}
	}
	if s.SuccessRate != nil {
		// The rate has four places; whole percent rounds down so a failure never reads 100%.
		parts = append(parts, fmt.Sprintf("%d%% ok", int64(math.Round(*s.SuccessRate*10000))/100))
	}
	if d := s.DurationSeconds; d != nil {
		parts = append(parts, "median "+cronStatsDuration(d.Median), "p95 "+cronStatsDuration(d.P95))
	}
	if s.ConsecutiveFailures > 0 {
		parts = append(parts, fmt.Sprintf("failing ×%d", s.ConsecutiveFailures))
	}
	return label, strings.Join(parts, " · ")
}

// cronStatsDuration uses the cron views' Duration form in whole seconds,
// also past what a Duration holds.
func cronStatsDuration(seconds float64) string {
	if seconds >= float64(math.MaxInt64/int64(time.Second)) {
		total := math.Round(seconds)
		hours := math.Floor(total / 3600)
		rest := int64(total - hours*3600)
		return fmt.Sprintf("%.0fh%dm%ds", hours, rest/60, rest%60)
	}
	return (time.Duration(math.Round(seconds)) * time.Second).String()
}
