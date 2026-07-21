package daemon

func registerProjectOperationPolicies(policies map[string]HostOperationPolicy) {
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationProjectRead, Capability: hostCapabilityRead,
	}, "listProjects", "showProject")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationProjectAdministration, Capability: hostCapabilityManage,
		Mutation: true, restricted: true,
	}, "resolveProject", "initProject", "mergeProject", "removeProject", "purgeProject",
		"restoreProject", "detachProjectAlias", "renameProject", "patchProjectMetadata")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTokenAdministration, Capability: hostCapabilityManage,
		restricted: true,
	}, "listTokens")
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationTokenAdministration, Capability: hostCapabilityManage,
		Mutation: true, restricted: true,
	}, "createToken", "revokeToken")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationIntegrationAdministration, Capability: hostCapabilityManage,
		restricted: true,
	}, "getIssueSyncStatus")
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationIntegrationAdministration, Capability: hostCapabilityManage,
		Mutation: true, restricted: true,
	}, "enableIssueSync", "disableIssueSync", "runIssueSyncOnce")
}
