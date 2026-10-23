package daemon

func registerTaskOperationPolicies(policies map[string]HostOperationPolicy) {
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskRead, Capability: hostCapabilityRead}, "getCronCapabilities", "showCronRun", "listCronRuns")
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskMutation, Capability: hostCapabilityWrite, Mutation: true}, "observeCronRun")
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskRead, Capability: hostCapabilityRead}, "showCronJob", "listCronJobs", "showCronWorkflow", "listCronWorkflows")
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskMutation, Capability: hostCapabilityWrite, Mutation: true}, "createCronJob", "replaceCronJob", "archiveCronJob", "restoreCronJob", "createCronWorkflow", "replaceCronWorkflow", "archiveCronWorkflow", "restoreCronWorkflow")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskRead, Capability: hostCapabilityRead,
	}, "listAllIssues", "listIssues", "showIssue", "issuePlanningDates", "showIssueByUID", "getIssueMetadata", "reachableIssueGraph",
		"listLabels", "listRecurrences", "showRecurrence", "readyIssues", "readyIssuesGlobal",
		"searchIssues", "pollEvents", "pollProjectEvents", "getProjectEventHead", "auditCloses", "digestGlobal",
		"digestProject", "getIssueLeaseStatus", "readUISnapshot", "readUIReferences",
		"resolveUIIssueReference", "readUILaunchTarget")

	// MCP Events subscriptions are daemon-owned delivery state, not project
	// data. Delivery re-authorizes with the read policy of pollEvents.
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskRead, Capability: hostCapabilityRead,
	}, "mcpEventsList", "mcpEventsSubscribe", "mcpEventsUnsubscribe", "mcpEventSubscriptions")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskRead, Capability: hostCapabilityRead, LongLived: true,
	}, "streamEvents")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskMutation, Capability: hostCapabilityWrite, Mutation: true,
	}, "createIssue", "editIssue", "createComment", "editComment", "addLabel", "removeLabel",
		"createLink", "deleteLink", "assignIssue", "unassignIssue", "claimIssue",
		"setIssuePriority", "closeIssue", "reopenIssue", "deleteIssue", "restoreIssue",
		"createRecurrence", "patchRecurrence", "deleteRecurrence", "patchIssueMetadata",
		"moveIssue", "importIssues", "acquireIssueLease", "renewIssueLease", "releaseIssueLease")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskAdministration, Capability: hostCapabilityManage, Mutation: true,
	}, "purgeIssue", "forceReleaseIssueLease", "rewriteAuthorIdentity")
}
