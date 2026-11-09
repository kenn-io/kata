package tracker

import (
	"hegel.dev/go/hegel"
	"testing"
)

// An independent equality oracle checks the full qualified subject and resource
// intersection. Generating the dimensions independently catches accidental ORs.
func TestQualifiedGrantIntersectionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		d, p := fixture()
		bits := hegel.Draw(ht, hegel.Integers(0, 127))
		r, j, source := root, project, "notion:source"
		if bits&1 != 0 {
			p.AuthorityInstanceUID = owner
		}
		if bits&2 != 0 {
			p.Kind = SubjectOwnerLocal
		}
		if bits&4 != 0 {
			p.Subject = "other"
		}
		if bits&8 != 0 {
			r = owner
		}
		if bits&16 != 0 {
			j = owner
		}
		if bits&32 != 0 {
			source = "notion:other"
		}
		action := ActionBind
		if bits&64 != 0 {
			action = ActionStatus
		}
		if got, want := d.Allows(r, j, p, action, source), bits == 0; got != want {
			ht.Fatalf("grant intersection bits=%d got=%v want=%v", bits, got, want)
		}
	}, hegel.WithTestCases(1000))
}
