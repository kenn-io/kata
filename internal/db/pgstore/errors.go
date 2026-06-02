package pgstore

import "errors"

// ErrNotImplementedPhase3 is returned by pgstore domain-method stubs until
// each entity group's real Postgres queries land. Any accidental production
// call into an unimplemented method fails loudly.
var ErrNotImplementedPhase3 = errors.New("pgstore: not implemented in Phase 3 — see Phase 4 for queries")
