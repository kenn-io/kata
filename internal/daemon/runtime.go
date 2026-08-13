package daemon

import kitdaemon "go.kenn.io/kit/daemon"

// RuntimeProcessAlive reports whether a runtime record still identifies the
// process that created it. Records without a verifiable identity retain the
// legacy PID-only behavior; a definite identity mismatch is stale.
func RuntimeProcessAlive(record kitdaemon.RuntimeRecord) bool {
	if !kitdaemon.ProcessAlive(record.PID) {
		return false
	}
	return kitdaemon.CompareRuntimeProcessIdentity(record) != kitdaemon.ProcessIdentityMismatch
}
