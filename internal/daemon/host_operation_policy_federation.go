package daemon

func registerFederationOperationPolicies(policies map[string]HostOperationPolicy) {
	// Publication creates a native project; it grants no federation authority.
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationProjectAdministration, Capability: hostCapabilityManage,
		Mutation: true, restricted: true,
	}, "publish-federation-project")
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationRead, Capability: hostCapabilityRead,
	}, "getFederationStatus", "getProjectFederation", "getProjectFederationStatus", "getFederationBridgeStatus")

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
		"leaveFederationReplica", "rebindFederationReplica", "configureFederationSigning",
		"connectFederationBridge", "disconnectFederationBridge", "repinFederationRootKey")

	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationTransport, Capability: hostCapabilityFederate,
	}, "getFederationProjectMetadata", "pollFederationProjectEvents")
	registerHostOperations(policies, HostOperationPolicy{
		Kind: hostOperationFederationTransport, Capability: hostCapabilityFederate, Mutation: true,
	}, "disconnectRelayEnrollment", "ingestFederationProjectEvents", "getRelayReset", "offerRelayDeliveries", "acceptRelayDeliveries", "ackRelayDeliveries")
}
