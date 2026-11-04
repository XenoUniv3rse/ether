//go:build linux || darwin

package wasm

import (
	"time"

	"golang.org/x/sys/unix"
)

// threadCPU reports the CPU time the calling OS thread has used. Callers
// lock the goroutine to its thread first (runtime.LockOSThread).
func threadCPU() (time.Duration, bool) {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts) != nil {
		return 0, false
	}
	return time.Duration(ts.Nano()), true
}
