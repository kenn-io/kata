package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/httpurl"
)

func registerFederationSigningHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "configureFederationSigning",
		Method:      http.MethodPost,
		Path:        "/api/v1/federation/replicas/{project_uid}/actions/configure-signing",
	}, func(ctx context.Context, in *api.ConfigureFederationSigningRequest) (*struct{}, error) {
		// Selecting daemon filesystem/environment secrets requires owner authority.
		_, attributed := PrincipalFromContext(ctx)
		if webSessionAuthenticated(ctx) || insecureReadonlyRequest(ctx) ||
			unauthenticatedPrivateNetworkRequest(ctx) ||
			(!attributed && !ownerLocalTransport(ctx)) || ensureTokenAdminAllowed(ctx) != nil {
			return nil, api.NewError(http.StatusForbidden, "federation_signing_admin_forbidden",
				"signing configuration requires daemon owner authority", "", nil)
		}
		store := cfg.federationCredentialStore()
		current, found, err := store.FederationCredential(ctx, in.ProjectUID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		if !found {
			return nil, api.NewError(http.StatusNotFound, "federation_credential_not_found",
				"active federation credential not found", "", nil)
		}
		if current.LeavePending {
			return nil, api.NewError(http.StatusConflict, "federation_leave_pending",
				"federation leave is pending", "", nil)
		}
		hubURL := in.Body.HubURL
		if hubURL == "" {
			hubURL = current.HubURL
		}
		canonical, err := httpurl.CanonicalHTTPBaseURL(hubURL)
		if err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation", "invalid signing hub URL", "", nil)
		}
		if in.Body.KeyFile != "" && !filepath.IsAbs(in.Body.KeyFile) {
			return nil, api.NewError(http.StatusBadRequest, "validation",
				"key_file must be an absolute path on the daemon", "", nil)
		}
		source := federationsigning.Source{
			KeyID: in.Body.KeyID, KeyFile: in.Body.KeyFile, KeyEnv: in.Body.KeyEnv, HubURL: canonical,
		}
		if err := federationsigning.ValidatePolicy(canonical,
			[]federationsigning.Key{{Source: source, EnrollmentID: 1}}); err != nil {
			return nil, api.NewError(http.StatusBadRequest, "validation",
				"invalid or unavailable federation signing source on the daemon", "", nil)
		}
		replacer, ok := store.(config.FederationCredentialReplacer)
		if !ok {
			return nil, internalAPIError(fmt.Errorf("credential store does not support exact replacement"))
		}
		replacement := current
		replacement.Signing = &source
		err = replacer.ReplaceFederationCredential(ctx, config.FederationCredentialReplacement{
			ProjectUID: in.ProjectUID, Expected: current, Replacement: replacement,
		})
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return nil, api.NewError(http.StatusConflict, "federation_credential_conflict",
				"federation credential changed; retry signing configuration", "", nil)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		if cfg.FederationWake != nil {
			cfg.FederationWake()
		}
		return nil, nil
	})
}
