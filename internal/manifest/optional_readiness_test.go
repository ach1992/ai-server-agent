package manifest

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestOptionalReadinessBoundedConcurrentFreshProbes(t *testing.T) {
	var inFlight, peak atomic.Int32
	var calls [optionalProbeCount]atomic.Int32
	var checks [optionalProbeCount]func() bool
	for i := range checks {
		i := i
		checks[i] = func() bool {
			calls[i].Add(1)
			n := inFlight.Add(1)
			for {
				max := peak.Load()
				if n <= max || peak.CompareAndSwap(max, n) {
					break
				}
			}
			time.Sleep(12 * time.Millisecond)
			inFlight.Add(-1)
			return i%2 == 0
		}
	}
	for invocation := 1; invocation <= 2; invocation++ {
		values := runOptionalProbes(checks)
		for i, actual := range values {
			if actual != (i%2 == 0) || calls[i].Load() != int32(invocation) {
				t.Fatalf("probe %d invocation %d: value=%t calls=%d", i, invocation, actual, calls[i].Load())
			}
		}
		if inFlight.Load() != 0 {
			t.Fatal("probe still in flight after return")
		}
	}
	if peak.Load() != optionalProbeParallelism {
		t.Fatalf("parallelism observed %d, want exactly %d", peak.Load(), optionalProbeParallelism)
	}
}

// A probe that changes outcome must be visible on the very next invocation;
// slow verification is not a reason to report stale installed=true.
func TestOptionalReadinessNeverCachesAdapterTruth(t *testing.T) {
	var phase atomic.Bool
	var calls atomic.Int32
	var checks [optionalProbeCount]func() bool
	for i := range checks {
		checks[i] = func() bool { calls.Add(1); return phase.Load() }
	}
	first := runOptionalProbes(checks)
	phase.Store(true)
	second := runOptionalProbes(checks)
	for i := range first {
		if first[i] || !second[i] {
			t.Fatalf("stale optional probe %d first=%t second=%t", i, first[i], second[i])
		}
	}
	if got := calls.Load(); got != 2*optionalProbeCount {
		t.Fatalf("executed %d checks, want fresh checks both invocations", got)
	}
}
