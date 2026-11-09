package client

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

// TrackerSelection names an account without carrying credentials or selectors.
type TrackerSelection = tracker.Selection

// TrackerConnection is a grant-filtered public descriptor, never owner config.
type TrackerConnection = tracker.PublicDescriptor

// TrackerOperationResult contains only classified durable operation facts.
type TrackerOperationResult = tracker.ControlResult

// TrackerConnections is one project's discovery result.
type TrackerConnections struct {
	RootInstanceUID string              `json:"root_instance_uid"`
	ProjectUID      string              `json:"project_uid"`
	Connections     []TrackerConnection `json:"connections"`
}

// TrackerBindingOptions carries provider resource/status settings only. The
// daemon rejects authentication selectors and arbitrary provider configuration.
type TrackerBindingOptions struct {
	StatusSync      *string        `json:"status_sync,omitempty"`
	Config          map[string]any `json:"config,omitempty"`
	IntervalSeconds *int           `json:"interval_seconds,omitempty"`
	Interval        string         `json:"interval,omitempty"`
}

// TrackerOperation uses a caller-retained UID. Never replace it automatically
// after a timeout. Omitted selection preserves an existing named binding only.
type TrackerOperation struct {
	ExpectedUpdatedAt *time.Time             `json:"expected_updated_at,omitempty"`
	UID               string                 `json:"uid"`
	Kind              tracker.ControlKind    `json:"kind"`
	Provider          string                 `json:"provider"`
	Resource          string                 `json:"resource"`
	Selection         *TrackerSelection      `json:"selection,omitempty"`
	Binding           *TrackerBindingOptions `json:"binding,omitempty"`
}

type TrackerReceipt struct {
	Settlement      *tracker.ControlSettlement `json:"settlement,omitempty"`
	RootInstanceUID string                     `json:"root_instance_uid"`
	ProjectUID      string                     `json:"project_uid"`
	UID             string                     `json:"uid"`
	Selection       TrackerSelection           `json:"selection"`
	Kind            tracker.ControlKind        `json:"kind"`
	State           tracker.ControlState       `json:"state"`
	Result          *TrackerOperationResult    `json:"result,omitempty"`
}

// TrackerMCPClient declares a client class that cannot borrow owner authority.
// The daemon accepts only the actual account-bound REST token for this class.
func TrackerMCPClient(_ context.Context, request *http.Request) error {
	request.Header.Set("X-Kata-Tracker-Client", "mcp")
	return nil
}

func (c *Client) TrackerConnections(ctx context.Context, projectID int64, editors ...RequestEditorFn) (*TrackerConnections, error) {
	result := new(TrackerConnections)
	err := c.trackerRequest(ctx, http.MethodGet, projectID, "/tracker-connections", nil, result, editors...)
	return result, err
}
func (c *Client) TrackerOperation(ctx context.Context, projectID int64, body TrackerOperation, editors ...RequestEditorFn) (*TrackerReceipt, error) {
	if !uid.Valid(body.UID) {
		return nil, fmt.Errorf("tracker operation UID must be a normalized ULID")
	}
	result := new(TrackerReceipt)
	err := c.trackerRequest(ctx, http.MethodPost, projectID, "/tracker-operations", body, result, editors...)
	if err != nil {
		return nil, err
	}
	if err := validateTrackerReceipt(result, body.UID); err != nil {
		return nil, err
	}
	return result, nil
}
func (c *Client) TrackerOperationReceipt(ctx context.Context, projectID int64, operationUID string, editors ...RequestEditorFn) (*TrackerReceipt, error) {
	if !uid.Valid(operationUID) {
		return nil, fmt.Errorf("tracker operation UID must be a normalized ULID")
	}
	result := new(TrackerReceipt)
	err := c.trackerRequest(ctx, http.MethodGet, projectID, "/tracker-operations/"+operationUID, nil, result, editors...)
	if err != nil {
		return nil, err
	}
	if err := validateTrackerReceipt(result, operationUID); err != nil {
		return nil, err
	}
	return result, nil
}

type trackerRequestOptions struct{ body any }

func (o trackerRequestOptions) GetPathParams() (map[string]any, error) { return nil, nil }
func (o trackerRequestOptions) GetQuery() (map[string]any, error)      { return nil, nil }
func (o trackerRequestOptions) GetHeader() (map[string]string, error)  { return nil, nil }
func (o trackerRequestOptions) GetBody() any                           { return o.body }

