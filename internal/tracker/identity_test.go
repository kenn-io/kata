package tracker

import (
	"testing"
)

const root = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const project = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
const owner = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
const conn = "01ARZ3NDEKTSV4RRFFQ69G5FAY"

func fixture() (Descriptor, Principal) {
	p := Principal{AuthorityInstanceUID: root, Kind: SubjectAccount, Subject: "member"}
	d := Descriptor{Selection: Selection{OwnerInstanceUID: owner, ConnectionUID: conn, Generation: "g1"}, Provider: "notion", Label: "Example account", Grants: []Grant{{RootInstanceUID: root, ProjectUID: project, Principal: p, Actions: []Action{ActionDiscover, ActionBind, ActionRead}, Sources: []string{"notion:source"}}}}
	return d, p
}

// Contract: transport enrollment and matching actor labels never imply account use.
func TestGrantRequiresQualifiedSubjectAndProject(t *testing.T) {
	d, p := fixture()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if !d.Allows(root, project, p, ActionBind, "notion:source") {
		t.Fatal("explicit member grant denied")
	}
	cases := []Principal{p, p, p}
	cases[0].AuthorityInstanceUID = owner
	cases[1].Kind = SubjectOwnerLocal
	cases[2].Subject = "other"
	for _, other := range cases {
		if d.Allows(root, project, other, ActionBind, "notion:source") {
			t.Fatal("qualified subject broadened")
		}
	}
	if d.Allows(owner, project, p, ActionBind, "notion:source") || d.Allows(root, owner, p, ActionBind, "notion:source") || d.Allows(root, project, p, ActionBind, "notion:other") {
		t.Fatal("scope broadened")
	}
	if d.Allows(root, project, p, ActionStatus, "notion:source") {
		t.Fatal("ungranted action")
	}
}

func TestServiceGrantDoesNotBorrowMember(t *testing.T) {
	d, p := fixture()
	service := Principal{AuthorityInstanceUID: root, Kind: SubjectService, Subject: "sync"}
	if d.Allows(root, project, service, ActionRead, "notion:source") {
		t.Fatal("service borrowed member grant")
	}
	d.Grants = append(d.Grants, Grant{RootInstanceUID: root, ProjectUID: project, Principal: service, Actions: []Action{ActionRead}, Sources: []string{"notion:source"}})
	if !d.Allows(root, project, service, ActionRead, "notion:source") || d.Allows(root, project, p, ActionStatus, "notion:source") {
		t.Fatal("service/member separation")
	}
}

func TestSelectionRequiresExactIdentity(t *testing.T) {
	d, p := fixture()
	if _, err := Select([]Descriptor{d}, nil, root, project, p, ActionBind, "notion:source"); err != ErrSelectionRequired {
		t.Fatalf("implicit selection: %v", err)
	}
	selected, err := Select([]Descriptor{d}, &d.Selection, root, project, p, ActionBind, "notion:source")
	if err != nil || selected.Selection != d.Selection {
		t.Fatal("explicit selection rejected", err)
	}
	changed := d.Selection
	changed.Generation = "g2"
	if _, err := Select([]Descriptor{d}, &changed, root, project, p, ActionBind, "notion:source"); err != ErrUnavailable {
		t.Fatal("stale selection accepted")
	}
	if _, err := Select([]Descriptor{d, d}, &d.Selection, root, project, p, ActionBind, "notion:source"); err != ErrAmbiguous {
		t.Fatal("ambiguous registry accepted")
	}
}

func TestGrantRequiresExplicitResources(t *testing.T) {
	d, _ := fixture()
	d.Grants[0].Sources = nil
	if d.Validate() == nil {
		t.Fatal("omitted resource scope accepted")
	}
	d.Grants[0].AllAccessibleSources = true
	if d.Validate() != nil {
		t.Fatal("explicit all-accessible denied")
	}
	d.Grants[0].Sources = []string{"notion:source"}
	if d.Validate() == nil {
		t.Fatal("conflicting scopes accepted")
	}
}

func TestPublicDescriptorOmitsOtherGrants(t *testing.T) {
	d, p := fixture()
	public, ok := d.Public(root, project, p)
	if !ok || public.Selection != d.Selection || public.Provider != "notion" {
		t.Fatal("discovery mismatch")
	}
	other := p
	other.Subject = "ungranted"
	if _, ok := d.Public(root, project, other); ok {
		t.Fatal("ungranted connection discovered")
	}
}
