// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
)

func TestMixedTaskProbes(t *testing.T) {
	for _, tasks := range []int{0, 1, 3, 17} {
		for _, offset := range []int{0, 1} {
			if got := mixedTaskParkBursts(2, tasks, offset); got != uint64(2*tasks) {
				t.Fatalf("tasks=%d offset=%d: completed %d tasks, want %d", tasks, offset, got, 2*tasks)
			}
		}
	}
}

func BenchmarkTaskBurst1000(b *testing.B) {
	b.ReportAllocs()
	if got := taskBursts(b.N, 1000); got != uint64(1000*b.N) {
		b.Fatalf("completed %d tasks, want %d", got, 1000*b.N)
	}
}

func BenchmarkTaskBurst100000(b *testing.B) {
	b.ReportAllocs()
	if got := taskBursts(b.N, 100000); got != uint64(100000*b.N) {
		b.Fatalf("completed %d tasks, want %d", got, 100000*b.N)
	}
}

func BenchmarkTaskPark1000(b *testing.B) {
	b.ReportAllocs()
	if got := taskParkBursts(b.N, 1000); got != uint64(1000*b.N) {
		b.Fatalf("completed %d tasks, want %d", got, 1000*b.N)
	}
}

func BenchmarkTaskPark100000(b *testing.B) {
	b.ReportAllocs()
	if got := taskParkBursts(b.N, 100000); got != uint64(100000*b.N) {
		b.Fatalf("completed %d tasks, want %d", got, 100000*b.N)
	}
}

func BenchmarkMixedParkScalarFirst100000(b *testing.B) {
	b.ReportAllocs()
	if got := mixedTaskParkBursts(b.N, 100000, 0); got != uint64(100000*b.N) {
		b.Fatalf("completed %d tasks, want %d", got, 100000*b.N)
	}
}

func BenchmarkMixedParkPointerFirst100000(b *testing.B) {
	b.ReportAllocs()
	if got := mixedTaskParkBursts(b.N, 100000, 1); got != uint64(100000*b.N) {
		b.Fatalf("completed %d tasks, want %d", got, 100000*b.N)
	}
}

func BenchmarkLiveTaskFootprint1(b *testing.B)      { benchmarkLiveTaskFootprint(b, 1) }
func BenchmarkLiveTaskFootprint1000(b *testing.B)   { benchmarkLiveTaskFootprint(b, 1000) }
func BenchmarkLiveTaskFootprint100000(b *testing.B) { benchmarkLiveTaskFootprint(b, 100000) }

// benchmarkLiveTaskFootprint uses the existing compiler-generated workload.
// Every worker has reached its readiness checkpoint and cannot finish before
// release. Readiness is published before the channel receive, so this does not
// assert that every worker has already installed its channel waiter. Samples
// include the coordinator and root/executor setup. ThreadCreateProfile counts
// creation records, not a census of live OS threads; StackInuse counts Go stacks,
// not the complete native-thread stack or process RSS.
func benchmarkLiveTaskFootprint(b *testing.B, tasks int) {
	if b.N != 1 {
		b.Skip("requires -benchtime=1x")
	}
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	beforeThreads, _ := runtime.ThreadCreateProfile(nil)
	beforeGoroutines := runtime.NumGoroutine()
	var before, live runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	atomic.StoreUint64(&taskParkReady, 0)
	atomic.StoreUint64(&taskParkRelease, 0)
	done := make(chan uint64, 1)
	go func() { done <- taskParkUntilReleased(tasks) }()
	for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
		runtime.Gosched()
	}
	runtime.Gosched()
	liveThreads, _ := runtime.ThreadCreateProfile(nil)
	liveGoroutines := runtime.NumGoroutine()
	runtime.ReadMemStats(&live)
	atomic.StoreUint64(&taskParkRelease, 1)
	completed := <-done
	b.StopTimer()
	if completed != uint64(tasks) {
		b.Fatalf("completed %d tasks, want %d", completed, tasks)
	}
	b.ReportMetric(float64(int64(live.HeapAlloc)-int64(before.HeapAlloc))/float64(tasks), "live-heap-B/task")
	b.ReportMetric(float64(int64(live.HeapObjects)-int64(before.HeapObjects))/float64(tasks), "live-objects/task")
	b.ReportMetric(float64(int64(live.StackInuse)-int64(before.StackInuse))/float64(tasks), "live-Go-stack-B/task")
	b.ReportMetric(float64(liveGoroutines-beforeGoroutines), "Go-goroutine-delta")
	b.ReportMetric(float64(liveGoroutines), "Go-goroutines")
	b.ReportMetric(float64(liveThreads-beforeThreads), "thread-record-delta")
	b.ReportMetric(float64(liveThreads), "thread-records")
	b.ReportMetric(float64(tasks), "tasks")
}

