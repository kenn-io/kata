package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestAttachmentUploadRequiresName(t *testing.T) {
	env, f := attachmentEnv(t)
	project, issue := seedProjectAndIssue(t, env)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/attachments?actor=operator", env.URL, project.ID, issue.ShortID), strings.NewReader("abc"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
	require.Contains(t, string(raw), "name: required query parameter is missing")
	require.Empty(t, f.Requests())
}

func TestGeneratedClientAttachmentRoundTrip(t *testing.T) {
	for _, method := range []string{"AddAttachment", "AddAttachmentWithResponse"} {
		t.Run(method, func(t *testing.T) {
			env, _ := attachmentEnv(t)
			project, issue := seedProjectAndIssue(t, env)
			apiClient, err := runtime.NewAPIClient(env.URL, runtime.WithHTTPClient(contextHTTPClient{client: env.HTTP}))
			require.NoError(t, err)
			client := generated.NewClient(apiClient)
			var body generated.AddAttachmentBody
			body.InitFromBytes([]byte("abc"), "sample.txt")
			name, actor := "sample.txt", "operator"
			options := &generated.AddAttachmentRequestOptions{
				PathParams: &generated.AddAttachmentPath{ProjectID: project.ID, Ref: issue.ShortID},
				Query:      &generated.AddAttachmentQuery{Name: name, Actor: &actor},
				Body:       &body,
			}
			var id string
			if method == "AddAttachment" {
				upload, err := client.AddAttachment(t.Context(), options)
				require.NoError(t, err)
				id = upload.AttachmentID
			} else {
				upload, err := client.AddAttachmentWithResponse(t.Context(), options)
				require.NoError(t, err)
				require.Equal(t, 200, upload.StatusCode)
				require.NotNil(t, upload.JSON200)
				id = upload.JSON200.AttachmentID
			}
			content, err := client.AttachmentContent(t.Context(), &generated.AttachmentContentRequestOptions{
				PathParams: &generated.AttachmentContentPath{ProjectID: project.ID, Ref: issue.ShortID, AttachmentID: id},
			})
			require.NoError(t, err)
			require.Equal(t, []byte("abc"), []byte(*content))
		})
	}
}
