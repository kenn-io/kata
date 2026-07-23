package daemon

func registerFederationOperationPolicies(policies map[string]HostOperationPolicy) {
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationRead, Capability: hostCapabilityRead,
	}, "getFederationStatus", "getProjectFederation", "getProjectFederationStatus")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationAdministration, Capability: hostCapabilityFederate,
		restricted: true,
	}, "listFederationEnrollments")
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationAdministration, Capability: hostCapabilityFederate,
		Mutation: true, restricted: true,
	}, "enableProjectFederation", "skipFederationQuarantine", "retryFederationQuarantine",
		"createFederationEnrollment", "rotateFederationEnrollment",
		"revokeFederationEnrollment", "createFederationReplica",
		"leaveFederationReplica")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationTransport, Capability: hostCapabilityFederate,
	}, "getFederationProjectMetadata", "pollFederationProjectEvents")
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationTransport, Capability: hostCapabilityFederate, Mutation: true,
	}, "ingestFederationProjectEvents")
}
