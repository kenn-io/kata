package api

import (
	"go.kenn.io/kata/internal/tracker"
	"time"
)

// TrackerProjectRequest carries only the caller daemon's local project handle;
// forwarding resolves the portable project UID before translating at each hop.
type TrackerProjectRequest struct {
	Client    string `header:"X-Kata-Tracker-Client"`
	ProjectID int64  `path:"project_id"`
}

type TrackerConnectionsResponse struct{ Body TrackerConnectionsBody }
type TrackerConnectionsBody struct {
	RootInstanceUID string                     `json:"root_instance_uid"`
	ProjectUID      string                     `json:"project_uid"`
	Connections     []tracker.PublicDescriptor `json:"connections"`
}

// TrackerOperationBody chooses identities and nonsecret provider settings only.
// Reuse UID when retrying the same request, including after a timeout.
type TrackerOperationBody struct {
	ExpectedUpdatedAt *time.Time                  `json:"expected_updated_at,omitempty"`
	UID               string                      `json:"uid"`
	Kind              tracker.ControlKind         `json:"kind" enum:"bind,rebind,disable,run,activate,reconcile"`
	Provider          string                      `json:"provider" enum:"github,notion,linear,plane,ticktick,todoist,twenty"`
	Resource          string                      `json:"resource"`
	Selection         *tracker.Selection          `json:"selection,omitempty"`
	Binding           *EnableIssueSyncRequestBody `json:"binding,omitempty"`
}

type TrackerOperationRequest struct {
	Client    string `header:"X-Kata-Tracker-Client"`
	ProjectID int64  `path:"project_id"`
	Body      TrackerOperationBody
}

type TrackerOperationOut struct {
	Settlement      *tracker.ControlSettlement `json:"settlement,omitempty"`
	RootInstanceUID string                     `json:"root_instance_uid"`
	ProjectUID      string                     `json:"project_uid"`
	UID             string                     `json:"uid"`
	Selection       tracker.Selection          `json:"selection"`
	Kind            tracker.ControlKind        `json:"kind"`
	State           tracker.ControlState       `json:"state"`
	Result          *tracker.ControlResult     `json:"result,omitempty"`
}

type TrackerOperationResponse struct{ Body TrackerOperationOut }

type TrackerOperationReadRequest struct {
	Client    string `header:"X-Kata-Tracker-Client"`
	ProjectID int64  `path:"project_id"`
	UID       string `path:"uid"`
}

// TrackerRelayPayload is a bounded typed control request, never a general API
// proxy. Each receiving hop authenticates its own enrollment and preserves the
// asserted original subject separately from enrollment attribution.
type TrackerRelayPayload struct {
	Kind      string                `json:"kind" enum:"discover,execute,receipt"`
	UID       string                `json:"uid"`
	Operation *TrackerOperationBody `json:"operation,omitempty"`
}
type TrackerRelayBody struct {
	Assertion tracker.RelayAssertion `json:"assertion"`
	Payload   TrackerRelayPayload    `json:"payload"`
}
type TrackerRelayRequest struct {
	ProjectID     int64  `path:"project_id"`
	Authorization string `header:"Authorization"`
	Body          TrackerRelayBody
}
type TrackerRelayResult struct {
	RootInstanceUID string                  `json:"root_instance_uid"`
	ProjectUID      string                  `json:"project_uid"`
	Connections     *TrackerConnectionsBody `json:"connections,omitempty"`
	Operation       *TrackerOperationOut    `json:"operation,omitempty"`
}
type TrackerRelayResponse struct{ Body TrackerRelayResult }

// TrackerRecoveryOut is owner-only local metadata. No provider request, result
// payload, connection selector or executable transport authority is returned.
type TrackerRecoveryOut struct {
	RestoredHistoryGap   bool               `json:"restored_history_gap"`
	RootInstanceUID      string             `json:"root_instance_uid"`
	ProjectUID           string             `json:"project_uid"`
	Role                 string             `json:"role"`
	Selection            *tracker.Selection `json:"selection,omitempty"`
	WriteUID             string             `json:"write_uid,omitempty"`
	PendingEventUID      string             `json:"pending_event_uid,omitempty"`
	Phase                string             `json:"phase,omitempty"`
	ProviderAcknowledged bool               `json:"provider_acknowledged"`
	WritesBlocked        bool               `json:"writes_blocked"`
	ActivationRequired   bool               `json:"activation_required"`
}
type TrackerRecoveryResponse struct{ Body TrackerRecoveryOut }

type TrackerControlSettlementRequest struct {
	Client    string `header:"X-Kata-Tracker-Client"`
	ProjectID int64  `path:"project_id"`
	UID       string `path:"uid"`
	Body      struct{}
}

type TrackerRecoveryRefreshRequest struct {
	Client    string `header:"X-Kata-Tracker-Client"`
	ProjectID int64  `path:"project_id"`
	Body      struct{}
}
type TrackerRecoveryRefreshOut struct {
	RootInstanceUID   string `json:"root_instance_uid"`
	ProjectUID        string `json:"project_uid"`
	ReceiptsRefreshed bool   `json:"receipts_refreshed"`
}
type TrackerRecoveryRefreshResponse struct{ Body TrackerRecoveryRefreshOut }
