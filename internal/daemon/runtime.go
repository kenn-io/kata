package daemon

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

// RuntimeProcessAlive reports whether a runtime record still identifies the
// process that created it. For legacy records without an identity, a process
// created after the record was published proves that the PID has been reused.
func RuntimeProcessAlive(record kitdaemon.RuntimeRecord) bool {
	if record.PID <= 0 || record.PID > math.MaxInt32 || !kitdaemon.ProcessAlive(record.PID) {
		return false
	}
	switch kitdaemon.CompareRuntimeProcessIdentity(record) {
	case kitdaemon.ProcessIdentityMatch:
		return true
	case kitdaemon.ProcessIdentityMismatch:
		return false
	}
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

// RuntimeRecordRestartable reports whether the record's daemon handles the
// restart signal sent by SignalDaemonRestart.
func RuntimeRecordRestartable(record kitdaemon.RuntimeRecord) bool {
	return record.Metadata[RuntimeRestartMetadataKey] == "1"
}
