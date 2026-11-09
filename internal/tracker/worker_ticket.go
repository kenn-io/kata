package tracker

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"
	"time"

	"go.kenn.io/kata/internal/uid"
)

const WorkerProtocolVersion = 1

type WorkerPurpose string

const (
	WorkerExecute     WorkerPurpose = "execute"
	WorkerReserve     WorkerPurpose = "reserve"
	WorkerPermit      WorkerPurpose = "permit"
	WorkerAcknowledge WorkerPurpose = "acknowledge"
)

// WorkerTicket is a root-signed, exact typed call, not a transport credential.
// It contains no provider configuration, credential selector or bearer token.
// Signatures do not consume permits: native full-value CAS provides one-use.
// Separate purposes prevent execution, dispatch and acknowledgement confusion.
type WorkerTicket struct {
	Version           int           `json:"version"`
	Purpose           WorkerPurpose `json:"purpose"`
	KeyID             string        `json:"key_id"`
	RootInstanceUID   string        `json:"root_instance_uid"`
	ProjectUID        string        `json:"project_uid"`
	Selection         Selection     `json:"selection"`
	Principal         Principal     `json:"principal"`
	RootActor         string        `json:"root_actor"`
	Action            Action        `json:"action"`
	Resource          string        `json:"resource"`
	SourceKey         string        `json:"source_key,omitempty"`
	Provider          string        `json:"provider"`
	Method            string        `json:"method"`
	RunUID            string        `json:"run_uid"`
	CallUID           string        `json:"call_uid"`
	SessionUID        string        `json:"session_uid"`
	RootEpoch         string        `json:"root_epoch"`
	OwnerEpoch        string        `json:"owner_epoch"`
	ArgumentsDigest   string        `json:"arguments_digest"`
	WriteUID          string        `json:"write_uid,omitempty"`
	ReservationDigest string        `json:"reservation_digest,omitempty"`
	ExpiresAt         int64         `json:"expires_at"`
}
type SignedWorkerTicket struct {
	Ticket    WorkerTicket `json:"ticket"`
	Signature []byte       `json:"signature"`
}

func (t WorkerTicket) Validate() error {
	if t.Version != WorkerProtocolVersion || !ValidIdentityText(t.KeyID, 256) || !uid.Valid(t.RootInstanceUID) || !uid.Valid(t.ProjectUID) || t.Selection.Validate() != nil || t.Principal.Validate() != nil || !ValidIdentityText(t.RootActor, 256) || !ValidIdentityText(t.Resource, 2048) || !uid.Valid(t.RunUID) || !uid.Valid(t.CallUID) || !uid.Valid(t.SessionUID) || !uid.Valid(t.RootEpoch) || !uid.Valid(t.OwnerEpoch) || !DigestValid(t.ArgumentsDigest) || t.ExpiresAt <= 0 {
		return ErrInvalid
	}
	switch t.Principal.Kind {
	case SubjectAccount, SubjectService:
	case SubjectOwnerLocal, SubjectStaticOwner:
		if t.Principal.Subject != t.Principal.AuthorityInstanceUID {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	if t.Action == ActionStatus && t.Principal.Kind != SubjectService {
		return ErrDenied
	}
	if t.SourceKey != "" && !ValidIdentityText(t.SourceKey, 2048) {
		return ErrInvalid
	}
	if t.Principal.Kind == SubjectService && t.Principal.AuthorityInstanceUID != t.RootInstanceUID {
		return ErrDenied
	}
	if !WorkerMethodAllowed(t.Provider, t.Method, t.Action) {
		return ErrInvalid
	}
	switch t.Purpose {
	case WorkerExecute:
		if t.Method == t.Provider+".acknowledge" {
			return ErrInvalid
		}
		if t.WriteUID != "" || t.ReservationDigest != "" {
			return ErrInvalid
		}
	case WorkerReserve, WorkerPermit:
		if t.Action != ActionStatus || !DigestValid(t.WriteUID) || !DigestValid(t.ReservationDigest) {
			return ErrInvalid
		}
	case WorkerAcknowledge:
		if t.Action != ActionRecover || t.Method != t.Provider+".acknowledge" || !DigestValid(t.WriteUID) || !DigestValid(t.ReservationDigest) || t.ReservationDigest != t.ArgumentsDigest {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func workerTicketTranscript(t WorkerTicket) ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Domain string       `json:"domain"`
		Ticket WorkerTicket `json:"ticket"`
	}{"kata:tracker-worker:" + string(t.Purpose) + ":v1", t}, json.Deterministic(true))
}
func SignWorkerTicket(t WorkerTicket, key ed25519.PrivateKey) (SignedWorkerTicket, error) {
	raw, err := workerTicketTranscript(t)
	if err != nil {
		return SignedWorkerTicket{}, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return SignedWorkerTicket{}, ErrUnavailable
	}
	return SignedWorkerTicket{Ticket: t, Signature: ed25519.Sign(key, raw)}, nil
}

// Verify requires a caller-supplied retained project pin; never accept a key
// supplied by the ticket. Metadata recovery uses a new bounded acknowledgement,
// not an expired execution ticket. Clock bounds are prerequisites, not leases
// that can erase unknown provider outcomes.
func (s SignedWorkerTicket) Verify(pin ed25519.PublicKey, root, project, keyID string, purpose WorkerPurpose, now time.Time) error {
	t := s.Ticket
	raw, err := workerTicketTranscript(t)
	if err != nil {
		return err
	}
	if t.RootInstanceUID != root || t.ProjectUID != project || t.KeyID != keyID || t.Purpose != purpose || len(pin) != ed25519.PublicKeySize || len(s.Signature) != ed25519.SignatureSize || !ed25519.Verify(pin, raw, s.Signature) {
		return ErrDenied
	}
	maxLifetime := 30 * time.Second
	if purpose == WorkerPermit {
		maxLifetime = 5 * time.Second
	}
	if t.ExpiresAt <= now.Unix() || t.ExpiresAt > now.Add(maxLifetime).Unix() {
		return ErrUnavailable
	}
	return nil
}
func WorkerArgumentsDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// WorkerMethodAllowed is an explicit provider interface, never a generic HTTP
// proxy. Binding preflight reuses read methods under an exact bind grant.
func WorkerMethodAllowed(provider, method string, action Action) bool {
	if !SupportedProvider(provider) || !strings.HasPrefix(method, provider+".") {
		return false
	}
	name := strings.TrimPrefix(method, provider+".")
	if name == "head" || name == "receipt" || name == "retire-prepared" || name == "acknowledge" {
		return action == ActionRecover
	}
	if name == "close" {
		return action == ActionRecover
	}
	if name == "discover" {
		return action == ActionDiscover
	}
	if name == "authorize" {
		return validAction(action)
	}
	if name == "write-status" {
		return action == ActionStatus
	}
	if action != ActionRead && action != ActionBind {
		return false
	}
	if name == "read-status" {
		return true
	}
	switch provider {
	case "github":
		return name == "status-locators" || name == "repository" || name == "issues" || name == "comments" || name == "repository-comments" || name == "parents"
	case "notion":
		return name == "database" || name == "data-source" || name == "pages" || name == "content"
	case "linear":
		return name == "scope" || name == "states" || name == "issues"
	case "plane":
		return name == "project" || name == "states" || name == "work-items"
	case "ticktick":
		return name == "project" || name == "data" || name == "task"
	case "todoist":
		return name == "account" || name == "project" || name == "tasks"
	case "twenty":
		return name == "workspace" || name == "schema" || name == "tasks"
	}
	return false
}
