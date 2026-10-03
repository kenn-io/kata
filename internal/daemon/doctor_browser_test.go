package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
)

func TestDoctorRejectsBrowserAuthorityEvenWithOperatorToken(t *testing.T) {
	for _, kind := range []PrincipalKind{PrincipalStaticToken, PrincipalBootstrap} {
		t.Run(string(kind), func(t *testing.T) {
			mux := http.NewServeMux()
			registerDoctorHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), ServerConfig{})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor", nil)
			req = req.WithContext(withWebSession(context.Background(), Principal{Kind: kind}))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			require.Equal(t, http.StatusForbidden, rr.Code)
		})
	}
}
