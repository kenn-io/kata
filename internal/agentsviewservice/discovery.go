// Package agentsviewservice describes root-owned AgentsView destinations.
// Discovery never reads a transcript or receiver credential and grants no upload.
package agentsviewservice

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"go.kenn.io/kata/internal/agentsviewwire"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/uid"
)

const (
	// MaxDiscoveryBytes bounds a discovery response.
	MaxDiscoveryBytes = 16 << 10
	// MaxSnapshotBytes bounds a shared snapshot.
	MaxSnapshotBytes int64 = 64 << 20
	// MaxLifetime bounds descriptor validity.
	MaxLifetime = 5 * time.Minute
	// ClockSkew permits small differences between participant clocks.
	ClockSkew = 5 * time.Second
	// MaxRouteNodes allows eight hubs plus a final leaf.
	MaxRouteNodes = 9
)

// Descriptor is the root-signed destination descriptor.
type Descriptor = agentsviewwire.Descriptor

// TrustedRoot must come from the daemon's already-enrolled pin. A descriptor
// or a response carrying a public key cannot establish this trust input.
type TrustedRoot struct {
	ProjectUID   string
	AuthorityUID string
	KeyID        string
	PublicKey    ed25519.PublicKey
	Retired      bool
}

// RouteObservation records one negotiated hop.
type RouteObservation = agentsviewwire.RouteObservation

// Discovery contains the descriptor and its observed route.
type Discovery = agentsviewwire.Discovery

// CanonicalBase rejects credential-bearing/ambiguous destinations without
// including their text in errors. HTTP is allowed only for loopback services.
func CanonicalBase(raw string) (string, error) {
	return httpurl.CanonicalHTTPSOrLoopbackBase(raw)
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// ServiceID commits to canonical non-secret policy. Receiver credentials are
// deliberately not inputs and are never needed for discovery.
func ServiceID(receiver, locator string) (string, error) {
	receiver, err := CanonicalBase(receiver)
	if err != nil {
		return "", err
	}
	locator, err = CanonicalBase(locator)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Receiver string
		Locator  string
		Version  int
	}{receiver, locator, 1})
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

func descriptorBytes(d Descriptor) ([]byte, error) {
	d.Signature = nil
	return json.Marshal(struct {
		Domain     string     `json:"domain"`
		Descriptor Descriptor `json:"descriptor"`
	}{"kata.project-trace-service.v1", d})
}

func descriptorShape(d Descriptor, now time.Time) error {
	base, err := CanonicalBase(d.LocatorBase)
	key, keyErr := hex.DecodeString(d.KeyID)
	service, serviceErr := hex.DecodeString(d.ServiceID)
	if d.Version != 1 || d.EnvelopeVersion != 1 || !uid.Valid(d.ProjectUID) || !uid.Valid(d.AuthorityUID) ||
		d.MaxBytes != MaxSnapshotBytes || err != nil || base != d.LocatorBase ||
		keyErr != nil || len(key) != sha256.Size || strings.ToLower(d.KeyID) != d.KeyID ||
		serviceErr != nil || len(service) != sha256.Size || strings.ToLower(d.ServiceID) != d.ServiceID ||
		d.IssuedAt <= 0 || d.ExpiresAt <= d.IssuedAt || d.ExpiresAt-d.IssuedAt > int64(MaxLifetime/time.Second) ||
		d.IssuedAt > now.Add(ClockSkew).Unix() || d.ExpiresAt < now.Add(-ClockSkew).Unix() {
		return errors.New("AgentsView destination invalid or expired")
	}
	return nil
}

