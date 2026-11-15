package docbank

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankMetadataReads(t *testing.T) {
	const vid = "6f1c2e8a-2222-4222-8222-222222222222"
	// Valid source metadata can exceed 1MiB while each value stays below 64KiB.
	var fields strings.Builder
	for i := range 20 {
		if i > 0 {
			fields.WriteByte(',')
		}
		fmt.Fprintf(&fields, `{"key":"office.custom.notes_%02d","namespace":"office.custom","sensitive":false,"source_field":"Notes %02d","value":{"kind":"string","string":%q}}`, i, i, strings.Repeat("x", 60<<10))
	}
	record := fmt.Sprintf(`{"contract_version":"source-metadata/v1","fields":[%s],"warnings":[]}`, fields.String())
	meta := fmt.Sprintf(`,"source_metadata":%s`, record)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/path", "/api/v1/nodes/42":
			_, _ = fmt.Fprintf(w, `{"id":42,"name":"report.pdf","kind":"file","current_version_id":%q,"trashed_at":"","revision":3%s}`, vid, meta)
		case "/api/v1/versions/" + vid:
			_, _ = fmt.Fprintf(w, `{"id":%q,"node_id":42,"size":12,"mime_type":"application/pdf","blob_hash":"abc"%s}`, vid, meta)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewClient(server.URL, func() (string, error) { return "test-key", nil })
	node, err := c.ResolvePath(t.Context(), "/reports/report.pdf")
	require.NoError(t, err)
	require.EqualValues(t, 42, node.ID)
	require.Equal(t, "report.pdf", node.Name)
	require.Equal(t, "file", node.Kind)
	require.Equal(t, uuid.MustParse(vid), node.CurrentVersionID)
	byID, err := c.Node(t.Context(), 42)
	require.NoError(t, err)
	require.Equal(t, node, byID)
	version, err := c.Version(t.Context(), uuid.MustParse(vid))
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(vid), version.ID)
	require.EqualValues(t, 42, version.NodeID)
	require.EqualValues(t, 12, version.Size)
	require.Equal(t, []string{"/api/v1/path", "/api/v1/nodes/42", "/api/v1/versions/" + vid}, paths)
}
