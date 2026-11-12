package cron

import (
	"math"
	"sort"
	"time"
)

// RunStatRow contains only the evidence needed for statistics, newest first.
type RunStatRow struct {
	Status             string
	CreatedAt          time.Time
	StartedAt, EndedAt *time.Time
}
type DurationStats struct {
	_      struct{} `nullable:"true"`
	Count  int      `json:"count"`
	Min    float64  `json:"min"`
	Median float64  `json:"median"`
	P95    float64  `json:"p95"`
	Max    float64  `json:"max"`
}
type RunStatBucket struct {
	Date                  string         `json:"date"`
	ByStatus              map[string]int `json:"by_status"`
	MedianDurationSeconds *float64       `json:"median_duration_seconds" nullable:"true"`
}
type RunStatistics struct {
	Scanned             int             `json:"scanned"`
	ByStatus            map[string]int  `json:"by_status"`
	SuccessRate         *float64        `json:"success_rate" nullable:"true"`
	DurationSeconds     *DurationStats  `json:"duration_seconds"`
	LastSucceededAt     *time.Time      `json:"last_succeeded_at" nullable:"true"`
	LastFailedAt        *time.Time      `json:"last_failed_at" nullable:"true"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	Buckets             []RunStatBucket `json:"buckets,omitzero"`
}

func durationStats(values []float64) *DurationStats {
	if len(values) == 0 {
		return nil
	}
	sort.Float64s(values)
	rank := func(p float64) float64 { return values[int(math.Ceil(p*float64(len(values))))-1] }
	return &DurationStats{Count: len(values), Min: values[0], Median: rank(.5), P95: rank(.95), Max: values[len(values)-1]}
}

// SummarizeRuns computes statistics from rows ordered newest first without reading a clock.
func SummarizeRuns(rows []RunStatRow, day bool, loc *time.Location) RunStatistics {
	out := RunStatistics{Scanned: len(rows), ByStatus: map[string]int{"running": 0, "succeeded": 0, "failed": 0, "cancelled": 0, "unknown": 0}}
	var durations []float64
	byDay := map[string]int{}
	var daily [][]float64
	streak := true
	for _, r := range rows {
		out.ByStatus[r.Status]++
		if streak {
			if r.Status == "failed" {
				out.ConsecutiveFailures++
			} else if r.Status == "succeeded" {
				streak = false
			}
		}
		instant := r.CreatedAt
		if r.EndedAt != nil {
			instant = *r.EndedAt
		}
		if r.Status == "succeeded" && (out.LastSucceededAt == nil || instant.After(*out.LastSucceededAt)) {
			v := instant
			out.LastSucceededAt = &v
		}
		if r.Status == "failed" && (out.LastFailedAt == nil || instant.After(*out.LastFailedAt)) {
			v := instant
			out.LastFailedAt = &v
		}
		idx := -1
		if day {
			date := r.CreatedAt.In(loc).Format("2006-01-02")
			var ok bool
			idx, ok = byDay[date]
			if !ok {
				idx = len(out.Buckets)
				byDay[date] = idx
				out.Buckets = append(out.Buckets, RunStatBucket{Date: date, ByStatus: map[string]int{}})
				daily = append(daily, nil)
			}
			out.Buckets[idx].ByStatus[r.Status]++
		}
		if (r.Status == "succeeded" || r.Status == "failed") && r.StartedAt != nil && r.EndedAt != nil {
			d := float64(r.EndedAt.Unix()-r.StartedAt.Unix()) + float64(r.EndedAt.Nanosecond()-r.StartedAt.Nanosecond())/1e9
			durations = append(durations, d)
			if day {
				daily[idx] = append(daily[idx], d)
			}
		}
	}
	if n := out.ByStatus["succeeded"] + out.ByStatus["failed"]; n > 0 {
		v := math.Round(float64(out.ByStatus["succeeded"])/float64(n)*10000) / 10000
		out.SuccessRate = &v
	}
	out.DurationSeconds = durationStats(durations)
	for i := range out.Buckets {
		if s := durationStats(daily[i]); s != nil {
			v := s.Median
			out.Buckets[i].MedianDurationSeconds = &v
		}
	}
	if day && out.Buckets == nil {
		out.Buckets = []RunStatBucket{}
	}
	return out
}
