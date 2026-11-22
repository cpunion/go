package probe

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// Keep both phases in one scheduler so the test can observe changes in load.
func mixedYieldWork(rounds, tasks, split, first, second int) uint64 {
	parallelYieldWorkSize = first
	var total uint64
	for round := 0; round < rounds; round++ {
		if round == split {
			parallelYieldWorkSize = second
		}
		atomic.StoreUint64(&parallelYieldDone, 0)
		atomic.StoreUint64(&parallelYieldChecksum, 0)
		atomic.StoreUint64(&parallelYieldSeed, uint64(round*tasks))
		for range tasks {
			go parallelYieldWorker()
		}
		for atomic.LoadUint64(&parallelYieldDone) < uint64(tasks) {
			runtime.Gosched()
		}
		total += atomic.LoadUint64(&parallelYieldChecksum)
	}
	return total
}

func mixedChecksum(begin, end uint64, work int) uint64 {
	count := end - begin
	seedTotal := end*(end+1)/2 - begin*(begin+1)/2
	offset := parallelWork(work*2, 0)
	factor := parallelWork(work*2, 1) - offset
	return factor*seedTotal + offset*count
}

func benchMixed(b *testing.B, tasks, first, second int) {
	b.ReportAllocs()
	split := b.N / 4
	got := mixedYieldWork(b.N, tasks, split, first, second)
	b.StopTimer()
	mid, end := uint64(split*tasks), uint64(b.N*tasks)
	want := mixedChecksum(0, mid, first) + mixedChecksum(mid, end, second)
	if got != want {
		b.Fatalf("checksum %d, want %d", got, want)
	}
}

func BenchmarkShortThenLong64(b *testing.B)  { benchMixed(b, 64, 256, 4096) }
func BenchmarkLongThenShort64(b *testing.B)  { benchMixed(b, 64, 4096, 256) }
func BenchmarkShortThenLong128(b *testing.B) { benchMixed(b, 128, 256, 4096) }