// Keep the established transport, request editors and intersected project scope.
// This typed extension can run before upstream CI regenerates API clients; it
// never falls back to an older endpoint or another credential on failure.
func (c *Client) trackerRequest(ctx context.Context, method string, projectID int64, suffix string, body, target any, editors ...RequestEditorFn) error {
	if c == nil || c.apiClient == nil || projectID <= 0 {
		return fmt.Errorf("tracker project client is invalid")
	}
	path := "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + suffix
	request, err := c.apiClient.CreateRequest(ctx, runtime.RequestOptionsParameters{RequestURL: c.apiClient.GetBaseURL() + path, Method: method, ContentType: "application/json", Options: trackerRequestOptions{body: body}}, editors...)
	if err != nil {
		return err
	}
	response, err := c.WithResponseLimit(2<<20).apiClient.ExecuteRequest(ctx, request, path)
	if err != nil {
		return err
	}
	if response == nil {
		return fmt.Errorf("tracker response is unavailable")
	}
	if response.StatusCode != http.StatusOK {
		var envelope generated.ErrorEnvelope
		if json.Unmarshal(response.Content, &envelope) == nil && tracker.IsPublicErrorCode(envelope.ErrorData.Code) {
			envelope.Status = int64(response.StatusCode)
			envelope.ErrorData = generated.ErrorBody{Code: envelope.ErrorData.Code, Message: "tracker request failed"}
			return runtime.NewClientAPIError(envelope, runtime.WithStatusCode(response.StatusCode))
		}
		return runtime.NewClientAPIError(fmt.Errorf("tracker request failed"), runtime.WithStatusCode(response.StatusCode))
	}
	if json.Unmarshal(response.Content, target) != nil {
		return runtime.NewClientAPIError(fmt.Errorf("tracker response is invalid"), runtime.WithStatusCode(response.StatusCode))
	}
	return nil
}

// TrackerRecovery is owner-only local execution metadata. A blocked unknown
// does not become safe merely because a new process or account was selected.
type TrackerRecovery struct {
	RestoredHistoryGap   bool              `json:"restored_history_gap"`
	RootInstanceUID      string            `json:"root_instance_uid"`
	ProjectUID           string            `json:"project_uid"`
	Role                 string            `json:"role"`
	Selection            *TrackerSelection `json:"selection,omitempty"`
	WriteUID             string            `json:"write_uid,omitempty"`
	PendingEventUID      string            `json:"pending_event_uid,omitempty"`
	Phase                string            `json:"phase,omitempty"`
	ProviderAcknowledged bool              `json:"provider_acknowledged"`
	WritesBlocked        bool              `json:"writes_blocked"`
	ActivationRequired   bool              `json:"activation_required"`
}

func (c *Client) TrackerRecovery(ctx context.Context, projectID int64, editors ...RequestEditorFn) (*TrackerRecovery, error) {
	result := new(TrackerRecovery)
	err := c.trackerRequest(ctx, http.MethodGet, projectID, "/tracker-recovery", nil, result, editors...)
	return result, err
}

// SettleTrackerControl records an unresolved outcome for a stopped invocation
// under actual daemon-owner authority. It never resends the operation or clears
// an underlying provider write fence. It uses the original operation's UID.
func (c *Client) SettleTrackerControl(ctx context.Context, projectID int64, operationUID string, editors ...RequestEditorFn) (*TrackerReceipt, error) {
	if !uid.Valid(operationUID) {
		return nil, fmt.Errorf("tracker operation UID must be a normalized ULID")
	}
	result := new(TrackerReceipt)
	err := c.trackerRequest(ctx, http.MethodPost, projectID, "/tracker-operations/"+operationUID+"/settle", struct{}{}, result, editors...)
	if err != nil {
		return nil, err
	}
	if err := validateTrackerReceipt(result, operationUID); err != nil {
		return nil, err
	}
	return result, nil
}

func validateTrackerReceipt(r *TrackerReceipt, expectedUID string) error {
	// Validate the public subset of the native receipt. Private process/digest
	// fences are intentionally absent from this read-only wire representation.
	valid := r != nil && r.UID == expectedUID && uid.Valid(r.UID) && uid.Valid(r.RootInstanceUID) && uid.Valid(r.ProjectUID) && r.Selection.Validate() == nil && r.Kind.Action() != ""
	if valid && r.Settlement != nil {
		settlement := r.Settlement
		valid = r.State == tracker.ControlFailed && r.Result != nil && r.Result.Code == "tracker_outcome_unknown" && settlement.Kind == "abandon_unresolved_control" && !settlement.RecordedAt.IsZero() && settlement.Operator.Validate() == nil && settlement.Operator.AuthorityInstanceUID == r.RootInstanceUID && (settlement.Operator.Kind == tracker.SubjectOwnerLocal || settlement.Operator.Kind == tracker.SubjectStaticOwner) && settlement.Operator.Subject == r.RootInstanceUID
	}
	if valid {
		switch r.State {
		case tracker.ControlPending:
			valid = r.Result == nil
		case tracker.ControlComplete:
			valid = r.Result != nil && r.Result.Validate() == nil && r.Result.Code == "ok"
		case tracker.ControlFailed:
			valid = r.Result != nil && r.Result.Validate() == nil && r.Result.Code != "ok"
		case tracker.ControlRestored:
			valid = r.Result != nil && r.Result.Validate() == nil && r.Result.Code == "tracker_restored_inactive"
		default:
			valid = false
		}
	}
	if !valid {
		return runtime.NewClientAPIError(fmt.Errorf("tracker response is invalid"), runtime.WithStatusCode(http.StatusOK))
	}
	return nil
}
