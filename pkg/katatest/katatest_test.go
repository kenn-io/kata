package katatest_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func TestServerPersistsProjectsAcrossClients(t *testing.T) {
	server := katatest.New(t)

	name, actor := "spoke-project", "tester"
	created, err := server.Client(t).InitProjectWithResponse(t.Context(), &generated.InitProjectRequestOptions{
		Body: &generated.InitProjectBody{Name: &name, Actor: &actor},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, created.StatusCode)

	other, err := client.NewForTarget(t.Context(), server.Endpoint, client.TargetAuth{})
	require.NoError(t, err)
	listed, err := other.ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.NoError(t, err)
	require.Len(t, listed.JSON200.Projects, 1)
	assert.Equal(t, "spoke-project", listed.JSON200.Projects[0].Name)
	assert.Equal(t, created.JSON200.Project.UID, listed.JSON200.Projects[0].UID)
}

func TestServerWithTokenRequiresBearer(t *testing.T) {
	server := katatest.New(t, katatest.WithToken("test-token"))

	anonymous, err := client.NewForTarget(t.Context(), server.Endpoint, client.TargetAuth{})
	require.NoError(t, err)
	_, err = anonymous.ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, client.StatusCode(err))

	listed, err := server.Client(t).ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, listed.StatusCode)
}

func TestServerWithUnixSocketServesUnixEndpoint(t *testing.T) {
	server := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("test-token"))
	require.Regexp(t, `^unix://`, server.Endpoint)

	listed, err := server.Client(t).ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, listed.StatusCode)
}
