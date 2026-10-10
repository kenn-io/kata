package db

import (
	"fmt"
	"strings"
)

// federationEventType declares one event type that federation transports:
// the wire version and feature that introduced it, and whether it is a
// baseline snapshot. Wire compatibility, push queries and snapshot checks all
// derive from federationEventTypes, so a new type is declared once.
type federationEventType struct {
	name     string
	version  int
	feature  string
	snapshot bool
}

var federationEventTypes = []federationEventType{
	{name: "project.metadata_updated", version: 30},
	{name: "issue.created", version: 30},
	{name: "issue.snapshot", version: 30, snapshot: true},
	{name: "issue.updated", version: 30},
	{name: "issue.closed", version: 30},
	{name: "issue.reopened", version: 30},
	{name: "issue.soft_deleted", version: 30},
	{name: "issue.restored", version: 30},
	{name: "issue.commented", version: 30},
	{name: "issue.comment_edited", version: 30},
	{name: "issue.assigned", version: 30},
	{name: "issue.unassigned", version: 30},
	{name: "issue.assignment_renewed", version: 30},
	{name: "issue.assignment_expired", version: 30},
	{name: "issue.priority_set", version: 30},
	{name: "issue.priority_cleared", version: 30},
	{name: "issue.labeled", version: 30},
	{name: "issue.unlabeled", version: 30},
	{name: "issue.linked", version: 30},
	{name: "issue.unlinked", version: 30},
	{name: "issue.links_changed", version: 30},
	{name: "issue.metadata_updated", version: 30},
	{name: "issue.external_root_bound", version: 30},
	{name: "issue.external_root_paused", version: 30},
	{name: "issue.external_root_resumed", version: 30},
	{name: "issue.external_root_unbound", version: 30},
	{name: "issue.external_comment_resolved", version: 30},
	{name: "issue.external_field_conflicted", version: 30},
	{name: "issue.external_field_resolved", version: 30},
	{name: "cron.job.created", version: 31, feature: CronEventFeature},
	{name: "cron.job.updated", version: 31, feature: CronEventFeature},
	{name: "cron.job.deleted", version: 31, feature: CronEventFeature},
	{name: "cron.job.restored", version: 31, feature: CronEventFeature},
	{name: "cron.job.snapshot", version: 31, feature: CronEventFeature, snapshot: true},
	{name: "cron.workflow.created", version: 31, feature: CronEventFeature},
	{name: "cron.workflow.updated", version: 31, feature: CronEventFeature},
	{name: "cron.workflow.deleted", version: 31, feature: CronEventFeature},
	{name: "cron.workflow.restored", version: 31, feature: CronEventFeature},
	{name: "cron.workflow.snapshot", version: 31, feature: CronEventFeature, snapshot: true},
	{name: "cron.run.observed", version: 31, feature: CronEventFeature},
	{name: "cron.run.snapshot", version: 31, feature: CronEventFeature, snapshot: true},
}

func lookupFederationEventType(kind string) (federationEventType, bool) {
	for _, declared := range federationEventTypes {
		if declared.name == kind {
			return declared, true
		}
	}
	return federationEventType{}, false
}

// FederationEventWireVersion is an explicit wire-content compatibility map,
// independent of later storage schema upgrades. New event types must declare
// their required wire version/feature before any client can publish them.
func FederationEventWireVersion(kind string) (int, string, error) {
	declared, ok := lookupFederationEventType(kind)
	if !ok {
		return 0, "", fmt.Errorf("%w: no wire compatibility declared for %s", ErrUnsupportedEventFeatures, kind)
	}
	return declared.version, declared.feature, nil
}

// IsFederationSnapshotEvent identifies a baseline document, including dormant
// cron history. It does not grant snapshot-author preservation.
func IsFederationSnapshotEvent(kind string) bool {
	declared, ok := lookupFederationEventType(kind)
	return ok && declared.snapshot
}

func isCronDefinitionEvent(kind string) bool {
	_, ok := lookupFederationEventType(kind)
	return ok && (strings.HasPrefix(kind, "cron.job.") || strings.HasPrefix(kind, "cron.workflow."))
}

// FederationPushEventTypesSQL is the quoted SQL list of every event type a
// spoke pushes, for use inside IN (...).
func FederationPushEventTypesSQL() string {
	names := make([]string, 0, len(federationEventTypes))
	for _, declared := range federationEventTypes {
		names = append(names, declared.name)
	}
	return sqlStringList(names)
}

// FederationSnapshotEventTypesSQL is the quoted SQL list of baseline snapshot
// event types, for use inside IN (...).
func FederationSnapshotEventTypesSQL() string {
	var names []string
	for _, declared := range federationEventTypes {
		if declared.snapshot {
			names = append(names, declared.name)
		}
	}
	return sqlStringList(names)
}

// sqlStringList quotes fixed identifiers declared in this package. It is not
// for caller input.
func sqlStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "'" + value + "'"
	}
	return strings.Join(quoted, ",")
}
