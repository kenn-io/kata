package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

var agentsViewUploadRoute = regexp.MustCompile(`^/api/v1/projects/[^/]+/(?:issues/[^/]+/transcript-share|federation/transcript-service/issues/[^/]+)$`)

func isAgentsViewBinaryUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && r.Header.Get("Content-Type") == "application/octet-stream" && agentsViewUploadRoute.MatchString(r.URL.Path)
}

type agentsViewShareRequest struct {
	ProjectID     int64  `path:"project_id"`
	Ref           string `path:"ref"`
	Authorization string `header:"Authorization"`
	Encoding      string `header:"Content-Encoding"`
	Actor         string `header:"X-Kata-Actor"`
	Control       string `header:"X-Kata-Transcript-Control" required:"true"`
	body          io.Reader
	control       []string
	headerBytes   int
}

// Resolve retains the streaming body rather than asking Huma to buffer it.
func (in *agentsViewShareRequest) Resolve(ctx huma.Context) []error {
	in.body = ctx.BodyReader()
	ctx.EachHeader(func(name, value string) {
		in.headerBytes += len(name) + len(value) + 4
		if strings.EqualFold(name, agentsviewrelay.ControlHeader) {
			in.control = append(in.control, value)
		}
	})
	return nil
}

type agentsViewShareResponse struct {
	CacheControl string `header:"Cache-Control"`
	Body         agentsviewrelay.Receipt
}

