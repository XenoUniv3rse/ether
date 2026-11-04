//go:build !linux && !darwin && !windows

package wasm

import "time"

// threadCPU is unavailable here: budgets fall back to the monotonic clock.
func threadCPU() (time.Duration, bool) { return 0, false }