func benchmarkMixedTaskFootprint(b *testing.B, tasks, offset int) {
	if b.N != 1 {
		b.Skip("requires -benchtime=1x")
	}
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	beforeThreads, _ := runtime.ThreadCreateProfile(nil)
	beforeGoroutines := runtime.NumGoroutine()
	var before, live runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	atomic.StoreUint64(&taskParkReady, 0)
	atomic.StoreUint64(&taskParkRelease, 0)
	done := make(chan uint64, 1)
	go func() { done <- mixedTaskParkUntilReleased(tasks, offset) }()
	for atomic.LoadUint64(&taskParkReady) < uint64(tasks) {
		runtime.Gosched()
	}
	runtime.Gosched()
	liveThreads, _ := runtime.ThreadCreateProfile(nil)
	liveGoroutines := runtime.NumGoroutine()
	runtime.ReadMemStats(&live)
	atomic.StoreUint64(&taskParkRelease, 1)
	completed := <-done
	b.StopTimer()
	if completed != uint64(tasks) {
		b.Fatalf("completed %d tasks, want %d", completed, tasks)
	}
	b.ReportMetric(float64(int64(live.HeapAlloc)-int64(before.HeapAlloc))/float64(tasks), "live-heap-B/task")
	b.ReportMetric(float64(int64(live.HeapObjects)-int64(before.HeapObjects))/float64(tasks), "live-objects/task")
	b.ReportMetric(float64(int64(live.StackInuse)-int64(before.StackInuse))/float64(tasks), "live-Go-stack-B/task")
	b.ReportMetric(float64(liveGoroutines-beforeGoroutines), "Go-goroutine-delta")
	b.ReportMetric(float64(liveGoroutines), "Go-goroutines")
	b.ReportMetric(float64(liveThreads-beforeThreads), "thread-record-delta")
	b.ReportMetric(float64(liveThreads), "thread-records")
	b.ReportMetric(float64(tasks), "tasks")
}

func BenchmarkLiveTaskFootprint256(b *testing.B) { benchmarkLiveTaskFootprint(b, 256) }
func BenchmarkLiveTaskFootprint264(b *testing.B) { benchmarkLiveTaskFootprint(b, 264) }
func BenchmarkLiveTaskFootprint265(b *testing.B) { benchmarkLiveTaskFootprint(b, 265) }
func BenchmarkLiveTaskFootprint266(b *testing.B) { benchmarkLiveTaskFootprint(b, 266) }
func BenchmarkLiveTaskFootprint267(b *testing.B) { benchmarkLiveTaskFootprint(b, 267) }
func BenchmarkLiveTaskFootprint268(b *testing.B) { benchmarkLiveTaskFootprint(b, 268) }

func BenchmarkMixedScalarFirst1000(b *testing.B) {
	benchmarkMixedTaskFootprint(b, 1000, 0)
}
func BenchmarkMixedScalarFirst100000(b *testing.B) {
	benchmarkMixedTaskFootprint(b, 100000, 0)
}
func BenchmarkMixedPointerFirst1000(b *testing.B) {
	benchmarkMixedTaskFootprint(b, 1000, 1)
}
func BenchmarkMixedPointerFirst100000(b *testing.B) {
	benchmarkMixedTaskFootprint(b, 100000, 1)
}
