package daemon

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/twentysync"
)

func TestOutcomesTwentySemanticReasons(t *testing.T) {
	for _, tc := range []struct {
		code, reason string
	}{
		{"UNAUTHENTICATED", "auth"},
		{"FORBIDDEN", "auth"},
		{"RATE_LIMITED", "rate_limit"},
		{"UNKNOWN", "other"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			cfg := twentysync.Config{APIOrigin: "https://twenty.example", WebOrigin: "https://twenty.example", WorkspaceID: "11111111-1111-4111-8111-111111111111"}
			session, err := twentysync.NewClient(twentysync.ClientConfig{
				Daemon:    config.TwentySyncConfig{APIOrigin: cfg.APIOrigin},
				LookupEnv: func(string) (string, bool) { return "example-token", true },
				Transport: outcomeTransport(func(r *http.Request) (*http.Response, error) {
					require.Equal(t, "/metadata", r.URL.Path)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"errors":[{"extensions":{"code":"` + tc.code + `"}}]}`))}, nil
				}),
			}).ForRun(t.Context(), cfg)
			require.NoError(t, err)
			_, err = session.Workspace(t.Context(), cfg)
			require.Error(t, err)
			require.Equal(t, tc.reason, syncFailureReason(err))
		})
	}
}
