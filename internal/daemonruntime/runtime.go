package daemonruntime

import (
	"math"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	kitdaemon "go.kenn.io/kit/daemon"
)

// RuntimeRestartMetadataKey marks a runtime record whose daemon re-executes
// itself, with its own arguments and environment, when it receives the
// restart signal. Records without it belong to daemons that need a manual
// restart after an update.
const RuntimeRestartMetadataKey = "restart_signal"

// RuntimeProcessAlive reports whether a runtime record still identifies a local
// process that created it. For legacy records without an identity, a process
// created after the record was published proves that the PID has been reused.
func RuntimeProcessAlive(record kitdaemon.RuntimeRecord) bool {
	if kitdaemon.RuntimeProcessScope(record) != kitdaemon.ProcessScopeLocal {
		return false
	}
	if record.PID <= 0 || record.PID > math.MaxInt32 || !kitdaemon.ProcessAlive(record.PID) {
		return false
	}
	switch kitdaemon.CompareRuntimeProcessIdentity(record) {
	case kitdaemon.ProcessIdentityMatch:
		return true
	case kitdaemon.ProcessIdentityMismatch:
		return false
	}
	return runtimeCreationTimeMatches(record)
}

func runtimeCreationTimeMatches(record kitdaemon.RuntimeRecord) bool {
	if record.StartedAt.IsZero() {
		return true
	}
	proc, err := process.NewProcess(int32(record.PID))
	if err != nil {
		return true
	}
	created, err := proc.CreateTime()
	if err != nil || created <= 0 {
		return true
	}
	return !time.UnixMilli(created).After(record.StartedAt)
}

// RuntimeRecordWorthProbing reports whether discovery should ping a record's
// endpoint. Beyond RuntimeProcessAlive, it keeps a record whose identity this
// process cannot judge when its PID is live and was not reused after the record
// was written. Startup refuses beside such a record, so discovery must still
// find the daemon instead of auto-starting one that will refuse. Discovery
// accepts it only if the endpoint reports the record's PID. Daemon controls
// keep using RuntimeProcessAlive and never signal such a record.
func RuntimeRecordWorthProbing(record kitdaemon.RuntimeRecord) bool {
	if kitdaemon.RuntimeProcessScope(record) != kitdaemon.ProcessScopeUnknown {
		return RuntimeProcessAlive(record)
	}
	return record.PID > 0 && record.PID <= math.MaxInt32 && kitdaemon.ProcessAlive(record.PID) &&
		runtimeCreationTimeMatches(record)
}

// RuntimeRecordBlocksStart keeps kit's startup rule and the legacy PID-reuse check.
func RuntimeRecordBlocksStart(record kitdaemon.RuntimeRecord) bool {
	if !kitdaemon.RuntimeRecordBlocksStart(record) {
		return false
	}
	if kitdaemon.RuntimeProcessScope(record) == kitdaemon.ProcessScopeLocal &&
		kitdaemon.CompareRuntimeProcessIdentity(record) == kitdaemon.ProcessIdentityUnknown {
		return record.PID <= math.MaxInt32 && runtimeCreationTimeMatches(record)
	}
	return true
}

// RuntimeRecordRestartable reports whether the record's daemon handles the
// restart signal sent by SignalDaemonRestart.
func RuntimeRecordRestartable(record kitdaemon.RuntimeRecord) bool {
	return record.Metadata[RuntimeRestartMetadataKey] == "1"
}
