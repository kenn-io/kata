package db

import (
	"errors"
	"reflect"

	"go.kenn.io/kata/internal/uid"
)

// Expected context comes from staged row fields and independently enrolled root
// authority. Exact retained inventory bytes supply the two inventory digests.
type ScopedProtocolCutoverExpectation struct {
	UID                     string   `json:"uid"`
	AuthorityUID            string   `json:"authority_uid"`
	RequestDigest           string   `json:"request_digest"`
	ProjectUIDs             []string `json:"project_uids"`
	Floor                   int      `json:"floor"`
	ReadinessDigest         string   `json:"readiness_digest"`
	SuspendedBindingsDigest string   `json:"suspended_bindings_digest"`
}
type ScopedProtocolCutoverCertificate struct {
	Version    int                              `json:"version"`
	Cutover    ScopedProtocolCutoverExpectation `json:"cutover"`
	Signatures []ScopedSignature                `json:"signatures"`
}

func (e ScopedProtocolCutoverExpectation) Validate() error {
	if !uid.Valid(e.UID) || !uid.Valid(e.AuthorityUID) || e.Floor != ScopedRelayProtocolVersion || !scopedDigest(e.RequestDigest) || !scopedDigest(e.ReadinessDigest) || !scopedDigest(e.SuspendedBindingsDigest) || len(e.ProjectUIDs) == 0 || len(e.ProjectUIDs) > MaxScopedSelectedProjects {
		return errors.New("invalid scoped cutover expectation")
	}
	previous := ""
	for _, project := range e.ProjectUIDs {
		if !uid.Valid(project) || project <= previous {
			return errors.New("invalid scoped cutover project set")
		}
		previous = project
	}
	return nil
}
func (c ScopedProtocolCutoverCertificate) Validate() error {
	if c.Version != 1 {
		return errors.New("unknown scoped cutover certificate version")
	}
	return c.Cutover.Validate()
}
func ScopedCutoverCertificateSigningBytes(c ScopedProtocolCutoverCertificate) ([]byte, error) {
	c.Signatures = nil
	return scopedTranscript(ScopedCutoverDomain, c)
}
func VerifyScopedCutoverCertificate(c ScopedProtocolCutoverCertificate, expected ScopedProtocolCutoverExpectation, trusted []RootKeyPin) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(c.Cutover, expected) {
		return errors.New("scoped cutover differs from staged expected row")
	}
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	raw, err := ScopedCutoverCertificateSigningBytes(c)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, c.Signatures, expected.ProjectUIDs, expected.AuthorityUID, pins, nil)
}