// SignDescriptor uses already-loaded signing material; it creates no key or pin.
func SignDescriptor(d Descriptor, key ed25519.PrivateKey) (Descriptor, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Descriptor{}, errors.New("AgentsView root signer unavailable")
	}
	d.KeyID = digest(key.Public().(ed25519.PublicKey))
	if err := descriptorShape(d, time.Unix(d.IssuedAt, 0)); err != nil {
		return Descriptor{}, err
	}
	raw, err := descriptorBytes(d)
	if err != nil {
		return Descriptor{}, err
	}
	d.Signature = ed25519.Sign(key, raw)
	return d, nil
}

// VerifyDescriptor validates the descriptor against the enrolled root pin.
func VerifyDescriptor(d Descriptor, pin TrustedRoot, now time.Time) error {
	if err := descriptorShape(d, now); err != nil {
		return err
	}
	if pin.Retired || len(pin.PublicKey) != ed25519.PublicKeySize || pin.KeyID != digest(pin.PublicKey) ||
		d.KeyID != pin.KeyID || d.ProjectUID != pin.ProjectUID || d.AuthorityUID != pin.AuthorityUID || len(d.Signature) != ed25519.SignatureSize {
		return errors.New("AgentsView service does not match enrolled root")
	}
	raw, err := descriptorBytes(d)
	if err != nil || !ed25519.Verify(pin.PublicKey, raw, d.Signature) {
		return errors.New("invalid AgentsView service root signature")
	}
	return nil
}

// validateRoute checks the complete observed path against negotiated identity.
// No URL, display name or response-supplied key substitutes for that path.
func validateRoute(d Discovery, project string, path []string) error {
	if len(path) == 0 || len(path) > MaxRouteNodes || len(path) != len(d.Route) ||
		d.Descriptor.ProjectUID != project || d.Descriptor.AuthorityUID != path[0] || d.InstanceUID != path[len(path)-1] {
		return errors.New("AgentsView destination mismatch")
	}
	seen := make(map[string]bool, len(path))
	for i, hop := range d.Route {
		if !uid.Valid(hop.InstanceUID) || seen[hop.InstanceUID] || hop.InstanceUID != path[i] || hop.ProjectUID != project {
			return errors.New("invalid AgentsView service route")
		}
		seen[hop.InstanceUID] = true
		if i == 0 {
			if hop.BindingUID != "" || hop.UpstreamInstanceUID != "" || hop.ResetEpoch != 0 {
				return errors.New("invalid transcript root route")
			}
		} else if !uid.Valid(hop.BindingUID) || hop.UpstreamInstanceUID != path[i-1] || hop.ResetEpoch <= 0 {
			return errors.New("invalid relay connection")
		}
	}
	return nil
}

// DecodeDiscovery parses and verifies a discovery against the enrolled root.
func DecodeDiscovery(raw []byte, pin TrustedRoot, path []string, now time.Time) (Discovery, error) {
	d, err := ParseDiscovery(raw, pin.ProjectUID, path, now)
	if err != nil {
		return Discovery{}, err
	}
	if err = VerifyDescriptor(d.Descriptor, pin, now); err != nil {
		return Discovery{}, err
	}
	return d, nil
}

// ParseDiscovery validates the bounded response received from the selected
// authenticated daemon. That daemon verifies its enrolled root pin; CLI clients
// must not manufacture a pin from this response and claim independent trust.
func ParseDiscovery(raw []byte, project string, path []string, now time.Time) (Discovery, error) {
	var d Discovery
	if len(raw) > MaxDiscoveryBytes {
		return d, errors.New("AgentsView service response too large")
	}
	if err := json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
		return Discovery{}, errors.New("invalid AgentsView service response")
	}
	if len(d.Descriptor.Signature) != ed25519.SignatureSize {
		return Discovery{}, errors.New("invalid AgentsView response")
	}
	if err := descriptorShape(d.Descriptor, now); err != nil {
		return Discovery{}, err
	}
	if path == nil {
		for _, hop := range d.Route {
			path = append(path, hop.InstanceUID)
		}
	}
	if err := validateRoute(d, project, path); err != nil {
		return Discovery{}, err
	}
	return d, nil
}
