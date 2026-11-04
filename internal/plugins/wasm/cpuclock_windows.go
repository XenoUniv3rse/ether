//go:build windows

package wasm

import (
	"math"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Windows reports a thread's CPU time precisely only in cycles
// (QueryThreadCycleTime); GetThreadTimes advances once per scheduler tick
// (about 15.6 ms), far coarser than the 2 ms candidate budget. The cycle
// count is read from the invariant time-stamp counter, so it converts to
// time at a constant rate, measured once against the monotonic clock.
var (
	procQueryThreadCycleTime = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryThreadCycleTime")
	cycleRateOnce            sync.Once
	cyclesPerNs              float64
)

// currentThread is GetCurrentThread's pseudo-handle.
const currentThread = ^uintptr(1)

func threadCycles() (uint64, bool) {
	var n uint64
	if r, _, _ := procQueryThreadCycleTime.Call(currentThread, uintptr(unsafe.Pointer(&n))); r == 0 {
		return 0, false
	}
	return n, true
}

// measureCycleRate times a few short spins. A spin the scheduler interrupts
// counts fewer cycles for its wall time, so the highest rate is the
// counter's own.
func measureCycleRate() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if procQueryThreadCycleTime.Find() != nil {
		return
	}
	for range 5 {
		c0, ok := threadCycles()
		if !ok {
			return
		}
		t0 := time.Now()
		for time.Since(t0) < time.Millisecond {
		}
		c1, _ := threadCycles()
		if r := float64(c1-c0) / float64(time.Since(t0)); r > cyclesPerNs {
			cyclesPerNs = r
		}
	}
}

// threadCPU reports the CPU time the calling OS thread has used. Callers
// lock the goroutine to its thread first (runtime.LockOSThread).
func threadCPU() (time.Duration, bool) {
	cycleRateOnce.Do(measureCycleRate)
	if cyclesPerNs <= 0 || math.IsInf(cyclesPerNs, 0) {
		return 0, false
	}
	n, ok := threadCycles()
	if !ok {
		return 0, false
	}
	return time.Duration(float64(n) / cyclesPerNs), true
}
