package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func TestConstructorsAcceptUnixSocketEndpoints(t *testing.T) {
	isolateKataHome(t)
	open := katatest.New(t, katatest.WithUnixSocket())
	secured := katatest.New(t, katatest.WithUnixSocket(), katatest.WithToken("socket-token"))
	t.Setenv("KATA_AUTH_TOKEN", "socket-token")

	constructors := map[string]func() (*client.Client, error){
		"New": func() (*client.Client, error) {
			return client.New(open.Endpoint)
		},
		"NewWithGlobalAuth": func() (*client.Client, error) {
			return client.NewWithGlobalAuth(t.Context(), secured.Endpoint)
		},
		"NewWithBearer": func() (*client.Client, error) {
			return client.NewWithBearer(t.Context(), secured.Endpoint, "socket-token")
		},
		"NewForTarget": func() (*client.Client, error) {
			return client.NewForTarget(t.Context(), secured.Endpoint, client.TargetAuth{Token: "socket-token"})
		},
	}
	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			api, err := construct()
			require.NoError(t, err)
			assert.NotNil(t, listProjects(t, api))
		})
	}
}

func TestUnixSocketEndpointRequiresAbsolutePath(t *testing.T) {
	_, err := client.NewForTarget(t.Context(), "unix://relative.sock", client.TargetAuth{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestStatusCodeReportsDaemonErrorEnvelope(t *testing.T) {
	server := katatest.New(t)

	_, err := server.Client(t).ShowProjectWithResponse(t.Context(), &generated.ShowProjectRequestOptions{
		PathParams: &generated.ShowProjectPath{ProjectID: 424242},
	})
	require.Error(t, err)

	assert.Equal(t, http.StatusNotFound, client.StatusCode(err))
	envelope, ok := errors.AsType[generated.ErrorEnvelope](err)
	require.True(t, ok)
	assert.Equal(t, "project_not_found", envelope.ErrorData.Code)
}

func TestStatusCodeReportsUndecodableErrorBody(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	t.Cleanup(proxy.Close)
	api, err := client.NewWithHTTPClient(proxy.URL, proxy.Client())
	require.NoError(t, err)

	_, err = api.ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.Error(t, err)

	assert.Equal(t, http.StatusBadGateway, client.StatusCode(err))
}

func TestStatusCodeIsZeroWithoutResponse(t *testing.T) {
	api, err := client.New("http://127.0.0.1:1")
	require.NoError(t, err)

	_, err = api.ListProjectsWithResponse(t.Context(), &generated.ListProjectsRequestOptions{})
	require.Error(t, err)

	assert.Zero(t, client.StatusCode(err))
}
