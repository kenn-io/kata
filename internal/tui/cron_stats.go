package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

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

// cronStatsJob is the job whose detail is on top of the stack, if any.
func (m Model) cronStatsJob() string {
	if n := len(m.cron.stack); n > 0 && m.cron.stack[n-1].ref.kind == cronJobs {
		return m.cron.stack[n-1].ref.uid
	}
	return ""
}

// cronStatsDue reports whether the open job detail lacks statistics from the
// current refresh generation.
func (m Model) cronStatsDue() bool {
	uid := m.cronStatsJob()
	return uid != "" && (m.cron.stats.uid != uid || m.cron.stats.events != m.cron.events)
}

// cronStatsBegin starts a read for the open job detail. A different job drops
// the previous line; the same job keeps it until the new result arrives.
func (m Model) cronStatsBegin() (Model, func() cronStatsMsg) {
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
	return m, func() cronStatsMsg {
		msg.body, msg.err = cronRead(func(ctx context.Context, id int64) (*generated.CronRunStatsResponseBody, error) {
			return reader.CronRunStats(ctx, id, uid)
		}, id)
		return msg
	}
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
	value := "loading…"
	var tooOld apiVersionError
	switch {
	case s.uid != uid:
	case s.body != nil:
		value = cronStatsText(*s.body)
	case errors.As(s.err, &tooOld):
		value = "needs daemon API " + cronStatsAPIVersion + " or newer"
	case s.err != nil:
		value = "unavailable: " + sanitizeForLine(s.err.Error())
	}
	d.field("30d", value)
}

// cronStatsText renders counts, success rate, duration percentiles, and the
// failing streak, omitting whatever the window has no data for.
func cronStatsText(s generated.CronRunStatsResponseBody) string {
	runs := cronCount(int(s.Scanned), "run")
	if s.Truncated {
		runs = fmt.Sprintf("%d+ runs", s.Scanned)
	}
	parts := []string{runs}
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
	return strings.Join(parts, " · ")
}

// cronStatsDuration uses the cron views' Duration form, in whole seconds,
// and whole hours past what a Duration holds.
func cronStatsDuration(seconds float64) string {
	if seconds >= float64(math.MaxInt64/int64(time.Second)) {
		return fmt.Sprintf("%.0fh", seconds/3600)
	}
	return (time.Duration(math.Round(seconds)) * time.Second).String()
}
