package agentsviewrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strings"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// VerifySnapshot authenticates exact bytes and the small v1 identity projection
// before any forwarding. It never decodes message content, loads an archive or
// rewrites the envelope. Full closure validation belongs to AgentsView's codec.
// The caller must supply a frozen private spool, not a path reopened later.
func VerifySnapshot(ctx context.Context, r io.ReadSeeker, c Control) error {
	if c.SnapshotBytes <= 0 || c.SnapshotBytes > MaxSnapshotBytes {
		return errors.New("invalid snapshot size")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx, r}, MaxSnapshotBytes+1))
	if err != nil {
		return err
	}
	if n != c.SnapshotBytes || hex.EncodeToString(h.Sum(nil)) != c.SnapshotSHA256 {
		return errors.New("snapshot bytes differ from consent")
	}
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	d := jsontext.NewDecoder(io.LimitReader(contextReader{ctx, r}, MaxSnapshotBytes+1))
	var identity envelopeIdentity
	if err = readIdentityObject(d, "", &identity); err != nil {
		return err
	}
	if _, err = d.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("trailing envelope data")
	}
	if identity.version != 1 || (identity.manifestVersion < 2 || identity.manifestVersion > 7) || identity.sessionID != c.SessionID || identity.nativeID != c.SessionID || identity.id != c.SessionID || identity.agent != c.Agent || !identity.segments {
		return errors.New("snapshot envelope identity differs from consent")
	}
	_, err = r.Seek(0, io.SeekStart)
	return err
}

type envelopeIdentity struct {
	version, manifestVersion       int64
	sessionID, nativeID, id, agent string
	segments                       bool
}

func readIdentityObject(d *jsontext.Decoder, path string, v *envelopeIdentity) error {
	t, err := d.ReadToken()
	if err != nil {
		return err
	}
	if t.Kind() != '{' {
		return errors.New("expected envelope object")
	}
	for d.PeekKind() != '}' {
		key, err := d.ReadToken()
		if err != nil {
			return err
		}
		if key.Kind() != '"' {
			return errors.New("invalid envelope member")
		}
		// Escape JSON Pointer segments so a literal slash cannot impersonate nesting.
		segment := strings.ReplaceAll(strings.ReplaceAll(key.String(), "~", "~0"), "/", "~1")
		name := path + "/" + segment
		switch name {
		case "/manifest", "/manifest/session":
			if err := readIdentityObject(d, name, v); err != nil {
				return err
			}
		case "/v", "/manifest/v":
			tok, err := d.ReadToken()
			if err != nil {
				return err
			}
			if tok.Kind() != '0' {
				return errors.New("invalid envelope version")
			}
			version, err := tok.Int()
			if err != nil {
				return errors.New("invalid envelope version")
			}
			if name == "/v" {
				v.version = version
			} else {
				v.manifestVersion = version
			}
		case "/session_id", "/manifest/native_session_id", "/manifest/session/id", "/manifest/session/agent":
			tok, err := d.ReadToken()
			if err != nil {
				return err
			}
			if tok.Kind() != '"' || len(tok.String()) > 256 {
				return errors.New("invalid envelope identity")
			}
			switch name {
			case "/session_id":
				v.sessionID = tok.String()
			case "/manifest/native_session_id":
				v.nativeID = tok.String()
			case "/manifest/session/id":
				v.id = tok.String()
			case "/manifest/session/agent":
				v.agent = tok.String()
			}
		case "/segments":
			if d.PeekKind() != '[' {
				return errors.New("invalid envelope segments")
			}
			v.segments = true
			if err := d.SkipValue(); err != nil {
				return err
			}
		default:
			if err := d.SkipValue(); err != nil {
				return err
			}
		}
	}
	_, err = d.ReadToken()
	return err
}
