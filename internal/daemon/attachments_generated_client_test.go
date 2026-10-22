package daemon_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

type attachmentAPIClient struct {
	runtime.APIClient
	t *testing.T
}

func (c attachmentAPIClient) CreateRequest(ctx context.Context, params runtime.RequestOptionsParameters, editors ...runtime.RequestEditorFn) (*http.Request, error) {
	if params.ContentType == "application/octet-stream" {
		require.True(c.t, params.Options.GetBody() == nil, "binary upload must bypass runtime body encoding")
	}
	return c.APIClient.CreateRequest(ctx, params, editors...)
}

func TestGeneratedClientAttachmentRoundTrip(t *testing.T) {
	for _, method := range []string{"AddAttachment", "AddAttachmentWithResponse"} {
		t.Run(method, func(t *testing.T) {
			env, _ := attachmentEnv(t)
			project, issue := seedProjectAndIssue(t, env)
			apiClient, err := runtime.NewAPIClient(env.URL, runtime.WithHTTPClient(contextHTTPClient{client: env.HTTP}))
			require.NoError(t, err)
			client := generated.NewClient(attachmentAPIClient{APIClient: apiClient, t: t})
			var body generated.AddAttachmentBody
			body.InitFromBytes([]byte("abc"), "sample.txt")
			name, actor := "sample.txt", "operator"
			options := &generated.AddAttachmentRequestOptions{
				PathParams: &generated.AddAttachmentPath{ProjectID: project.ID, Ref: issue.ShortID},
				Query:      &generated.AddAttachmentQuery{Name: &name, Actor: &actor},
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