func registerAgentsViewShareHandlers(humaAPI huma.API, cfg ServerConfig) {
	admission := make(chan struct{}, 2)
	for _, federation := range []bool{false, true} {
		operation, path := "shareIssueAgentsView", "/api/v1/projects/{project_id}/issues/{ref}/transcript-share"
		if federation {
			operation, path = "shareFederationIssueAgentsView", "/api/v1/projects/{project_id}/federation/transcript-service/issues/{ref}"
		}
		huma.Register(humaAPI, huma.Operation{OperationID: operation, Method: "POST", Path: path, MaxBodyBytes: agentsviewrelay.MaxSnapshotBytes, RequestBody: &huma.RequestBody{Required: true, Content: binaryContent("application/octet-stream")}}, func(ctx context.Context, in *agentsViewShareRequest) (*agentsViewShareResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()
			if cfg.AgentsViewRelay == nil || cfg.AgentsViewRelay.Selection == nil || cfg.AgentsViewRelay.Spool == nil {
				return nil, agentsViewUnavailable()
			}
			if in.headerBytes > agentsviewrelay.MaxProofBytes || (in.Encoding != "" && in.Encoding != "identity") {
				return nil, api.NewError(400, "agentsview_invalid_control", "bounded uncompressed upload required", "", nil)
			}
			control, err := agentsviewrelay.ParseControlHeader(in.control)
			if err != nil {
				return nil, api.NewError(400, "agentsview_invalid_control", err.Error(), "", nil)
			}
			if !federation && control.ClientKind != "cli" && control.ClientKind != "tui" {
				return nil, api.NewError(400, "agentsview_invalid_control", "invalid ordinary client kind", "", nil)
			}
			var incoming federationPrincipal
			if federation {
				if cfg.AgentsViewRelay.VerifyFederationUpload == nil {
					return nil, agentsViewUnavailable()
				}
				ctx, incoming, err = authorizeFederationRequestForCapabilities(ctx, cfg, in.Authorization, in.ProjectID, []string{"push"}, federationTransportOperation(operation))
				if err != nil {
					return nil, err
				}
				if incoming.ScopedProjectID != in.ProjectID || incoming.RelayProtocolVersion != db.RelayProtocolVersion {
					return nil, api.NewError(403, "auth_invalid", "negotiated project enrollment required", "", nil)
				}
				if err = checkAgentsViewIncoming(control, incoming, cfg.DB.InstanceUID()); err != nil {
					return nil, err
				}
				if err = cfg.AgentsViewRelay.VerifyFederationUpload(ctx, control); err != nil {
					return nil, err
				}
			}
			finish, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), in.ProjectID), cfg.DB, in.ProjectID)
			if err != nil {
				return nil, agentsViewUnavailable()
			}
			defer finish()
			select {
			case admission <- struct{}{}:
				defer func() { <-admission }()
			default:
				return nil, api.NewError(429, "admission_limited", "AgentsView uploads are busy", "", nil)
			}
			issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
			association, err := loadAgentsViewClose(ctx, cfg, issue, control)
			if err != nil {
				return nil, err
			}
			if association.AuthorityUID == cfg.DB.InstanceUID() {
				if cfg.AgentsViewRelay.Ingest == nil || cfg.RootSigning == nil {
					return nil, agentsViewUnavailable()
				}
				pin, err := agentsViewRootPin(ctx, cfg.DB, association)
				if err != nil {
					return nil, err
				}
				if err = agentsViewRootAdmission(association, pin, cfg.RootSigning.Signer()); err != nil {
					return nil, err
				}
			} else if cfg.AgentsViewRelay.Forward == nil {
				return nil, agentsViewUnavailable()
			}
			actor, err := agentsViewUploadActor(ctx, federation, in.Actor, incoming)
			if err != nil {
				return nil, err
			}
			var claimPeer *federationPrincipal
			if federation {
				claimPeer = &incoming
			}
			var mappedClaim db.ClaimPrincipal
			if err = requireAgentsViewClaimWhileCoordinated(ctx, cfg, issue, actor, claimPeer, control.ClientKind, &mappedClaim); err != nil {
				return nil, err
			}
			if err = cfg.AgentsViewRelay.Selection(ctx, issue, association, nil); err != nil {
				return nil, err
			}
			spool, err := cfg.AgentsViewRelay.Spool(ctx, io.LimitReader(in.body, agentsviewrelay.MaxSnapshotBytes+1), control)
			if err != nil {
				if spool != nil {
					_ = spool.Close()
				}
				return nil, err
			}
			if spool == nil {
				return nil, agentsViewUnavailable()
			}
			defer func() { _ = spool.Close() }()
			if err = agentsviewrelay.VerifySnapshot(ctx, spool, control); err != nil {
				return nil, api.NewError(400, "agentsview_invalid_snapshot", err.Error(), "", nil)
			}
			revalidate := func() error {
				if err := revalidateBrowserSession(ctx); err != nil {
					return api.NewError(http.StatusUnauthorized, "web_session_required", "browser session is no longer valid", "", nil)
				}
				if err := revalidateIssueScopedPrincipal(ctx, cfg.DB); err != nil {
					return err
				}
				if err := revalidateAgentsViewCaller(ctx); err != nil {
					return err
				}
				if federation {
					now, err := evaluateFederationRequestForCapabilities(ctx, cfg, in.Authorization, in.ProjectID, []string{"push"}, federationTransportOperation(operation))
					if err != nil {
						return err
					}
					if now.principal != incoming {
						return api.NewError(403, "auth_invalid", "enrollment changed during upload", "", nil)
					}
				}
				current, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, issue.UID, db.IncludeDeletedNo)
				if err != nil {
					return err
				}
				if _, err = loadAgentsViewClose(ctx, cfg, current, control); err != nil {
					return err
				}
				if err = requireAgentsViewClaimWhileCoordinated(ctx, cfg, current, actor, claimPeer, control.ClientKind, &mappedClaim); err != nil {
					return err
				}
				if _, err := agentsViewRootPin(ctx, cfg.DB, association); err != nil {
					return err
				}
				return cfg.AgentsViewRelay.Selection(ctx, current, association, nil)
			}
			if err = revalidate(); err != nil {
				return nil, err
			}
			pin, err := agentsViewRootPin(ctx, cfg.DB, association)
			if err != nil {
				return nil, err
			}
			var receipt agentsviewrelay.Receipt
			if association.AuthorityUID == cfg.DB.InstanceUID() {
				if cfg.AgentsViewRelay.Ingest == nil || cfg.RootSigning == nil {
					return nil, agentsViewUnavailable()
				}
				signer := cfg.RootSigning.Signer()
				if !agentsViewSignerMatches(signer, pin) {
					return nil, agentsViewUnavailable()
				}
				ack, err := cfg.AgentsViewRelay.Ingest(ctx, issue, association, spool)
				if err != nil {
					return nil, err
				}
				if ack.SessionID != association.SessionID || ack.Agent != association.Agent || (ack.Status != "created" && ack.Status != "unchanged") {
					return nil, api.NewError(502, "agentsview_invalid_receipt", "receiver acknowledgement differs", "", nil)
				}
				locator, _ := association.Locator()
				receipt, err = agentsviewrelay.SignReceipt(agentsviewrelay.Receipt{Version: 1, ProjectUID: association.ProjectUID, AuthorityUID: association.AuthorityUID, ServiceID: association.ServiceID, IssueUID: issue.UID, CloseEventUID: control.CloseEventUID, SnapshotSHA256: association.SnapshotSHA256, SnapshotBytes: association.SnapshotBytes, Agent: association.Agent, SessionID: association.SessionID, Locator: locator, Status: ack.Status, AcceptedAt: time.Now().UTC()}, signer.PrivateKey)
				if err != nil {
					return nil, internalAPIError(err)
				}
			} else {
				if cfg.AgentsViewRelay.Forward == nil {
					return nil, agentsViewUnavailable()
				}
				receipt, err = cfg.AgentsViewRelay.Forward(ctx, issue, agentsViewForwardControl(control, mappedClaim), spool)
				if err != nil {
					return nil, err
				}
			}
			locator, _ := association.Locator()
			if err = agentsviewrelay.VerifyReceipt(receipt, ed25519.PublicKey(pin.PublicKey)); err != nil || receipt.ProjectUID != association.ProjectUID || receipt.AuthorityUID != association.AuthorityUID || receipt.ServiceID != association.ServiceID || receipt.IssueUID != issue.UID || receipt.CloseEventUID != control.CloseEventUID || receipt.SnapshotSHA256 != association.SnapshotSHA256 || receipt.SnapshotBytes != association.SnapshotBytes || receipt.Agent != association.Agent || receipt.SessionID != association.SessionID || receipt.Locator != locator {
				return nil, api.NewError(502, "agentsview_invalid_receipt", "root receipt differs from selected snapshot", "", nil)
			}
			if err = revalidate(); err != nil {
				return nil, err
			}
			raw, err := json.Marshal(receipt)
			if err != nil || len(raw) > agentsviewrelay.MaxProofBytes {
				return nil, api.NewError(502, "agentsview_invalid_receipt", "root receipt too large", "", nil)
			}
			return &agentsViewShareResponse{CacheControl: "private, no-store", Body: receipt}, nil
		})
	}
}

