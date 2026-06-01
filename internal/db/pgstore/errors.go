package pgstore

import "errors"

// ErrNotImplementedPhase3 is returned by every pgstore domain method during
// Phase 3. Phase 4 replaces each entity group's stubs with real queries;
// until then any accidental call from a non-Migrate code path fails loudly.
var ErrNotImplementedPhase3 = errors.New("pgstore: not implemented in Phase 3 — see Phase 4 for queries")
