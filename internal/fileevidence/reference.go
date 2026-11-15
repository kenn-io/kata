// Package fileevidence resolves operator-configured file references, without retaining bytes.
package fileevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"regexp"
	"strings"
)

// MaxBytes limits each complete resolved file.
const MaxBytes = 16 << 20

var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Reference identifies a dataset and relative file, not a client mount path.
// CaptureSHA256 is request-only; all other provenance is daemon-produced.
type Reference struct {
	provenanceSupplied bool
	Source             string `json:"source"`
	Path               string `json:"path"`
	CaptureSHA256      *bool  `json:"capture_sha256,omitempty" writeOnly:"true"`
	Origin             string `json:"origin,omitempty" readOnly:"true"`
	SHA256             string `json:"sha256,omitempty" readOnly:"true"`
	Size               *int64 `json:"size,omitempty" readOnly:"true"`
	Revision           string `json:"revision,omitempty" readOnly:"true"`
}

func (r Reference) validatePath() error {
	if !aliasPattern.MatchString(r.Source) || len(r.Path) > 4096 || !fs.ValidPath(r.Path) || r.Path == "." || strings.ContainsAny(r.Path, "\\:\x00") {
		return errors.New("file evidence requires a source alias and canonical portable relative path")
	}
	return nil
}

// ValidateRequest refuses caller-authored provenance. Bytes are always resolved by the daemon.
func (r Reference) ValidateRequest() error {
	if err := r.validatePath(); err != nil {
		return err
	}
	if r.provenanceSupplied || r.Origin != "" || r.SHA256 != "" || r.Size != nil || r.Revision != "" {
		return errors.New("file provenance is daemon-produced; supply only source, path, capture_sha256")
	}
	return nil
}

// ValidateRetained validates replayed metadata without touching any source.
func (r Reference) ValidateRetained() error {
	if err := r.validatePath(); err != nil {
		return err
	}
	if r.Origin == "" || len(r.Origin) > 256 || r.Size == nil || *r.Size < 0 || *r.Size > MaxBytes || r.CaptureSHA256 != nil || len(r.Revision) > 4096 {
		return errors.New("inconsistent retained file provenance")
	}
	if r.SHA256 != "" {
		raw, err := hex.DecodeString(r.SHA256)
		if err != nil || len(raw) != 32 || r.SHA256 != strings.ToLower(r.SHA256) {
			return errors.New("file sha256 must be 64 lowercase hexadecimal characters")
		}
	}
	return nil
}

func describe(r Reference, raw []byte, hash bool) Reference {
	n := int64(len(raw))
	r.Size = &n
	r.CaptureSHA256 = nil
	r.SHA256 = ""
	if hash {
		sum := sha256.Sum256(raw)
		r.SHA256 = hex.EncodeToString(sum[:])
	}
	return r
}

// UnmarshalJSON records output-field presence, including explicit null/zero.
func (r *Reference) UnmarshalJSON(raw []byte) error {
	type plain Reference
	var v plain
	if err := json.Unmarshal(raw, &v, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"origin", "sha256", "size", "revision"} {
		if _, ok := fields[name]; ok {
			v.provenanceSupplied = true
		}
	}
	*r = Reference(v)
	return nil
}
