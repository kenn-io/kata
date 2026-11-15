package kata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type peerEnrollmentRecorder struct{ request FederationAccessRequest }

func (r *peerEnrollmentRecorder) AuthorizeFederation(_ context.Context, request FederationAccessRequest) (FederationAccessDecision, error) {
	r.request = request
	return FederationAccessDecision{}, nil
}

func TestFederationPeerAdapterEnrollmentProject(t *testing.T) {
	recorder := &peerEnrollmentRecorder{}
	adapter := hostFederationAccessControllerAdapter{controller: recorder}
	_, err := adapter.AuthorizeFederation(t.Context(), daemon.HostFederationAccessRequest{Enrollment: db.FederationEnrollment{ID: 42, Actor: "member"}, EnrollmentProjectUID: "enrollment-project", Project: db.Project{UID: "peer-project"}, Capability: "pull", Operation: daemon.HostFederationOperation{ID: "getRelayReset", Mutation: true}})
	require.NoError(t, err)
	require.Equal(t, "enrollment-project", recorder.request.Enrollment.ProjectUID)
	require.Equal(t, "peer-project", recorder.request.Project.UID)
}
