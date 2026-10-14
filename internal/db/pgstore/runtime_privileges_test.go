package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimePrivilegeValidationCoversFederationObjects(t *testing.T) {
	ctx := context.Background()
	t.Run("provenance table", func(t *testing.T) {
		db := openPrivilegeCheckDB(t, [][]driver.Value{{
			"federation_entity_provenance", false, true, true, true,
		}}, nil)
		store := &Store{DB: db, schema: "runtime_privilege_check"}
		err := store.validateRuntimeTablePrivileges(ctx)
		require.ErrorContains(t, err, `postgres runtime role lacks SELECT privilege on table "runtime_privilege_check.federation_entity_provenance"`)
	})

	t.Run("relay outbox sequence", func(t *testing.T) {
		sequences := make([][]driver.Value, 0, len(canonicalSequenceNames)+1)
		for _, name := range canonicalSequenceNames {
			sequences = append(sequences, []driver.Value{name, true, true, true})
		}
		sequences = append(sequences, []driver.Value{"federation_relay_outbox_id_seq", false, true, true})
		db := openPrivilegeCheckDB(t, nil, sequences)
		store := &Store{DB: db, schema: "runtime_privilege_check"}
		err := store.validateRuntimeSequencePrivileges(ctx)
		require.ErrorContains(t, err, `postgres runtime role lacks USAGE privilege on sequence "runtime_privilege_check.federation_relay_outbox_id_seq"`)
	})
}

func openPrivilegeCheckDB(t *testing.T, tables, sequences [][]driver.Value) *sql.DB {
	t.Helper()
	db := sql.OpenDB(privilegeCheckConnector{tables: tables, sequences: sequences})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

type privilegeCheckConnector struct {
	tables    [][]driver.Value
	sequences [][]driver.Value
}

func (c privilegeCheckConnector) Connect(context.Context) (driver.Conn, error) {
	return privilegeCheckConn{connector: c}, nil
}

func (privilegeCheckConnector) Driver() driver.Driver { return privilegeCheckDriver{} }

type privilegeCheckDriver struct{}

func (privilegeCheckDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("privilege check driver requires a connector")
}

type privilegeCheckConn struct {
	connector privilegeCheckConnector
}

func (privilegeCheckConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are unsupported")
}

func (privilegeCheckConn) Close() error { return nil }

func (privilegeCheckConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are unsupported")
}

func (c privilegeCheckConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "has_table_privilege"):
		return &privilegeCheckRows{columns: []string{"relname", "select", "insert", "update", "delete"}, values: c.connector.tables}, nil
	case strings.Contains(query, "has_sequence_privilege"):
		return &privilegeCheckRows{columns: []string{"relname", "usage", "select", "update"}, values: c.connector.sequences}, nil
	default:
		return nil, errors.New("unexpected privilege query")
	}
}

type privilegeCheckRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *privilegeCheckRows) Columns() []string { return r.columns }

func (r *privilegeCheckRows) Close() error { return nil }

func (r *privilegeCheckRows) Next(dest []driver.Value) error {
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}
