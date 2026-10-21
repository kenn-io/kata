package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestGeneratedAttachmentContentPreservesBytes(t *testing.T) {
	for _, data := range [][]byte{{0x89, 0x50, 0, 0xff, 0xfe}, []byte("plain text"), []byte(`"Ynl0ZXM="`)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
		}))
		api, err := New(server.URL)
		require.NoError(t, err)
		result, err := api.AttachmentContent(t.Context(), &generated.AttachmentContentRequestOptions{PathParams: &generated.AttachmentContentPath{ProjectID: 42, Ref: "abc4", AttachmentID: "01JB3Z6V2K8M4Q9T1X5W7Y0RNA"}})
		server.Close()
		require.NoError(t, err)
		got, err := result.Bytes()
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
}
