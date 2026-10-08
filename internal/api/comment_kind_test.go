package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
)

func TestCommentRequestCanonicalKindValidation(t *testing.T) {
	mux := http.NewServeMux()
	h := humago.New(mux, huma.DefaultConfig("Comment kinds", "0.26.0"))
	huma.Register(h, huma.Operation{OperationID: "comment-kind", Method: http.MethodPost, Path: "/projects/{project_id}/issues/{ref}/comments"}, func(_ context.Context, _ *CommentRequest) (*struct{ Body string }, error) {
		return &struct{ Body string }{Body: "accepted"}, nil
	})
	for _, tc := range []struct {
		kind   string
		status int
	}{{"reply", 200}, {"confirm", 200}, {"refute", 200}, {"supersede", 200}, {"answer", 422}, {"REPLY", 422}} {
		t.Run(tc.kind, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/projects/1/issues/abc4/comments", strings.NewReader(`{"body":"Response","reply_to":"c:abcdef","kind":"`+tc.kind+`"}`))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
}
