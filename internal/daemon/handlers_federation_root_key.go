package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const (
	rootRepinConsequencePin      = "The project has no root pin yet; the loaded key becomes its root key."
	rootRepinConsequenceRotation = "Downstream relays accept the new key through the signed rotation on their next sync."
	rootRepinConsequenceUnsigned = "Downstream relays that pinned the previous key reject receipts signed by the new key " +
		"and cannot follow an unsigned repin; Kata has no command that moves them to the new key. " +
		"New relays can still connect. Receipts signed by the previous key stay verifiable."
	rootRepinConsequenceNewKey = "Creates a new federation-root.key. Other projects pinned to the missing key " +
		"stay unavailable until each is repinned."
	rootRepinConsequenceStrands = "This project has %d active relay enrollment(s) pinned to the previous key; " +
		"an unsigned repin is refused unless you pass --strand-relays, which accepts that those relays can no " +
		"longer sync the project."
)

func registerFederationRootKeyRepin(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "repinFederationRootKey", Method: http.MethodPost, Path: "/api/v1/federation/root-keys/{project_name}/repin", Summary: "Pin a root project to this daemon's loaded root signing key"}, func(ctx context.Context, in *api.RepinFederationRootKeyRequest) (*api.RepinFederationRootKeyResponse, error) {
		if config.ValidateProjectName(in.ProjectName) != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", "valid local project name required", "", nil)
		}
		if cfg.RootSigning == nil {
			return nil, api.NewError(http.StatusServiceUnavailable, "root_signing_unavailable", "this server has no root signing authority", "", nil)
		}
		if in.Body.PreviousKeyFile != "" && !projectDiagnosticOwnerAuthority(ctx) {
			return nil, api.NewError(http.StatusForbidden, "forbidden", "previous_key_file requires daemon-owner transport or token", "", nil)
		}
		if in.Body.PreviousKeyFile != "" && !filepath.IsAbs(in.Body.PreviousKeyFile) {
			return nil, api.NewError(http.StatusBadRequest, "validation", "previous_key_file must be an absolute path on the daemon host", "", nil)
		}
		result, err := repinRootKey(ctx, cfg, in)
		if err != nil {
			return nil, err
		}
		return &api.RepinFederationRootKeyResponse{Body: result}, nil
	})
}

type rootRepinTarget struct {
	project db.Project
	pin     db.RootKeyPin
	pinned  bool
	// relays counts the active grants pinned to the current key.
	relays int64
}

func rootRepinTargetFor(ctx context.Context, store db.Storage, name string) (rootRepinTarget, error) {
	project, err := store.ProjectByName(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return rootRepinTarget{}, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
	}
	if err != nil {
		return rootRepinTarget{}, internalAPIError(err)
	}
	notRoot := api.NewError(http.StatusConflict, "root_key_not_local",
		fmt.Sprintf("project %s is not a hub project rooted on this daemon", name), "", nil)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) || err == nil && binding.Role != db.FederationRoleHub {
		return rootRepinTarget{}, notRoot
	}
	if err != nil {
		return rootRepinTarget{}, internalAPIError(err)
	}
	target := rootRepinTarget{project: project}
	target.pin, err = store.RootAuthority(ctx, project.UID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return target, nil
	case err != nil:
		return rootRepinTarget{}, internalAPIError(err)
	case target.pin.AuthorityUID != store.InstanceUID():
		return rootRepinTarget{}, notRoot
	}
	target.pinned = true
	target.relays, err = federationEnrollmentCount(ctx, store, binding)
	if err != nil {
		return rootRepinTarget{}, internalAPIError(err)
	}
	return target, nil
}

// repinRootKey pins the loaded key, creating the key file when missing. A
// supplied previous key turns the replacement into a signed rotation. An
// unsigned repin with active relay enrollments needs strand_relays.
func repinRootKey(ctx context.Context, cfg ServerConfig, in *api.RepinFederationRootKeyRequest) (api.FederationRootKeyRepinResult, error) {
	target, err := rootRepinTargetFor(ctx, cfg.DB, in.ProjectName)
	if err != nil {
		return api.FederationRootKeyRepinResult{}, err
	}
	previous, err := rootRepinPreviousSigner(target, in.Body.PreviousKeyFile)
	if err != nil {
		return api.FederationRootKeyRepinResult{}, err
	}
	signer, created, err := cfg.RootSigning.ensureKey(cfg.DB.InstanceUID(), false)
	if err != nil {
		return api.FederationRootKeyRepinResult{}, api.NewError(http.StatusConflict, "root_signing_key_invalid", err.Error(), "fix or move the key file, then retry", nil)
	}
	result := rootRepinPlan(target, signer, created, previous)
	result.KeyFile = cfg.RootSigning.path
	if in.Body.Preflight {
		return result, nil
	}
	if strands := rootRepinStrandsRelays(target, result, in.Body.StrandRelays); strands != nil {
		// Refuse before a key file is created; the storage transaction repeats
		// the check under the enrollment fence.
		return api.FederationRootKeyRepinResult{}, strands
	}
	// A current pin still loads a key file restored while the daemon runs.
	signer, _, err = cfg.RootSigning.ensureKey(cfg.DB.InstanceUID(), true)
	if err != nil {
		return api.FederationRootKeyRepinResult{}, api.NewError(http.StatusConflict, "root_signing_key_invalid", err.Error(), "fix or move the key file, then retry", nil)
	}
	if result.Status == "current" {
		return result, nil
	}
	result.KeyID = db.RootSignerKeyID(signer)
	next := db.RootKeyPin{ProjectUID: target.project.UID, AuthorityUID: signer.AuthorityUID, KeyID: result.KeyID, PublicKey: signer.PrivateKey.Public().(ed25519.PublicKey)}
	switch result.Method {
	case "pin":
		err = cfg.DB.PinRootAuthority(ctx, next)
		result.Status = "pinned"
	case "signed_rotation":
		var transition db.RootKeyTransition
		transition, err = db.SignRootKeyTransition(target.pin, next, previous.PrivateKey)
		if err == nil {
			err = cfg.DB.RotateRootAuthority(ctx, transition)
		}
		result.Status = "rotated"
	default:
		repinner, ok := cfg.DB.(db.RootRepinStorage)
		if !ok {
			return api.FederationRootKeyRepinResult{}, api.NewError(http.StatusServiceUnavailable, "root_repin_unavailable", "storage cannot repin root keys", "", nil)
		}
		err = repinner.RepinRootAuthority(ctx, target.pin.KeyID, next, in.Body.StrandRelays)
		result.Status = "repinned"
	}
	if strands, ok := errors.AsType[*db.RootRepinStrandsRelaysError](err); ok {
		return api.FederationRootKeyRepinResult{}, rootRepinStrandsRelaysError(target.project.Name, strands.Enrollments)
	}
	if err != nil {
		return api.FederationRootKeyRepinResult{}, api.NewError(http.StatusConflict, "root_repin_failed", err.Error(), "", nil)
	}
	return result, nil
}

