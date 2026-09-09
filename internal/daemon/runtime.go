package daemon

import kitdaemon "go.kenn.io/kit/daemon"

// RuntimeRestartMetadataKey marks a runtime record whose daemon re-executes
// itself, with its own arguments and environment, when it receives the
// restart signal. Records without it belong to daemons that need a manual
// restart after an update.
const RuntimeRestartMetadataKey = "restart_signal"

// RuntimeProcessAlive reports whether a runtime record still identifies the
// process that created it. Records without a verifiable identity retain the
// legacy PID-only behavior; a definite identity mismatch is stale.
func RuntimeProcessAlive(record kitdaemon.RuntimeRecord) bool {
	if !kitdaemon.ProcessAlive(record.PID) {
		return false
	}
	return kitdaemon.CompareRuntimeProcessIdentity(record) != kitdaemon.ProcessIdentityMismatch
}

// RuntimeRecordRestartable reports whether the record's daemon handles the
// restart signal sent by SignalDaemonRestart.
func RuntimeRecordRestartable(record kitdaemon.RuntimeRecord) bool {
	return record.Metadata[RuntimeRestartMetadataKey] == "1"
}