func checkAgentsViewIncoming(c agentsviewrelay.Control, p federationPrincipal, localUID string) error {
	for i, hop := range c.Route {
		if hop.InstanceUID != localUID {
			continue
		}
		if i+1 >= len(c.Route) {
			break
		}
		child := c.Route[i+1]
		if child.InstanceUID == p.SpokeInstanceUID && child.BindingUID == p.RelayBindingUID && child.ResetEpoch == p.RelayResetEpoch && child.UpstreamInstanceUID == localUID {
			return nil
		}
		break
	}
	return api.NewError(409, "agentsview_route_changed", "incoming enrollment differs from selected route", "", nil)
}

func agentsViewUploadActor(ctx context.Context, federation bool, actor string, incoming federationPrincipal) (string, error) {
	if federation {
		return incoming.Actor, nil
	}
	return attributedActor(ctx, actor)
}

func agentsViewSignerMatches(signer db.RootAttributionSigner, pin db.RootKeyPin) bool {
	return signer.AuthorityUID == pin.AuthorityUID && len(signer.PrivateKey) == ed25519.PrivateKeySize && db.RootPublicKeyID(signer.PrivateKey.Public().(ed25519.PublicKey)) == pin.KeyID
}

func agentsViewRootPin(ctx context.Context, store db.Storage, association agentsviewrelay.Association) (db.RootKeyPin, error) {
	pin, err := store.RootAuthority(ctx, association.ProjectUID)
	if errors.Is(err, db.ErrNotFound) {
		return db.RootKeyPin{}, api.NewError(409, "agentsview_route_changed", "root pin changed", "", nil)
	}
	if err != nil {
		return db.RootKeyPin{}, internalAPIError(err)
	}
	if pin.Retired || pin.AuthorityUID != association.AuthorityUID || pin.KeyID != association.RootKeyID {
		return db.RootKeyPin{}, api.NewError(409, "agentsview_route_changed", "root pin changed", "", nil)
	}
	return pin, nil
}

func agentsViewRootAdmission(a agentsviewrelay.Association, pin db.RootKeyPin, signer db.RootAttributionSigner) error {
	if pin.Retired || pin.AuthorityUID != a.AuthorityUID || pin.KeyID != a.RootKeyID {
		return api.NewError(409, "agentsview_route_changed", "root pin changed", "", nil)
	}
	if !agentsViewSignerMatches(signer, pin) {
		return agentsViewUnavailable()
	}
	return nil
}
