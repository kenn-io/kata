package daemon

import (
	"encoding/hex"
	"testing"

	"go.kenn.io/kata/internal/docbank"
	"hegel.dev/go/hegel"
)

// A path prefix is a scope boundary only at a complete directory segment.
// Generate the scope rather than repeating the same hand-picked folder.
func TestDocbankOriginScopeBoundaryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		segment := "scope-" + hex.EncodeToString(hegel.Draw(ht, hegel.Binary(0, 24)))
		name := "file-" + hex.EncodeToString(hegel.Draw(ht, hegel.Binary(0, 24)))
		folder := "/" + segment
		cfg := ServerConfig{DocbankFolder: folder}
		for _, livePath := range []string{folder, folder + "/" + name, folder + "/nested/" + name} {
			if err := exposedDocbankNode(cfg, docbank.Node{Path: livePath}); err != nil {
				ht.Fatalf("scoped live path %q rejected: %v", livePath, err)
			}
		}
		for _, livePath := range []string{folder + "-sibling/" + name, folder + "/../" + name, "/other/" + name, folder + "//" + name, folder + "/\\" + name, ""} {
			if err := exposedDocbankNode(cfg, docbank.Node{Path: livePath}); err == nil {
				ht.Fatalf("unscoped or noncanonical live path %q accepted under %q", livePath, folder)
			}
		}
		if err := exposedDocbankNode(cfg, docbank.Node{Path: folder + "/" + name, TrashedAt: "2026-01-01T00:00:00Z"}); err == nil {
			ht.Fatalf("trashed scoped path accepted")
		}
	}, hegel.WithTestCases(64))
}
