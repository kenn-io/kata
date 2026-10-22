package db

// RelayEventIsLocalAudit identifies records about one instance's project catalog,
// transport, recurrence scheduling, reached schedules or root-arbitrated claims.
// They stay in its local audit journal; shared content and unknown data events
// retain relay delivery intent.
func RelayEventIsLocalAudit(eventType string) bool {
	switch eventType {
	case "project.created", "project.renamed", "project.merged", "project.author_rewritten", "project.federation_enabled", "project.alias_removed",
		"recurrence.created", "recurrence.updated", "recurrence.deleted", "recurrence.materialized", "recurrence.materialization_skipped",
		"close.throttled", "claim.acquired", "claim.released", "claim.expired", "claim.force_released", "claim.violated",
		"issue.scheduled_reached":
		return true
	default:
		return false
	}
}

// RelayResetRetainedEventTypesSQL is the quoted SQL list of daemon-local
// catalog records a relay reset keeps. They describe this instance's project
// catalog, carry no root receipt, and never feed the replica projection fold,
// unlike an author rewrite, which the authoritative snapshot supersedes.
func RelayResetRetainedEventTypesSQL() string {
	return sqlStringList([]string{"project.created", "project.renamed", "project.merged", "project.alias_removed", "project.removed", "project.restored"})
}
