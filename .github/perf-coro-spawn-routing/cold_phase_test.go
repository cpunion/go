//go:build corocoldphase

package corobench

import (
	"testing"
	_ "unsafe"
)

//go:linkname dumpColdPhases runtime.coroColdDump
func dumpColdPhases()

func BenchmarkColdPhase16(b *testing.B) {
	if parallelYieldWork(b.N, 16, 4096) == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
	b.StopTimer()
	dumpColdPhases()
}

func BenchmarkColdPhaseLight64(b *testing.B) {
	if parallelYieldWork(b.N, 64, 512) == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
	b.StopTimer()
	dumpColdPhases()
}
