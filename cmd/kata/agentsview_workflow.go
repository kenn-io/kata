package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"go.kenn.io/kata/internal/transcript"
)

// Consumer-local v1 DTOs follow the shared server contract. They deliberately
// avoid generated symbols from unmerged prerequisites. Wire names stay generic.
type agentsViewRoute struct {
	InstanceUID         string `json:"instance_uid"`
	ProjectUID          string `json:"project_uid"`
	BindingUID          string `json:"binding_uid,omitempty"`
	UpstreamInstanceUID string `json:"upstream_instance_uid,omitempty"`
	ResetEpoch          int64  `json:"reset_epoch,omitempty"`
}
type agentsViewAssociation struct {
	Version         int               `json:"v"`
	IssueUID        string            `json:"issue_uid"`
	ProjectUID      string            `json:"project_uid"`
	AuthorityUID    string            `json:"authority_uid"`
	ServiceID       string            `json:"service_id"`
	RootKeyID       string            `json:"root_key_id"`
	Agent           string            `json:"agent"`
	NativeSessionID string            `json:"native_session_id"`
	SessionID       string            `json:"session_id"`
	SnapshotSHA256  string            `json:"snapshot_sha256"`
	SnapshotBytes   int64             `json:"snapshot_bytes"`
	LocatorBase     string            `json:"locator_base"`
	Route           []agentsViewRoute `json:"route"`
}
type agentsViewConsent struct {
	Version     int                   `json:"v"`
	InstanceUID string                `json:"instance_uid"`
	Association agentsViewAssociation `json:"association"`
}
type agentsViewNativeIdentity struct{ Agent, NativeSessionID string }

func (n agentsViewNativeIdentity) canonical() (string, error) {
	ref := transcript.Transcript{Agent: n.Agent, SessionID: n.NativeSessionID}
	if err := ref.Validate(); err != nil {
		return "", errors.New("AgentsView: exact current native session unavailable or ambiguous")
	}
	if n.Agent == "claude" {
		return n.NativeSessionID, nil
	}
	return n.Agent + ":" + n.NativeSessionID, nil
}

type agentsViewSelection struct {
	InstanceUID, IssueUID, ProjectUID, AuthorityUID, ServiceID, RootKeyID, LocatorBase string
	Route                                                                              []agentsViewRoute
	Discovery                                                                          jsontext.Value
	ExpiresAt                                                                          int64
}
type agentsViewCloseEvent struct {
	UID, Type, IssueUID, ProjectUID string
	Association                     agentsViewAssociation
}
type agentsViewCloseResult struct {
	Changed, Reused bool
	Event           *agentsViewCloseEvent
	Raw             []byte
}

// Select returns live verified policy pinned to the enrolled root and route.
// Share returns nil ONLY after verifying the bounded root-signed receipt against
// that pin and the full association. Missing adapters return unavailable.
type agentsViewWorkflowPort interface {
	Select(context.Context, string) (agentsViewSelection, error)
	Close(context.Context, string, map[string]any, map[string]string) (agentsViewCloseResult, error)
	Original(context.Context, string, string) (agentsViewCloseEvent, error)
	Share(context.Context, string, agentsViewConsent, string, io.Reader) error
}
type agentsViewWorkflow struct{ port agentsViewWorkflowPort }

