package daemon

func registerTaskOperationPolicies(policies map[string]HostOperationPolicy) {
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTaskRead, Capability: hostCapabilityRead,
	}, "listAllIssues", "listIssues", "showIssue", "showIssueByUID", "reachableIssueGraph",
		"listLabels", "listRecurrences", "showRecurrence", "readyIssues", "readyIssuesGlobal",
		"searchIssues", "pollEvents", "pollProjectEvents", "auditCloses", "digestGlobal",
		"digestProject", "getIssueLeaseStatus", "readUISnapshot", "readUIReferences")

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
