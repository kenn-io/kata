package testfix

import (
	"testing"

	"go.kenn.io/kit/fslink"
)

// MkJunction creates a directory junction at link pointing to target.
// A junction is the reparse point form Go reports as neither a symlink nor a
// directory, so it exercises path canonicalization that filepath.EvalSymlinks
// cannot walk through. Tests are skipped when the host refuses to create one;
// on platforms without junctions every call skips.
func MkJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := fslink.CreateJunction(target, link); err != nil {
		t.Skipf("cannot create directory junction: %v", err)
	}
}
