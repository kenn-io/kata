package tracker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

const WorkerChunkBytes = 512 << 10
const WorkerResultBytes = 64 << 20
const WorkerProjectBytes = 128 << 20
const WorkerDaemonBytes = 512 << 20

// Result manifests are volatile provider data. They never authorize a write or
// replace the small durable original status receipt. Raw-byte quotas apply in
// addition to transport's encoded-body cap and the broker's retained-byte caps.
type WorkerResultManifest struct {
	Version          int      `json:"version"`
	RootInstanceUID  string   `json:"root_instance_uid"`
	ProjectUID       string   `json:"project_uid"`
	OwnerInstanceUID string   `json:"owner_instance_uid"`
	RunUID           string   `json:"run_uid"`
	CallUID          string   `json:"call_uid"`
	SessionUID       string   `json:"session_uid"`
	Size             int      `json:"size"`
	Digest           string   `json:"digest"`
	Chunks           []string `json:"chunks"`
}
type WorkerResultChunk struct {
	RootInstanceUID  string `json:"root_instance_uid"`
	ProjectUID       string `json:"project_uid"`
	OwnerInstanceUID string `json:"owner_instance_uid"`
	CallUID          string `json:"call_uid"`
	SessionUID       string `json:"session_uid"`
	Ordinal          int    `json:"ordinal"`
	Data             []byte `json:"data"`
}

func (m WorkerResultManifest) matches(t WorkerTicket) bool {
	return t.Validate() == nil && m.Version == WorkerProtocolVersion && m.RootInstanceUID == t.RootInstanceUID && m.ProjectUID == t.ProjectUID && m.OwnerInstanceUID == t.Selection.OwnerInstanceUID && m.RunUID == t.RunUID && m.CallUID == t.CallUID && m.SessionUID == t.SessionUID
}
func resultDigest(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func SplitWorkerResult(t WorkerTicket, data []byte) (WorkerResultManifest, []WorkerResultChunk, error) {
	if t.Validate() != nil || len(data) == 0 || len(data) > WorkerResultBytes {
		return WorkerResultManifest{}, nil, ErrInvalid
	}
	m := WorkerResultManifest{Version: 1, RootInstanceUID: t.RootInstanceUID, ProjectUID: t.ProjectUID, OwnerInstanceUID: t.Selection.OwnerInstanceUID, RunUID: t.RunUID, CallUID: t.CallUID, SessionUID: t.SessionUID, Size: len(data), Digest: resultDigest(data)}
	var chunks []WorkerResultChunk
	for start := 0; start < len(data); start += WorkerChunkBytes {
		end := min(start+WorkerChunkBytes, len(data))
		part := bytes.Clone(data[start:end])
		m.Chunks = append(m.Chunks, resultDigest(part))
		chunks = append(chunks, WorkerResultChunk{RootInstanceUID: m.RootInstanceUID, ProjectUID: m.ProjectUID, OwnerInstanceUID: m.OwnerInstanceUID, CallUID: m.CallUID, SessionUID: m.SessionUID, Ordinal: len(chunks), Data: part})
	}
	return m, chunks, nil
}

type WorkerResultAssembler struct {
	manifest WorkerResultManifest
	data     []byte
	next     int
	closed   bool
}

func NewWorkerResultAssembler(m WorkerResultManifest, t WorkerTicket) (*WorkerResultAssembler, error) {
	if !m.matches(t) || m.Size <= 0 || m.Size > WorkerResultBytes || !DigestValid(m.Digest) || len(m.Chunks) != (m.Size+WorkerChunkBytes-1)/WorkerChunkBytes {
		return nil, ErrInvalid
	}
	for _, digest := range m.Chunks {
		if !DigestValid(digest) {
			return nil, ErrInvalid
		}
	}
	m.Chunks = append([]string(nil), m.Chunks...)
	return &WorkerResultAssembler{manifest: m}, nil
}
func (a *WorkerResultAssembler) Add(c WorkerResultChunk) error {
	if a == nil || a.closed {
		return ErrUnavailable
	}
	m := a.manifest
	if c.RootInstanceUID != m.RootInstanceUID || c.ProjectUID != m.ProjectUID || c.OwnerInstanceUID != m.OwnerInstanceUID || c.CallUID != m.CallUID || c.SessionUID != m.SessionUID || c.Ordinal < 0 || c.Ordinal > a.next || c.Ordinal >= len(m.Chunks) {
		return ErrInvalid
	}
	if c.Ordinal < a.next {
		start := c.Ordinal * WorkerChunkBytes
		end := min(start+WorkerChunkBytes, len(a.data))
		if resultDigest(c.Data) == m.Chunks[c.Ordinal] && bytes.Equal(c.Data, a.data[start:end]) {
			return nil
		}
		return ErrInvalid
	}
	want := min(WorkerChunkBytes, m.Size-len(a.data))
	if len(c.Data) != want || resultDigest(c.Data) != m.Chunks[a.next] {
		return ErrInvalid
	}
	a.data = append(a.data, c.Data...)
	a.next++
	return nil
}
func (a *WorkerResultAssembler) Complete() ([]byte, error) {
	if a == nil || a.closed {
		return nil, ErrUnavailable
	}
	if a.next != len(a.manifest.Chunks) || len(a.data) != a.manifest.Size || resultDigest(a.data) != a.manifest.Digest {
		return nil, ErrInvalid
	}
	return bytes.Clone(a.data), nil
}
func (a *WorkerResultAssembler) Close() {
	if a != nil {
		clear(a.data)
		a.data = nil
		a.closed = true
	}
}
