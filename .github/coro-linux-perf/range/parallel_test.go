package probe

import (
	"runtime"
	"sync/atomic"
	"testing"
)

var (
	parallelYieldDone     uint64
	parallelYieldChecksum uint64
	parallelYieldSeed     uint64
	parallelYieldWorkSize int
)

func parallelWork(iterations int, value uint64) uint64 {
	for i := 0; i < iterations; i++ {
		value = value*6364136223846793005 + 1442695040888963407
	}
	return value
}

func parallelYieldWorker() {
	seed := atomic.AddUint64(&parallelYieldSeed, 1)
	value := parallelWork(parallelYieldWorkSize, seed)
	runtime.Gosched()
	value = parallelWork(parallelYieldWorkSize, value)
	atomic.AddUint64(&parallelYieldChecksum, value)
	atomic.AddUint64(&parallelYieldDone, 1)
}

func parallelYieldWork(rounds, tasks, work int) uint64 {
	if rounds <= 0 || tasks <= 0 || work <= 0 {
		return 0
	}
	var total uint64
	for round := 0; round < rounds; round++ {
		atomic.StoreUint64(&parallelYieldDone, 0)
		atomic.StoreUint64(&parallelYieldChecksum, 0)
		atomic.StoreUint64(&parallelYieldSeed, uint64(round*tasks))
		parallelYieldWorkSize = work
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

func benchYield(b *testing.B, tasks, work int) {
	b.ReportAllocs()
	got := parallelYieldWork(b.N, tasks, work)
	b.StopTimer()
	n := uint64(b.N * tasks)
	sum := n * (n + 1) / 2
	offset := parallelWork(work*2, 0)
	factor := parallelWork(work*2, 1) - offset
	want := factor*sum + offset*n
	if got != want {
		b.Fatalf("checksum %d, want %d", got, want)
	}
}

func BenchmarkB32W4096(b *testing.B) { benchYield(b, 32, 4096) }

func BenchmarkB64W4096(b *testing.B) { benchYield(b, 64, 4096) }

func BenchmarkB128W4096(b *testing.B) { benchYield(b, 128, 4096) }

func BenchmarkB64W256(b *testing.B) { benchYield(b, 64, 256) }

func BenchmarkB64W16384(b *testing.B) { benchYield(b, 64, 16384) }

func BenchmarkB16W4096(b *testing.B) { benchYield(b, 16, 4096) }