func (w agentsViewWorkflow) Prepare(ctx context.Context, ref, path string, native agentsViewNativeIdentity) (agentsViewConsent, error) {
	canonical, err := native.canonical()
	if err != nil {
		return agentsViewConsent{}, err
	}
	snapshot, err := captureAgentsViewSnapshot(ctx, path)
	if err != nil {
		return agentsViewConsent{}, err
	}
	defer snapshot.Close()
	if err = inspectAgentsViewSnapshot(ctx, snapshot, native.Agent, canonical); err != nil {
		return agentsViewConsent{}, err
	}
	selected, err := w.port.Select(ctx, ref)
	if err != nil {
		return agentsViewConsent{}, err
	}
	if err = selected.valid(); err != nil {
		return agentsViewConsent{}, err
	}
	return agentsViewConsent{Version: 1, InstanceUID: selected.InstanceUID, Association: agentsViewAssociation{
		Version: 1, IssueUID: selected.IssueUID, ProjectUID: selected.ProjectUID, AuthorityUID: selected.AuthorityUID,
		ServiceID: selected.ServiceID, RootKeyID: selected.RootKeyID, Agent: native.Agent, NativeSessionID: native.NativeSessionID, SessionID: canonical,
		SnapshotSHA256: snapshot.digest, SnapshotBytes: snapshot.size, LocatorBase: selected.LocatorBase,
		Route: append([]agentsViewRoute(nil), selected.Route...),
	}}, nil
}
func (s agentsViewSelection) valid() error {
	if s.InstanceUID == "" || s.IssueUID == "" || s.ProjectUID == "" || s.AuthorityUID == "" || !validAgentsViewDigest(s.ServiceID) || !validAgentsViewDigest(s.RootKeyID) || s.LocatorBase == "" || len(s.Route) == 0 || len(s.Route) > 9 || len(s.Discovery) == 0 || int64(len(s.Discovery)) > agentsViewRecordLimit || s.ExpiresAt <= time.Now().Unix() {
		return errors.New("AgentsView: current pinned service discovery unavailable or expired")
	}
	return nil
}
func (c agentsViewConsent) valid() error {
	a := c.Association
	canonical, err := (agentsViewNativeIdentity{a.Agent, a.NativeSessionID}).canonical()
	if err != nil || canonical != a.SessionID || c.Version != 1 || a.Version != 1 || c.InstanceUID == "" || a.IssueUID == "" || a.ProjectUID == "" || a.AuthorityUID == "" || !validAgentsViewDigest(a.ServiceID) || !validAgentsViewDigest(a.RootKeyID) || !validAgentsViewDigest(a.SnapshotSHA256) || a.SnapshotBytes <= 0 || a.SnapshotBytes > agentsViewSnapshotLimit || len(a.Route) == 0 || len(a.Route) > 9 || a.LocatorBase == "" {
		return errors.New("AgentsView: invalid retained consent record")
	}
	return nil
}
func (c agentsViewConsent) matches(s agentsViewSelection) bool {
	a := c.Association
	return c.InstanceUID == s.InstanceUID && a.IssueUID == s.IssueUID && a.ProjectUID == s.ProjectUID && a.AuthorityUID == s.AuthorityUID && a.ServiceID == s.ServiceID && a.RootKeyID == s.RootKeyID && a.LocatorBase == s.LocatorBase && reflect.DeepEqual(a.Route, s.Route)
}
func (c agentsViewConsent) matchesEvent(event agentsViewCloseEvent, uid string) bool {
	return uid != "" && event.UID == uid && event.Type == "issue.closed" && event.IssueUID == c.Association.IssueUID && event.ProjectUID == c.Association.ProjectUID && reflect.DeepEqual(event.Association, c.Association)
}
func (w agentsViewWorkflow) preflight(ctx context.Context, ref string, c agentsViewConsent, path, digest string) (*agentsViewFrozenSnapshot, agentsViewSelection, error) {
	if err := c.valid(); err != nil {
		return nil, agentsViewSelection{}, err
	}
	if digest != c.Association.SnapshotSHA256 {
		return nil, agentsViewSelection{}, errors.New("AgentsView: confirm the exact reviewed snapshot with --confirm-transcript-sha256")
	}
	snapshot, err := freezeAgentsViewSnapshot(ctx, path, digest, c.Association.SnapshotBytes)
	if err != nil {
		return nil, agentsViewSelection{}, err
	}
	selected, err := w.port.Select(ctx, ref)
	if err == nil {
		err = selected.valid()
	}
	if err == nil && !c.matches(selected) {
		err = errors.New("AgentsView: selected issue, daemon, project, root, service or route changed; sharing refused")
	}
	if err == nil {
		err = inspectAgentsViewSnapshot(ctx, snapshot, c.Association.Agent, c.Association.SessionID)
	}
	if err != nil {
		_ = snapshot.Close()
		return nil, agentsViewSelection{}, err
	}
	return snapshot, selected, nil
}

