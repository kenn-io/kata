package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedObjectCommitments(t *testing.T) {
	v, _, _ := verificationView(t)
	result := SourceResult{OperationUID: verificationID(50), OperationDigest: strings.Repeat("a", 64), State: "rejected", ReasonCode: "conflict"}
	resultDigest, err := ScopedSourceResultObjectDigest(result)
	require.NoError(t, err)
	require.Equal(t, "763c5d92f2bf955376c057df236fae0a77491857f2a073ae831af8e249a22b89", resultDigest)
	result.Signatures = []ScopedSignature{{ProjectUID: verificationID(1), KeyID: strings.Repeat("c", 64), Signature: "outer"}}
	same, err := ScopedSourceResultObjectDigest(result)
	require.NoError(t, err)
	require.Equal(t, resultDigest, same)
	result.ReasonCode = "canceled"
	changed, err := ScopedSourceResultObjectDigest(result)
	require.NoError(t, err)
	require.NotEqual(t, resultDigest, changed)
	result.State = "future"
	_, err = ScopedSourceResultObjectDigest(result)
	require.Error(t, err)
	coverage := SourceCoverage{OperationUID: verificationID(50), OperationDigest: strings.Repeat("a", 64), View: v.Key, Cut: ScopedCut{Sequence: 9223372036854775807, Digest: strings.Repeat("b", 64), CaptureUID: verificationID(21)}, Outcome: "outside_scope"}
	_, err = ScopedSourceCoverageObjectDigest(coverage)
	require.NoError(t, err)
	coverage.Outcome = "future"
	_, err = ScopedSourceCoverageObjectDigest(coverage)
	require.Error(t, err)
	request := ScopedControlRequest{RequestUID: verificationID(60), Digest: strings.Repeat("a", 64), Kind: "status", Subject: ScopedEndpoint{ProjectUID: verificationID(1), IssueUID: verificationID(40)}, Principal: ScopedControlPrincipal{HolderInstanceUID: v.DestinationUID, Holder: "member", ClientKind: "cli"}}
	requestDigest, err := ScopedControlRequestObjectDigest(request)
	require.NoError(t, err)
	require.Equal(t, "30f944e2b1a43a68a585057af7c65340a57e458bcc73431f725f1001bf4d472f", requestDigest)
	controlResult := ScopedControlResult{RequestUID: request.RequestUID, Digest: request.Digest, State: "pending"}
	d, err := ScopedControlResultObjectDigest(controlResult)
	require.NoError(t, err)
	require.NotEqual(t, requestDigest, d)
	conversion := ScopedConversionRequest{UID: verificationID(60), Digest: strings.Repeat("a", 64), CatalogEntryUID: verificationID(61), RouteDigest: v.RouteDigest, Target: v, ProjectUIDs: []string{verificationID(1), verificationID(2)}}
	requestDigest, err = ScopedConversionRequestObjectDigest(conversion)
	require.NoError(t, err)
	state := ScopedConversionState{Request: conversion, Phase: "prepare", OldBindings: []ScopedOldBinding{}}
	d, err = ScopedConversionStateObjectDigest(state)
	require.NoError(t, err)
	require.NotEqual(t, requestDigest, d)
	state.Phase = "future"
	_, err = ScopedConversionStateObjectDigest(state)
	require.Error(t, err)
	cutover := ScopedProtocolCutoverCertificate{Version: 1, Cutover: ScopedProtocolCutoverExpectation{UID: verificationID(60), AuthorityUID: v.AuthorityUID, RequestDigest: strings.Repeat("a", 64), ProjectUIDs: conversion.ProjectUIDs, Floor: 2, ReadinessDigest: strings.Repeat("b", 64), SuspendedBindingsDigest: strings.Repeat("c", 64)}}
	d, err = ScopedCutoverCertificateObjectDigest(cutover)
	require.NoError(t, err)
	require.NotEqual(t, requestDigest, d)
	cutover.Version = 0
	_, err = ScopedCutoverCertificateObjectDigest(cutover)
	require.Error(t, err)
}
