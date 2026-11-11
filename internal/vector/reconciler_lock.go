package vector

// The semantic-search reconciler is elected with a PostgreSQL advisory lock.
// The key has to be spelled twice: pg_try_advisory_lock takes two int4
// arguments, while pg_catalog.pg_locks reports the same pair as classid/objid
// bigints holding the unsigned 32-bit truncation of those values. Both
// spellings are built from reconcilerLockName here, so a change to the key
// cannot land in one encoding and not the other.

// reconcilerLockName is the advisory-lock key prefix. current_schema() is
// appended so two kata schemas in one database elect independent reconcilers.
const reconcilerLockName = "kata:vector:reconciler:"

// reconcilerLockArgs is the (classid, objid) argument pair passed to
// pg_try_advisory_lock and pg_advisory_unlock.
const reconcilerLockArgs = `hashtext(current_database()), ` +
	`hashtext('` + reconcilerLockName + `' || current_schema())`

// ReconcilerLockPredicateSQL matches the reconciler's advisory lock in
// pg_catalog.pg_locks. It is exported so tests outside this package that
// observe the lock (internal/daemon's reconciler recovery tests) share this
// one definition of the key instead of re-deriving the truncation.
const ReconcilerLockPredicateSQL = `locktype = 'advisory'
	  AND granted
	  AND classid = (hashtext(current_database())::bigint & 4294967295)
	  AND objid = (hashtext('` + reconcilerLockName + `' || current_schema())::bigint & 4294967295)`

const reconcilerLockSQL = `SELECT pg_try_advisory_lock(` + reconcilerLockArgs + `)`

const reconcilerUnlockSQL = `SELECT pg_advisory_unlock(` + reconcilerLockArgs + `)`

const reconcilerLeaseHeldSQL = `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_locks
		WHERE pid = pg_backend_pid()
		  AND ` + ReconcilerLockPredicateSQL + `)`
