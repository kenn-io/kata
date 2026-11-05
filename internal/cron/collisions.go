package cron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"time"

	cronparser "github.com/netresearch/go-cron"
)

// CollisionStepBudget bounds candidate and peer stepping, including probes.
const CollisionStepBudget = 200000
const collisionWalkLimit = 10080

// CollisionJob is a candidate or a live stored peer. Native peers need progress.
type CollisionJob struct {
	UID, Name string
	Def       JobDefinition
	Progress  *RecurringProgress
}

// CollisionPeer identifies one member of the complete (or known) peer set.
type CollisionPeer struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// CollisionItem groups a peer set at its first shared instant.
type CollisionItem struct {
	At          time.Time       `json:"at"`
	Local       string          `json:"local"`
	Occurrences int             `json:"occurrences"`
	PeerCount   int             `json:"peer_count"`
	Peers       []CollisionPeer `json:"peers"`
}

// ValidationCollisions reports hypothetical coincidences, never dispatch.
// Truncated groups contain known peers; only complete results have exact sets.
type ValidationCollisions struct {
	Until     time.Time       `json:"until"`
	Items     []CollisionItem `json:"items"`
	Skipped   int             `json:"skipped"`
	Truncated bool            `json:"truncated"`
}

// The candidate and retained progress are validated by callers; peers are
// validated during admission below.
func occurrencesWithin(j JobDefinition, p *RecurringProgress, from, until time.Time, limit int, budget *int) ([]time.Time, bool, error) {
	after := from.UTC().Add(-time.Nanosecond)
	progress := RecurringProgress{Version: 1, Anchor: from.UTC(), Through: after}
	if p != nil {
		progress = *p
	}
	return walkOccurrences(j, progress, after, &until, limit, budget)
}

// PreviewCollisions is deterministic, observes cancellation between peer walks,
// and leaves both the fleet and candidate intact. Callers supply enabled peers
// of the same delivery kind in binary name/UID order, excluding the candidate,
// with validated retained progress for native peers.
func PreviewCollisions(ctx context.Context, candidate CollisionJob, peers []CollisionJob, from time.Time, budget int) (ValidationCollisions, error) {
	out := ValidationCollisions{Until: from.UTC().Add(7 * 24 * time.Hour), Items: []CollisionItem{}}
	if out.Until.Year() > 9999 {
		out.Until = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
		out.Truncated = true
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	candidate.Def.Enabled = true
	times, limited, err := occurrencesWithin(candidate.Def, candidate.Progress, from, out.Until, collisionWalkLimit, &budget)
	out.Truncated = out.Truncated || limited
	if err != nil {
		out.Skipped++
		return out, nil
	}
	// UTC nanoseconds are not sufficient for the full RFC3339 year domain.
	matches := make(map[time.Time][]CollisionPeer, len(times))
	locals := make(map[time.Time]string, len(times))
	zone := TriggerLocation(candidate.Def.Trigger)
	for _, at := range times {
		_, utcErr := at.MarshalText()
		local, localErr := at.In(zone).MarshalText()
		if utcErr != nil || localErr != nil {
			out.Truncated = true
			continue
		}
		matches[at] = nil
		locals[at] = string(local)
	}
	for _, peer := range peers {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !IsRecurringIssueJob(candidate.Def) && pinsDisjoint(candidate.Def.Options, peer.Def.Options) {
			continue
		}
		if !collisionInstants(peer.Def) || peer.Def.Validate() != nil {
			out.Skipped++
			continue
		}
		instants, limited, err := occurrencesWithin(peer.Def, peer.Progress, from, out.Until, collisionWalkLimit, &budget)
		out.Truncated = out.Truncated || limited
		if err != nil {
			out.Skipped++
			continue
		}
		for _, at := range instants {
			if members, ok := matches[at]; ok {
				matches[at] = append(members, CollisionPeer{UID: peer.UID, Name: peer.Name})
			}
		}
	}
	bySet := map[string]int{}
	for _, at := range times {
		members := matches[at]
		if len(members) == 0 {
			continue
		}
		var key strings.Builder
		for _, peer := range members {
			key.WriteString(peer.UID)
			key.WriteByte(0)
		}
		if index, ok := bySet[key.String()]; ok {
			out.Items[index].Occurrences++
			continue
		}
		bySet[key.String()] = len(out.Items)
		item := CollisionItem{At: at, Local: locals[at], Occurrences: 1, PeerCount: len(members), Peers: members}
		if len(item.Peers) > 10 {
			item.Peers = item.Peers[:10]
			out.Truncated = true
		}
		out.Items = append(out.Items, item)
	}
	// Candidate instants are strictly ordered, so insertion order is already
	// first instant then peer-set key (two sets cannot first appear at one instant).
	if len(out.Items) > 20 {
		out.Items = out.Items[:20]
		out.Truncated = true
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// Both nonempty string pins must match; other JSON values are wildcards.
// Executor labels are an adapter convention, not an enforced Kata capability.
func pinsDisjoint(a, b jsontext.Value) bool {
	var left, right map[string]jsontext.Value
	_ = json.Unmarshal(a, &left)
	_ = json.Unmarshal(b, &right)
	for _, key := range []string{"adapter", "executor"} {
		var x, y string
		if left[key].Kind() != '"' || right[key].Kind() != '"' {
			continue
		}
		_ = json.Unmarshal(left[key], &x)
		_ = json.Unmarshal(right[key], &y)
		if x != "" && y != "" && x != y {
			return true
		}
	}
	return false
}

func collisionInstants(job JobDefinition) bool {
	if (IsRecurringIssueJob(job) && job.Trigger.Kind == "interval") || job.Trigger.Kind == "once" {
		return true
	}
	if job.Trigger.Kind != "cron" {
		return false
	}
	schedule, err := adapterCronSchedule(job.Trigger)
	return err == nil && schedule != nil && !cronparser.IsTriggered(schedule)
}
