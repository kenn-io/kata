//go:build !linux

package daemon

import "runtime"

func federationPollCPUs() int {
	return runtime.GOMAXPROCS(0)
}