// rootRepinStrandsRelays refuses an unaccepted unsigned repin while the
// project's last count showed active relay enrollments.
func rootRepinStrandsRelays(target rootRepinTarget, plan api.FederationRootKeyRepinResult, strandRelays bool) error {
	if plan.Method != "unsigned_repin" || target.relays == 0 || strandRelays {
		return nil
	}
	return rootRepinStrandsRelaysError(target.project.Name, target.relays)
}

func rootRepinStrandsRelaysError(project string, enrollments int64) error {
	return api.NewError(http.StatusConflict, "root_repin_strands_relays",
		fmt.Sprintf("project %s has %d active relay enrollment(s) pinned to the previous key; an unsigned repin leaves them unable to sync the project", project, enrollments),
		"pass --previous-key-file <path> for a signed rotation that relays follow, or --strand-relays to accept that those relays can no longer sync", nil)
}

// rootRepinPreviousSigner reads the operator-supplied previous key and checks
// it against the project's current pin. An empty path yields an empty signer.
func rootRepinPreviousSigner(target rootRepinTarget, previousKeyFile string) (db.RootAttributionSigner, error) {
	if previousKeyFile == "" {
		return db.RootAttributionSigner{}, nil
	}
	if !target.pinned {
		return db.RootAttributionSigner{}, api.NewError(http.StatusConflict, "root_key_not_pinned", "project has no root pin to rotate from", "omit previous_key_file", nil)
	}
	key, err := config.ReadRootSigningKey(previousKeyFile)
	if err != nil {
		return db.RootAttributionSigner{}, api.NewError(http.StatusConflict, "root_signing_key_invalid", fmt.Sprintf("previous key file %s: %v", previousKeyFile, err), "", nil)
	}
	previous := db.RootAttributionSigner{AuthorityUID: target.pin.AuthorityUID, PrivateKey: key}
	if !db.RootSignerMatches(target.pin, previous) {
		return db.RootAttributionSigner{}, api.NewError(http.StatusConflict, "root_signing_key_mismatch", "previous key file does not match the project's root pin", "", nil)
	}
	return previous, nil
}

func rootRepinPlan(target rootRepinTarget, signer db.RootAttributionSigner, created bool, previous db.RootAttributionSigner) api.FederationRootKeyRepinResult {
	result := api.FederationRootKeyRepinResult{ProjectName: target.project.Name, ProjectUID: target.project.UID, Status: "ready", CreatesKeyFile: created, KeyID: db.RootSignerKeyID(signer), PreviousKeyID: target.pin.KeyID, ActiveRelayEnrollments: target.relays}
	var consequences []string
	if created {
		consequences = append(consequences, rootRepinConsequenceNewKey)
	}
	switch {
	case target.pinned && result.KeyID == target.pin.KeyID:
		result.Status, result.Method = "current", "none"
	case !target.pinned:
		result.Method = "pin"
		consequences = append(consequences, rootRepinConsequencePin)
	case len(previous.PrivateKey) != 0:
		result.Method = "signed_rotation"
		consequences = append(consequences, rootRepinConsequenceRotation)
	default:
		result.Method = "unsigned_repin"
		consequences = append(consequences, rootRepinConsequenceUnsigned)
		if target.relays > 0 {
			consequences = append(consequences, fmt.Sprintf(rootRepinConsequenceStrands, target.relays))
		}
	}
	result.Consequence = strings.Join(consequences, " ")
	return result
}

// RootSigningWarning is the operator message for an unavailable root signing key.
func RootSigningWarning(health api.RootSigningHealth) string {
	problem := "does not match their root pin"
	if !health.KeyLoaded {
		problem = "is unavailable (" + health.KeyProblem + ")"
	}
	projects := "new hub projects"
	if len(health.Projects) > 0 {
		projects = strings.Join(health.Projects, ", ")
	}
	return fmt.Sprintf("root signing key %s %s; signed writes to %s fail until you restore the key file and restart the daemon, or run `kata federation root-key repin --project <name>`",
		health.KeyFile, problem, projects)
}
