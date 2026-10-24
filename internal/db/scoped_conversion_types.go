package db

import (
	"errors"
	"go.kenn.io/kata/internal/uid"
	"slices"
)

type ScopedConversionRequest struct {
	UID                string               `json:"uid"`
	Digest             string               `json:"digest"`
	CatalogEntryUID    string               `json:"catalog_entry_uid"`
	RouteDigest        string               `json:"route_digest"`
	ProjectUIDs        []string             `json:"project_uids"`
	Target             ScopedViewDescriptor `json:"target"`
	SuspendBindingUIDs []string             `json:"suspend_binding_uids"`
}

type ScopedConversionState struct {
	Request         ScopedConversionRequest `json:"request"`
	Phase           string                  `json:"phase"`
	RootCutoverUID  string                  `json:"root_cutover_uid"`
	InventoryDigest string                  `json:"inventory_digest"`
	BaselineDigest  string                  `json:"baseline_digest"`
	OldBindings     []ScopedOldBinding      `json:"old_bindings"`
}

type ScopedOldBinding struct {
	ProjectUID string `json:"project_uid"`
	BindingUID string `json:"binding_uid"`
	Mode       string `json:"mode"`
	Epoch      int64  `json:"epoch"`
}

type ScopedObligation struct {
	UID          string `json:"uid"`
	Kind         string `json:"kind"`
	BindingUID   string `json:"binding_uid"`
	PrincipalKey string `json:"principal_key"`
	ObjectUID    string `json:"object_uid"`
	Digest       string `json:"digest"`
	State        string `json:"state"`
	Epoch        int64  `json:"epoch"`
	Sequence     int64  `json:"sequence"`
}

func (r ScopedConversionRequest) Validate() error {
	if !uid.Valid(r.UID) || !uid.Valid(r.CatalogEntryUID) || !scopedDigest(r.Digest) || !scopedDigest(r.RouteDigest) || len(r.ProjectUIDs) == 0 || len(r.ProjectUIDs) > MaxScopedSelectedProjects {
		return errors.New("invalid conversion request")
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	previous := ""
	for _, id := range r.ProjectUIDs {
		if !uid.Valid(id) || id <= previous {
			return errors.New("invalid conversion project set")
		}
		previous = id
	}
	if len(r.ProjectUIDs) != len(r.Target.Projects) {
		return errors.New("conversion target project set mismatch")
	}
	for _, pin := range r.Target.Projects {
		if !slices.Contains(r.ProjectUIDs, pin.ProjectUID) {
			return errors.New("conversion target project mismatch")
		}
	}
	seen := map[string]bool{}
	for _, id := range r.SuspendBindingUIDs {
		if !uid.Valid(id) || seen[id] {
			return errors.New("invalid suspension binding set")
		}
		seen[id] = true
	}
	return nil
}
func (s ScopedConversionState) Validate() error {
	switch s.Phase {
	case "prepare", "fence", "activate", "complete":
	default:
		return errors.New("unknown conversion phase")
	}
	if err := s.Request.Validate(); err != nil {
		return err
	}
	if s.Phase != "prepare" && (!uid.Valid(s.RootCutoverUID) || !scopedDigest(s.InventoryDigest)) {
		return errors.New("unfenced conversion state")
	}
	if (s.Phase == "activate" || s.Phase == "complete") && !scopedDigest(s.BaselineDigest) {
		return errors.New("conversion missing baseline")
	}
	seen := map[string]bool{}
	for _, old := range s.OldBindings {
		if old.Validate() != nil || seen[old.ProjectUID] {
			return errors.New("invalid old conversion binding")
		}
		seen[old.ProjectUID] = true
	}
	return nil
}

// ScopedOldBinding uses actual native identities. Direct bindings have no relay
// UID/epoch; an empty pair is inventory shape, never a credential or authority.
func (b ScopedOldBinding) Validate() error {
	if !uid.Valid(b.ProjectUID) {
		return errors.New("invalid old binding project")
	}
	relay := uid.Valid(b.BindingUID) && b.Epoch > 0
	direct := b.BindingUID == "" && b.Epoch == 0
	switch b.Mode {
	case "v1":
		if !relay {
			return errors.New("v1 inventory requires relay identity")
		}
	case "direct":
		if !direct {
			return errors.New("direct inventory cannot invent relay identity")
		}
	case "detached":
		if !relay && !direct {
			return errors.New("detached inventory has partial relay identity")
		}
	default:
		return errors.New("unknown old binding mode")
	}
	return nil
}
