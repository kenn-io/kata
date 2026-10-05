package daemon

import (
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
)

// Attribution is bound after bearer/proxy/host authentication. Source labels and
// API bodies cannot grant the internal owner bypass or change a bound account.
func withRootAttribution(humaAPI huma.API, signer *db.RootAttributionSigner) {
	if signer == nil {
		return
	}
	humaAPI.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		request := ctx.Context()
		principal, ok := PrincipalFromContext(request)
		if ok && principal.Actor != "" {
			request = db.WithRootAttribution(request, *signer, principal.Actor)
		} else if projectOwnerAuthority(request) && ensureAttributedWriteAllowed(request) == nil {
			request = db.WithOwnerRootAttribution(request, *signer)
		}
		next(huma.WithContext(ctx, request))
	})
}
