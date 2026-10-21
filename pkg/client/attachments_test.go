package client

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestGeneratedAttachmentMultipart(t *testing.T) {
	for _, withResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "response"}[withResponse], func(t *testing.T) {
			binary := []byte{0x89, 0x50, 0, 0xff, 0xfe, 0x22, 0x5c, 0x0d, 0x0a}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				kind, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				require.NoError(t, err)
				require.Equal(t, "multipart/form-data", kind)
				if params["boundary"] == "" {
					http.Error(w, "missing multipart boundary", 400)
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
				require.NoError(t, r.ParseMultipartForm(1024)) //nolint:gosec // G120: bounded above with MaxBytesReader in this test server.
				defer func() { _ = r.MultipartForm.RemoveAll() }()
				require.Equal(t, "operator", r.FormValue("actor"))
				require.Equal(t, "01JB3Z6V2K8M4Q9T1X5W7Y0RNA", r.FormValue("attachment_id"))
				file, header, err := r.FormFile("file")
				require.NoError(t, err)
				defer func() { _ = file.Close() }()
				require.Equal(t, "screen.png", header.Filename)
				data, err := io.ReadAll(file)
				require.NoError(t, err)
				require.Equal(t, binary, data)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"attachment_id":"01JB3Z6V2K8M4Q9T1X5W7Y0RNA","attachment":{}}`)
			}))
			defer server.Close()
			api, err := New(server.URL)
			require.NoError(t, err)
			var file runtime.File
			file.InitFromBytes(binary, "screen.png")
			actor := "operator"
			options := &generated.AddAttachmentRequestOptions{PathParams: &generated.AddAttachmentPath{ProjectID: 42, Ref: "abc4"}, Body: &generated.AddAttachmentBody{Actor: &actor, AttachmentID: "01JB3Z6V2K8M4Q9T1X5W7Y0RNA", File: file}}
			if withResponse {
				result, err := api.AddAttachmentWithResponse(t.Context(), options)
				require.NoError(t, err)
				require.NotNil(t, result.JSON200)
			} else {
				result, err := api.AddAttachment(t.Context(), options)
				require.NoError(t, err)
				require.Equal(t, options.Body.AttachmentID, result.AttachmentID)
			}
		})
	}
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

func TestGeneratedAttachmentTextByteLimitUsesDecimalInteger(t *testing.T) {
	for _, withResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "response"}[withResponse], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "16777216", r.URL.Query().Get("max_bytes"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"state":"unprocessed","truncated":false}`)
			}))
			defer server.Close()
			api, err := New(server.URL)
			require.NoError(t, err)
			limit := int64(16 << 20)
			options := &generated.AttachmentTextRequestOptions{PathParams: &generated.AttachmentTextPath{ProjectID: 42, Ref: "abc4", AttachmentID: "example"}, Query: &generated.AttachmentTextQuery{MaxBytes: &limit}}
			if withResponse {
				_, err = api.AttachmentTextWithResponse(t.Context(), options)
			} else {
				_, err = api.AttachmentText(t.Context(), options)
			}
			require.NoError(t, err)
		})
	}
}
