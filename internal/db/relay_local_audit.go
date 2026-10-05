package db

// RelayEventIsLocalAudit identifies records about one instance's project catalog,
// transport, recurrence scheduling or root-arbitrated claims. They stay in its local audit journal;
// shared content and unknown data events retain relay delivery intent.
func RelayEventIsLocalAudit(eventType string) bool {
	switch eventType {
	case "project.created", "project.renamed", "project.merged", "project.author_rewritten", "project.federation_enabled", "project.alias_removed",
		"recurrence.created", "recurrence.updated", "recurrence.deleted", "recurrence.materialized", "recurrence.materialization_skipped",
		"close.throttled", "claim.acquired", "claim.released", "claim.expired", "claim.force_released", "claim.violated":
		return true
	default:
		return false
	}
}
