package federation_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

// Browse uses the origin's exposed subtree, including configured-folder
// defaults. Local vaults and Docbank source metadata are not a browse source.
func TestRelayDocbankBrowseUsesOriginAndBoundsPages(t *testing.T) {
	root, relay, leaf, _ := newRelayClaimChain(t)
	origin, middle, local := docbanktest.New(t), docbanktest.New(t), docbanktest.New(t)
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222220")
	origin.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
	origin.Seed("/kata/sub", docbank.Node{ID: 11, Name: "sub", Kind: "dir"}, nil)
	origin.Seed("/kata/file.pdf", docbank.Node{ID: 12, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("browse fixture")})
	origin.Seed("/kata/sub/nested.pdf", docbank.Node{ID: 13, Name: "nested.pdf", Kind: "file", CurrentVersionID: version}, nil)
	serveRelayDocbank(t, root, origin)
	serveRelayDocbank(t, relay, middle)
	serveRelayDocbank(t, leaf, local)
	base := fmt.Sprintf("/api/v1/projects/%d/docbank", leaf.project.ID)
	var paths []string
	for offset := range 2 {
		status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("%s/children?limit=1&offset=%d", base, offset), "", "")
		require.Equal(t, http.StatusOK, status, string(raw))
		var page struct {
			Origin    string `json:"origin"`
			Directory struct {
				Path string `json:"path"`
			} `json:"directory"`
			Items []struct {
				Path string `json:"path"`
			} `json:"items"`
			Total  int `json:"total"`
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Equal(t, root.store.InstanceUID(), page.Origin)
		require.Equal(t, "/kata", page.Directory.Path)
		require.Equal(t, 2, page.Total)
		require.Equal(t, 1, page.Limit)
		require.Equal(t, offset, page.Offset)
		require.Len(t, page.Items, 1)
		paths = append(paths, page.Items[0].Path)
		require.NotContains(t, string(raw), "source_metadata")
	}
	require.ElementsMatch(t, []string{"/kata/sub", "/kata/file.pdf"}, paths)
	status, raw := relayDocbankRequest(t, leaf, http.MethodGet, base+"/resolve?"+url.Values{"path": {"/kata/file.pdf"}}.Encode(), "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var resolved struct {
		Origin string `json:"origin"`
		Node   struct {
			ID               int64     `json:"id"`
			Path             string    `json:"path"`
			CurrentVersionID uuid.UUID `json:"current_version_id"`
		} `json:"node"`
	}
	require.NoError(t, json.Unmarshal(raw, &resolved))
	require.Equal(t, root.store.InstanceUID(), resolved.Origin)
	require.Equal(t, int64(12), resolved.Node.ID)
	require.Equal(t, "/kata/file.pdf", resolved.Node.Path)
	require.Equal(t, version, resolved.Node.CurrentVersionID)
	require.Empty(t, middle.Requests())
	require.Empty(t, local.Requests())
}

func TestRelayDocbankBrowseRejectsEscapeAndUnboundedPage(t *testing.T) {
	root, _, leaf, _ := newRelayClaimChain(t)
	origin := docbanktest.New(t)
	origin.Seed("/kata-other", docbank.Node{ID: 20, Name: "kata-other", Kind: "dir"}, nil)
	origin.Seed("/private", docbank.Node{ID: 21, Name: "private", Kind: "dir"}, nil)
	serveRelayDocbank(t, root, origin)
	base := fmt.Sprintf("/api/v1/projects/%d/docbank", leaf.project.ID)
	for _, operation := range []string{"children", "resolve"} {
		for _, tc := range []struct {
			path   string
			status int
		}{{"/kata-other", 403}, {"/private", 403}, {"/kata/../private", 400}, {"relative", 400}} {
			status, raw := relayDocbankRequest(t, leaf, http.MethodGet, base+"/"+operation+"?"+url.Values{"path": {tc.path}}.Encode(), "", "")
			require.Equal(t, tc.status, status, string(raw))
		}
	}
	for _, query := range []string{"limit=201", "limit=0", "offset=-1"} {
		status, raw := relayDocbankRequest(t, leaf, http.MethodGet, base+"/children?"+query, "", "")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
	}
	root.http.Close()
	status, raw := relayDocbankRequest(t, leaf, http.MethodGet, base+"/children", "", "")
	require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
}
