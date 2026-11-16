package sqlitestore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestExportStopsAfterCancellation(t *testing.T) {
	for _, kind := range []string{"attribution", "outbox", "inbox", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			d, err := Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			p, err := d.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			const binding = "01HZNQ7VFPK1XGD8R5MABCD4EZ"
			const authority = "01HZNQ7VFPK1XGD8R5MABCD4EY"
			const source = "01HZNQ7VFPK1XGD8R5MABCD4EX"
			hash := strings.Repeat("a", 64)
			envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: binding, ProjectUID: p.UID, AuthorityUID: authority, SenderInstanceUID: authority, ReceiverInstanceUID: source, Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{authority}, SourceUID: source, SourceHash: hash, Body: []byte("{}")})
			require.NoError(t, err)
			raw, err := json.Marshal(envelope)
			require.NoError(t, err)
			switch kind {
			case "attribution":
				public := make([]byte, 32)
				_, err = d.ExecContext(t.Context(), `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key) VALUES(?,?,?,?)`, p.UID, authority, db.RootPublicKeyID(public), base64.StdEncoding.EncodeToString(public))
			case "outbox":
				_, err = d.ExecContext(t.Context(), `INSERT INTO federation_relay_outbox(id,project_uid,binding_uid,stream,reset_epoch,source_uid,source_hash,envelope) VALUES(1,?,?,?,?,?,?,?)`, p.UID, binding, db.RelayStreamEvent, 1, source, hash, string(raw))
			case "inbox":
				_, err = d.ExecContext(t.Context(), `INSERT INTO federation_relay_inbox(project_uid,binding_uid,stream,reset_epoch,sequence,source_uid,source_hash,envelope_digest,envelope) VALUES(?,?,?,?,1,?,?,?,?)`, p.UID, binding, db.RelayStreamEvent, 1, source, hash, envelope.Digest, string(raw))
			case "cursor":
				_, err = d.ExecContext(t.Context(), `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch) VALUES(?,?,?,1)`, p.UID, binding, db.RelayStreamEvent)
			}
			require.NoError(t, err)
			d.SetMaxOpenConns(1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var seq iter.Seq2[db.ImportRecord, error]
			if kind == "attribution" {
				seq = d.ExportAttribution(ctx, db.ExportFilter{})
			} else {
				seq = d.ExportRelayState(ctx)
			}
			count := 0
			for _, err := range seq {
				require.NoError(t, err)
				count++
				cancel()
				// Acquiring the sole connection waits for database/sql to close the
				// canceled rows and record their context error before the consumer stops.
				waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
				conn, err := d.Conn(waitCtx)
				stop()
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				break
			}
			require.Equal(t, 1, count)
		})
	}
}
