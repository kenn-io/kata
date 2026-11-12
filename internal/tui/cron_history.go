package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/pkg/client/generated"
)

// cronProjectObservations preserves the project's loaded link order and uses
// recorded identities only. Current definitions cannot supply a missing ID.
func cronProjectObservations(runs []generated.CronRun, ref cronRef) []generated.CronRun {
	var matching []generated.CronRun
	if ref.uid == "" || (ref.kind != cronJobs && ref.kind != cronWorkflows) {
		return matching
	}
	for _, r := range runs {
		uid := r.JobUID
		if ref.kind == cronWorkflows {
			uid = r.WorkflowUID
		}
		if uid != nil && *uid == ref.uid {
			matching = append(matching, r)
		}
	}
	return matching
}

// cronObservationSummary describes observations, not occurrences or a complete
// time window. Sort a copy for the strip so navigation keeps its existing order.
func cronObservationSummary(runs []generated.CronRun) string {
	if len(runs) == 0 {
		return "no matching runs in the latest project runs"
	}
	sample := "Sample: " + cronCount(len(runs), "observation")
	ordered := slices.Clone(runs)
	slices.SortFunc(ordered, func(a, b generated.CronRun) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return cmp.Compare(a.UID, b.UID)
	})
	var counts [5]int
	var strip strings.Builder
	for _, r := range ordered {
		i := 4
		switch r.Status {
		case "succeeded":
			i = 0
		case "failed":
			i = 1
		case "cancelled":
			i = 2
		case "running":
			i = 3
		}
		counts[i]++
		strip.WriteByte("SFCR?"[i])
	}
	return fmt.Sprintf("%s\nsucceeded %d · failed %d · cancelled %d · running %d · unknown %d\nOrder: %s\nOldest left, newest right; S succeeded, F failed, C cancelled, R running, ? unknown",
		sample, counts[0], counts[1], counts[2], counts[3], counts[4], strip.String())
}
