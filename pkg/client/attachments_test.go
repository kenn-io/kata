package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestGeneratedAddAttachmentEncodesTypedMultipartBody(t *testing.T) {
	data := []byte{0x89, 0x50, 0, 0xff, 0xfe}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/42/issues/abc4/attachments", r.URL.Path)
		require.Equal(t, "operator", r.URL.Query().Get("actor"))
		reader, err := r.MultipartReader()
		require.NoError(t, err)
		fields := map[string]string{}
		fileCount := 0
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			body, err := io.ReadAll(part)
			require.NoError(t, err)
			if part.FormName() == "file" {
				fileCount++
				require.Equal(t, "screen.png", part.FileName())
				require.Equal(t, data, body)
			} else {
				fields[part.FormName()] = string(body)
			}
		}
		require.Equal(t, 1, fileCount)
		require.Equal(t, map[string]string{"attachment_id": "01JB3Z6V2K8M4Q9T1X5W7Y0RNA"}, fields)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"attachment_id":"01JB3Z6V2K8M4Q9T1X5W7Y0RNA","attachment":{}}`)
	}))
	defer server.Close()
	api, err := New(server.URL)
	require.NoError(t, err)
	var file runtime.File
	file.InitFromBytes(data, "screen.png")
	actor := "operator"
	_, err = api.AddAttachment(t.Context(), &generated.AddAttachmentRequestOptions{
		PathParams: &generated.AddAttachmentPath{ProjectID: 42, Ref: "abc4"},
		Query:      &generated.AddAttachmentQuery{Actor: &actor},
		Body:       &generated.AddAttachmentBody{AttachmentID: "01JB3Z6V2K8M4Q9T1X5W7Y0RNA", File: file},
	})
	require.NoError(t, err)
}

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
