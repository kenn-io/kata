package sqlitestore

import (
	"database/sql/driver"

	"modernc.org/sqlite"

	"go.kenn.io/kata/internal/db"
)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("kata_occurrence_key", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		occurrence, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		key := db.RecurringOccurrenceKey(occurrence)
		if key == "" {
			return nil, nil
		}
		return key, nil
	})
}
