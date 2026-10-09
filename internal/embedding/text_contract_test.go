package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// These fixed goldens were captured from the pre-change client, not recomputed
// from the implementation under test.
func TestEmptyTextControlsPreserveIdentities(t *testing.T) {
	for _, tc := range []struct{ salt, generation, space string }{
		{"", "2c56e86b15bbb89a", "be5a5089846228fd"},
		{"example-revision", "812a1c8e4cc04bb8", "347a80bfd779ff93"},
	} {
		c, err := New(Config{BaseURL: "http://127.0.0.1:9/v1", Model: "example-model", Dims: 768, Salt: tc.salt})
		require.NoError(t, err)
		require.Equal(t, tc.generation, c.Generation().Fingerprint())
		g, err := c.Space().Generation()
		require.NoError(t, err)
		require.Equal(t, tc.space, g.Fingerprint())
		matches, err := c.Space().Matches(tc.generation)
		require.NoError(t, err)
		require.True(t, matches, "existing generations must stay current")
	}
}

// The new controls opt into different vectors. Every literal byte, including
// whitespace, must participate in identity, and old vectors must not match.
func FuzzTextControlIdentity(f *testing.F) {
	f.Add(uint8(0), "task: search result | query: ")
	f.Add(uint8(1), " ")
	f.Add(uint8(2), "\x00\n")
	f.Add(uint8(3), "")
	f.Add(uint8(4), "true")
	f.Fuzz(func(t *testing.T, selector uint8, value string) {
		cfg := Config{BaseURL: "http://127.0.0.1:9/v1", Model: "example-model", Dims: 768}
		old, err := New(cfg)
		require.NoError(t, err)
		changed := true
		if selector%5 == 4 {
			cfg.RequestDimensions = true
		} else {
			fields := []*string{&cfg.DocumentPrefix, &cfg.DocumentSuffix, &cfg.QueryPrefix, &cfg.QuerySuffix}
			*fields[selector%5] = value
			changed = value != ""
		}
		c, err := New(cfg)
		require.NoError(t, err)
		oldSpace, err := old.Space().Generation()
		require.NoError(t, err)
		space, err := c.Space().Generation()
		require.NoError(t, err)
		if !changed {
			require.Equal(t, old.Generation().Fingerprint(), c.Generation().Fingerprint())
			require.Equal(t, oldSpace.Fingerprint(), space.Fingerprint())
			return
		}
		require.NotEqual(t, old.Generation().Fingerprint(), c.Generation().Fingerprint())
		require.NotEqual(t, oldSpace.Fingerprint(), space.Fingerprint())
		matches, err := c.Space().Matches(old.Generation().Fingerprint())
		require.NoError(t, err)
		require.False(t, matches)
	})
}

func TestTextRoleHTTPContract(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "literal"}[optIn], func(t *testing.T) {
			var inputs [][]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Input      []string `json:"input"`
					Dimensions *int     `json:"dimensions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if req.Dimensions != nil {
					t.Errorf("native width must not be requested by default: %v", *req.Dimensions)
				}
				inputs = append(inputs, req.Input)
				v := make([]float32, 768)
				v[0] = 3
				v[1] = 4
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v}}})
			}))
			defer srv.Close()
			cfg := Config{BaseURL: srv.URL, Model: "example-model", Dims: 768}
			if optIn {
				cfg.DocumentPrefix = "title: none | text: "
				cfg.QueryPrefix = "task: search result | query: "
				cfg.DocumentSuffix = " document end "
				cfg.QuerySuffix = " query end "
			}
			c, err := New(cfg)
			require.NoError(t, err)
			document := EmbedText("repair login", "session expired")
			query := "login"
			_, err = c.EncodeFunc()(context.Background(), []string{document})
			require.NoError(t, err)
			_, err = c.EmbedQuery(context.Background(), []string{query})
			require.NoError(t, err)
			require.Equal(t, [][]string{{cfg.DocumentPrefix + document + cfg.DocumentSuffix}, {cfg.QueryPrefix + query + cfg.QuerySuffix}}, inputs)
		})
	}
}

func TestRequestedTextDimensions(t *testing.T) {
	for _, width := range []int{768, 512, 256, 128} {
		for _, request := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/request=%t", width, request), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Dimensions *int `json:"dimensions"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					if request {
						if req.Dimensions == nil || *req.Dimensions != width {
							t.Errorf("dimensions = %v, want %d", req.Dimensions, width)
						}
					} else if req.Dimensions != nil {
						t.Errorf("unexpected dimensions")
					}
					v := make([]float32, width)
					v[0] = 3
					v[1] = 4
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v}}})
				}))
				defer srv.Close()
				c, err := New(Config{BaseURL: srv.URL, Model: "example-model", Dims: width, RequestDimensions: request})
				require.NoError(t, err)
				for _, encode := range []func(context.Context, []string) ([][]float32, error){c.Embed, c.EmbedQuery} {
					vecs, err := encode(context.Background(), []string{"example"})
					require.NoError(t, err)
					require.Len(t, vecs[0], width)
				}
			})
		}
	}
}
