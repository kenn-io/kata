package daemon

func registerTaskOperationPolicies(policies map[string]HostOperationPolicy) {
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskRead, Capability: hostCapabilityRead}, "listCronRequests", "getCronCapabilities", "showCronRun", "listCronRuns")
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskMutation, Capability: hostCapabilityWrite, Mutation: true}, "requestCronJobRun", "requestCronRunCancel", "observeCronRun", "pauseCronProject", "resumeCronProject")
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskRead, Capability: hostCapabilityRead}, "showCronJob", "listCronJobs", "showCronWorkflow", "listCronWorkflows")
	registerHostOperations(policies, HostOperationPolicy{Kind: hostOperationTaskMutation, Capability: hostCapabilityWrite, Mutation: true}, "createCronJob", "replaceCronJob", "archiveCronJob", "restoreCronJob", "pauseCronJob", "resumeCronJob", "createCronWorkflow", "replaceCronWorkflow", "archiveCronWorkflow", "restoreCronWorkflow")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskRead, Capability: hostCapabilityRead,
	}, "attachmentContent", "listAllIssues", "listIssues", "showIssue", "issuePlanningDates", "showIssueByUID", "getIssueMetadata", "reachableIssueGraph",
		"listLabels", "listRecurrences", "showRecurrence", "readyIssues", "readyIssuesGlobal",
		"searchIssues", "pollEvents", "pollProjectEvents", "auditCloses", "digestGlobal",
		"digestProject", "getIssueLeaseStatus", "readUISnapshot", "readUIReferences",
		"resolveUIIssueReference", "readUILaunchTarget")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskRead, Capability: hostCapabilityRead, LongLived: true,
	}, "streamEvents")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskMutation, Capability: hostCapabilityWrite, Mutation: true,
	}, "notifyIssue", "createIssue", "editIssue", "createComment", "editComment", "addLabel", "removeLabel",
		"createLink", "deleteLink", "assignIssue", "unassignIssue", "claimIssue",
		"setIssuePriority", "closeIssue", "reopenIssue", "deleteIssue", "restoreIssue",
		"createRecurrence", "patchRecurrence", "deleteRecurrence", "patchIssueMetadata", "addAttachment", "linkAttachment",
		"moveIssue", "importIssues", "acquireIssueLease", "renewIssueLease", "releaseIssueLease")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskAdministration, Capability: hostCapabilityManage, Mutation: true,
	}, "purgeIssue", "forceReleaseIssueLease", "rewriteAuthorIdentity")
}