func (w agentsViewWorkflow) Close(ctx context.Context, ref string, c agentsViewConsent, path, digest string, native agentsViewNativeIdentity, dry bool, body map[string]any, headers map[string]string) (agentsViewCloseResult, error) {
	if native.Agent != c.Association.Agent || native.NativeSessionID != c.Association.NativeSessionID {
		return agentsViewCloseResult{}, errors.New("AgentsView: closing native session differs from prepared snapshot")
	}
	snapshot, selected, err := w.preflight(ctx, ref, c, path, digest)
	if err != nil {
		return agentsViewCloseResult{}, err
	}
	defer snapshot.Close()
	request := make(map[string]any, len(body)+3)
	for key, value := range body {
		request[key] = value
	}
	request["agentsview_association"] = c.Association
	request["agentsview_discovery"] = selected.Discovery
	request["dry_run"] = dry
	result, err := w.port.Close(ctx, ref, request, headers)
	if err != nil {
		return result, errors.New("AgentsView: close response unconfirmed; no upload attempted. Retain files and recover the exact original event before explicit share")
	}
	if dry || !result.Changed || result.Reused {
		return result, nil
	}
	if result.Event == nil || !c.matchesEvent(*result.Event, result.Event.UID) {
		return result, errors.New("AgentsView: close event unconfirmed or association mismatch; no upload attempted")
	}
	// Revalidate policy after commit. A later failure never repeats close or
	// activates the direct runner. The same private spool supplies upload bytes.
	latest, err := w.port.Select(ctx, ref)
	if err == nil {
		err = latest.valid()
	}
	if err == nil && !c.matches(latest) {
		err = errors.New("selected route changed after close")
	}
	if err == nil {
		err = w.port.Share(ctx, ref, c, result.Event.UID, snapshot)
	}
	if err != nil {
		return result, fmt.Errorf("AgentsView: issue closed; upload unconfirmed for event %s. Retain files for explicit same-snapshot share: %w", result.Event.UID, err)
	}
	return result, nil
}
func (w agentsViewWorkflow) Share(ctx context.Context, ref string, c agentsViewConsent, path, eventUID, digest string) error {
	if eventUID == "" {
		return errors.New("AgentsView: --close-event must name the original associated close")
	}
	snapshot, _, err := w.preflight(ctx, ref, c, path, digest)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	original, err := w.port.Original(ctx, ref, eventUID)
	if err != nil {
		return err
	}
	if !c.matchesEvent(original, eventUID) {
		return errors.New("AgentsView: original close proof missing or association changed; ordinary closes cannot acquire a snapshot later")
	}
	if err = w.port.Share(ctx, ref, c, eventUID, snapshot); err != nil {
		return fmt.Errorf("AgentsView: upload unconfirmed; retain the original files for explicit same-snapshot retry: %w", err)
	}
	return nil
}

// Inspect only identity fields. Closure/fidelity validation belongs to the
// AgentsView export/ingest codec. Skip segment content without reserializing it.
func inspectAgentsViewSnapshot(ctx context.Context, snapshot *agentsViewFrozenSnapshot, agent, canonical string) error {
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decoder := jsontext.NewDecoder(agentsViewContextReader{ctx, snapshot})
	values := map[string]string{}
	var walk func(string) error
	walk = func(path string) error {
		token, err := decoder.ReadToken()
		if err != nil || token.Kind() != '{' {
			return errors.New("invalid envelope object")
		}
		for decoder.PeekKind() != '}' {
			name, err := decoder.ReadToken()
			if err != nil || name.Kind() != '"' || len(name.String()) > 256 {
				return errors.New("invalid envelope member")
			}
			field := path + "/" + name.String()
			switch field {
			case "/manifest", "/manifest/session":
				if err := walk(field); err != nil {
					return err
				}
			case "/v", "/manifest/v":
				token, err := decoder.ReadToken()
				if err != nil || token.Kind() != '0' || token.String() != "1" {
					return errors.New("unsupported envelope version")
				}
				values[field] = "1"
			case "/session_id", "/manifest/native_session_id", "/manifest/session/id", "/manifest/session/agent":
				token, err := decoder.ReadToken()
				if err != nil || token.Kind() != '"' || len(token.String()) > 160 {
					return errors.New("invalid envelope identity")
				}
				values[field] = token.String()
			default:
				if err := decoder.SkipValue(); err != nil {
					return err
				}
			}
		}
		_, err = decoder.ReadToken()
		return err
	}
	err := walk("")
	if err == nil {
		_, err = decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("trailing envelope JSON")
		}
	}
	if err != nil || values["/v"] != "1" || values["/manifest/v"] != "1" || values["/session_id"] != canonical || values["/manifest/native_session_id"] != canonical || values["/manifest/session/id"] != canonical || values["/manifest/session/agent"] != agent {
		return errors.New("AgentsView: snapshot envelope identity differs from selected session or is unsupported")
	}
	_, err = snapshot.Seek(0, io.SeekStart)
	return err
}

func decodeAgentsViewConsent(raw []byte) (agentsViewConsent, error) {
	var c agentsViewConsent
	if len(raw) == 0 || int64(len(raw)) > agentsViewRecordLimit {
		return c, errors.New("AgentsView: retained consent exceeds limit")
	}
	if err := json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)); err != nil {
		return c, errors.New("AgentsView: invalid retained consent JSON")
	}
	return c, c.valid()
}
