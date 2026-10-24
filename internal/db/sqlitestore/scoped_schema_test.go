package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// The persisted contract has twenty separate v2 objects. Removing any object
// breaks owner backup or leaves a custody state without native storage.
func TestScopedSchemaUpgradeAndFreshParity(t *testing.T) {
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	for _, table := range []string{
		"federation_scoped_views", "federation_scoped_generations", "federation_scoped_projects",
		"federation_scoped_records", "federation_scoped_batches", "federation_scoped_work",
		"federation_scoped_delivery", "federation_scoped_cursors", "federation_source_operations",
		"federation_private_sources", "federation_source_custody", "federation_source_dependencies",
		"federation_source_coverage", "federation_control_custody", "federation_mutation_expectations",
		"federation_predicate_incarnations", "federation_canonical_close_state", "federation_protocol_cutovers",
		"federation_scoped_conversions", "federation_conversion_obligations",
	} {
		t.Run(table, func(t *testing.T) {
			rows, err := s.QueryContext(t.Context(), `SELECT * FROM `+table+` LIMIT 0`)
			require.NoError(t, err)
			require.NoError(t, rows.Close())
		})
	}
	// Custody must not depend on a live project or grant row. The complete stream
	// identity permits identical sequence numbers in the two directions.
	fixture := dbtest.ScopedOwnerBackupFixture(t)
	var source *db.ScopedSourceOperationExport
	for _, record := range fixture.Records {
		if row, ok := record.(*db.ScopedSourceOperationExport); ok {
			source = row
		}
	}
	require.NotNil(t, source)
	for _, direction := range []db.ScopedDeliveryDirection{db.ScopedDeliveryInbound, db.ScopedDeliveryOutbound} {
		sender, receiver := fixture.View.AuthorityUID, fixture.View.DestinationUID
		if direction == db.ScopedDeliveryOutbound {
			sender, receiver = receiver, sender
		}
		header := db.ScopedDeliveryHeader{Version: 2, Domain: db.ScopedDeliveryDomain, BindingUID: fixture.Principal.BindingUID, Epoch: fixture.Principal.Epoch, Channel: db.ScopedDeliverySource, Sequence: 1, ObjectUID: source.OperationUID, ObjectDigest: source.Digest, AuthorityUID: fixture.View.AuthorityUID, SenderInstanceUID: sender, ReceiverInstanceUID: receiver, Path: []string{sender}}
		wire, err := db.EncodeScopedDelivery(header, source.ManifestBytes)
		require.NoError(t, err)
		headerBytes, err := db.ScopedDeliveryHeaderBytes(wire)
		require.NoError(t, err)
		row := db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: db.ScopedDeliveryStream{BindingUID: header.BindingUID, Epoch: header.Epoch, Direction: direction, Channel: header.Channel}, Sequence: header.Sequence}, ObjectUID: header.ObjectUID, ObjectDigest: header.ObjectDigest, OfferedDigest: db.ScopedOfferedDigest(wire), HeaderBytes: headerBytes, EnvelopeBytes: wire, Accepted: direction == db.ScopedDeliveryInbound, Emitted: direction == db.ScopedDeliveryOutbound}
		require.NoError(t, row.Validate(), "the index fixture satisfies the production delivery row contract")
		_, err = s.ExecContext(t.Context(), `INSERT INTO federation_scoped_delivery
   (binding_uid,epoch,direction,channel,sequence,object_uid,object_digest,offered_digest,header_bytes,envelope_bytes,accepted,emitted,acknowledged)
   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.BindingUID, row.Epoch, row.Direction, row.Channel, row.Sequence, row.ObjectUID, row.ObjectDigest, row.OfferedDigest, row.HeaderBytes, row.EnvelopeBytes, row.Accepted, row.Emitted, row.Acknowledged)
		require.NoError(t, err)
	}
}
